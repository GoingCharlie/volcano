#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG_FILE="${CONFIG_FILE:-${TEST_DIR}/config.env}"

if [[ ! -f "${CONFIG_FILE}" ]]; then
  echo "ERROR: config file not found: ${CONFIG_FILE}" >&2
  echo "Copy ${TEST_DIR}/config.env.example to ${TEST_DIR}/config.env and edit it first." >&2
  exit 1
fi

# shellcheck source=/dev/null
source "${CONFIG_FILE}"

: "${EXPECTED_CONTEXT:=}"
: "${TEST_NODES:=}"
: "${ACCELERATOR_RESOURCE:=huawei.com/asend-1980}"
: "${CARDS_PER_NODE:=8}"
: "${RECEIVER_CARDS:=4}"
: "${MOVING_CARDS:=2}"
: "${TEST_NAMESPACE:=repack-real-test}"
: "${TEST_ID:=repack-real-01}"
: "${TEST_IMAGE:=busybox:1.36.1}"
: "${VOLCANO_SCHEDULER_NAME:=volcano}"
: "${TOLERATIONS_JSON:=[]}"
: "${TIMEOUT_SECONDS:=1200}"
: "${CACHE_SETTLE_SECONDS:=10}"
: "${EXPECTED_IMAGE_TAG:=}"
: "${ALLOW_EXISTING_ACCELERATOR_PODS:=false}"

MANAGED_LABEL="repack-test.volcano.sh/id"
MANAGED_ANNOTATION="repack-test.volcano.sh/managed"
HOLD_GATE="repack-test.volcano.sh/hold"
RECEIVER_STS="repack-receiver"
MOVING_STS="repack-moving"
BLOCKER_POD="repack-new-user-job"
HEADLESS_SERVICE="repack-fixture"

TEST_NODES_NORMALIZED="${TEST_NODES//,/ }"
read -r -a NODES <<< "${TEST_NODES_NORMALIZED}"

log() {
  printf '[repack-real-test] %s\n' "$*" >&2
}

die() {
  printf '[repack-real-test] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

nodes_json() {
  printf '%s\n' "${NODES[@]}" | jq -R . | jq -s .
}

run_name() {
  local suffix="$1"
  printf '%s-%s' "${TEST_ID}" "${suffix}"
}

selector() {
  printf '%s=%s' "${MANAGED_LABEL}" "${TEST_ID}"
}

assert_config() {
  [[ -n "${EXPECTED_CONTEXT:-}" ]] || die "EXPECTED_CONTEXT is required"
  [[ -n "${TEST_NODES:-}" ]] || die "TEST_NODES is required"
  [[ "${#NODES[@]}" -eq 8 ]] || die "TEST_NODES must contain exactly 8 nodes; got ${#NODES[@]}"
  [[ "$(printf '%s\n' "${NODES[@]}" | sort -u | wc -l | tr -d ' ')" -eq 8 ]] || die "TEST_NODES must contain 8 distinct node names"
  [[ "${TEST_ID}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "TEST_ID must be a lowercase DNS/label value"
  [[ "${#TEST_ID}" -le 40 ]] || die "TEST_ID must be <= 40 characters"
  [[ "${TEST_NAMESPACE}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "TEST_NAMESPACE is not a valid DNS label"
  [[ "${CARDS_PER_NODE}" =~ ^[1-9][0-9]*$ ]] || die "CARDS_PER_NODE must be a positive integer"
  [[ "${RECEIVER_CARDS}" =~ ^[1-9][0-9]*$ ]] || die "RECEIVER_CARDS must be a positive integer"
  [[ "${MOVING_CARDS}" =~ ^[1-9][0-9]*$ ]] || die "MOVING_CARDS must be a positive integer"
  [[ "${CACHE_SETTLE_SECONDS}" =~ ^[0-9]+$ ]] || die "CACHE_SETTLE_SECONDS must be a non-negative integer"
  (( RECEIVER_CARDS + MOVING_CARDS <= CARDS_PER_NODE )) || die "receiver + moving cards must fit on one node"
  jq -e 'type == "array"' <<<"${TOLERATIONS_JSON}" >/dev/null || die "TOLERATIONS_JSON must be a JSON array"
}

repack_engine_json() {
  kubectl get deployments.apps -A -l app=volcano-repack-engine -o json
}

preflight() {
  require_command kubectl
  require_command jq
  assert_config

  local current_context
  current_context="$(kubectl config current-context)"
  [[ "${current_context}" == "${EXPECTED_CONTEXT}" ]] || die "kubectl context is ${current_context}, expected ${EXPECTED_CONTEXT}"
  log "kubectl context confirmed: ${current_context}"

  kubectl get --raw='/readyz' >/dev/null || die "apiserver /readyz failed"
  kubectl explain pod.spec.schedulingGates >/dev/null 2>&1 || die "cluster does not expose Pod schedulingGates"
  kubectl get crd repackruns.repack.volcano.sh >/dev/null || die "RepackRun CRD is missing"
  kubectl get crd repackpolicies.repack.volcano.sh >/dev/null || die "RepackPolicy CRD is missing"
  kubectl get crd podgroups.scheduling.volcano.sh >/dev/null || die "PodGroup CRD is missing"

  kubectl get crd repackruns.repack.volcano.sh -o json | jq -e '
    [.spec.versions[].schema.openAPIV3Schema.properties.status.properties.phase.enum[]] |
    index("PartiallySucceeded") != null
  ' >/dev/null || die "RepackRun CRD phase enum does not contain PartiallySucceeded"

  kubectl get crd repackpolicies.repack.volcano.sh -o json | jq -e '
    [.spec.versions[].schema.openAPIV3Schema.properties.status.properties.lastRunStatus.properties.phase.enum[]] |
    index("PartiallySucceeded") != null
  ' >/dev/null || die "RepackPolicy lastRunStatus.phase enum does not contain PartiallySucceeded"

  local engine_json engine_count engine_ready engine_namespace
  engine_json="$(repack_engine_json)"
  engine_count="$(jq '.items | length' <<<"${engine_json}")"
  [[ "${engine_count}" -eq 1 ]] || die "expected exactly one app=volcano-repack-engine Deployment, got ${engine_count}"
  engine_ready="$(jq -r '.items[0].status.readyReplicas // 0' <<<"${engine_json}")"
  [[ "${engine_ready}" -ge 1 ]] || die "Repack Engine is not Ready"
  engine_namespace="$(jq -r '.items[0].metadata.namespace' <<<"${engine_json}")"
  log "Repack Engine: $(jq -r '.items[0].metadata.namespace + "/" + .items[0].metadata.name + " image=" + .items[0].spec.template.spec.containers[0].image' <<<"${engine_json}")"

  if [[ -n "${EXPECTED_IMAGE_TAG}" ]]; then
    local mismatched
    mismatched="$(kubectl get deployments.apps -n "${engine_namespace}" -o json | jq -r --arg tag "${EXPECTED_IMAGE_TAG}" '
      .items[]
      | select(.metadata.name | test("controller|repack|scheduler|admission"))
      | . as $d
      | [$d.spec.template.spec.containers[].image] as $images
      | select(any($images[]; contains($tag)) | not)
      | $d.metadata.name + "\t" + ($images | join(","))
    ')"
    [[ -z "${mismatched}" ]] || die "Deployments not using EXPECTED_IMAGE_TAG=${EXPECTED_IMAGE_TAG}:\n${mismatched}"
  fi

  local node node_json ready alloc hostname duplicate_count
  for node in "${NODES[@]}"; do
    node_json="$(kubectl get node "${node}" -o json)" || die "node not found: ${node}"
    ready="$(jq -r '[.status.conditions[] | select(.type == "Ready")][0].status // "Unknown"' <<<"${node_json}")"
    [[ "${ready}" == "True" ]] || die "node ${node} is not Ready"
    [[ "$(jq -r '.spec.unschedulable // false' <<<"${node_json}")" == "false" ]] || die "node ${node} is cordoned"
    alloc="$(jq -r --arg resource "${ACCELERATOR_RESOURCE}" '.status.allocatable[$resource] // "0"' <<<"${node_json}")"
    [[ "${alloc}" == "${CARDS_PER_NODE}" ]] || {
      kubectl get node "${node}" -o json | jq -r '.status.allocatable | to_entries[] | select(.key | test("huawei|ascend|asend"; "i")) | "  \(.key)=\(.value)"' >&2
      die "node ${node} allocatable ${ACCELERATOR_RESOURCE}=${alloc}, expected ${CARDS_PER_NODE}"
    }
    hostname="$(jq -r '.metadata.labels["kubernetes.io/hostname"] // ""' <<<"${node_json}")"
    [[ -n "${hostname}" ]] || die "node ${node} has no kubernetes.io/hostname label"
    duplicate_count="$(kubectl get nodes -l "kubernetes.io/hostname=${hostname}" -o json | jq '.items | length')"
    [[ "${duplicate_count}" -eq 1 ]] || die "hostname label ${hostname} does not identify exactly one node"
    log "node OK: ${node}, ${ACCELERATOR_RESOURCE}=${alloc}, hostname=${hostname}"
  done

  local selected_json existing
  selected_json="$(nodes_json)"
  existing="$(kubectl get pods -A -o json | jq -r \
    --arg resource "${ACCELERATOR_RESOURCE}" \
    --arg namespace "${TEST_NAMESPACE}" \
    --argjson nodes "${selected_json}" '
      .items[]
      | select(.metadata.namespace != $namespace)
      | select(.spec.nodeName != null and ($nodes | index(.spec.nodeName)) != null)
      | ([.spec.initContainers[]?, .spec.containers[]?]
          | map(.resources.requests[$resource] // .resources.limits[$resource] // "0")
          | map(tonumber)
          | add // 0) as $cards
      | select($cards > 0)
      | [.metadata.namespace, .metadata.name, .spec.nodeName, ($cards | tostring)]
      | @tsv
    ')"
  if [[ -n "${existing}" && "${ALLOW_EXISTING_ACCELERATOR_PODS}" != "true" ]]; then
    printf 'namespace\tpod\tnode\tcards\n%s\n' "${existing}" >&2
    die "selected nodes already contain accelerator workloads; use dedicated idle nodes"
  fi
  if [[ -n "${existing}" ]]; then
    log "WARNING: continuing with existing accelerator pods because ALLOW_EXISTING_ACCELERATOR_PODS=true"
  fi

  local active_executes
  active_executes="$(kubectl get repackruns.repack.volcano.sh -o json | jq -r '
    .items[]
    | select(.spec.mode == "Execute")
    | select(.status.phase == "Pending" or .status.phase == "Running")
    | .metadata.name + "\t" + (.status.phase // "")
  ')"
  [[ -z "${active_executes}" ]] || die "another Execute RepackRun is active:\n${active_executes}"

  log "preflight passed"
}

cleanup_managed() {
  local sel
  sel="$(selector)"

  if kubectl get crd repackpolicies.repack.volcano.sh >/dev/null 2>&1; then
    kubectl delete repackpolicies.repack.volcano.sh -l "${sel}" --ignore-not-found --wait=true >/dev/null
  fi
  if kubectl get crd repackruns.repack.volcano.sh >/dev/null 2>&1; then
    kubectl delete repackruns.repack.volcano.sh -l "${sel}" --ignore-not-found --wait=true >/dev/null
  fi

  if kubectl get namespace "${TEST_NAMESPACE}" >/dev/null 2>&1; then
    local managed namespace_test_id
    managed="$(kubectl get namespace "${TEST_NAMESPACE}" -o json | jq -r --arg key "${MANAGED_ANNOTATION}" '.metadata.annotations[$key] // ""')"
    namespace_test_id="$(kubectl get namespace "${TEST_NAMESPACE}" -o json | jq -r --arg key "${MANAGED_LABEL}" '.metadata.labels[$key] // ""')"
    [[ "${managed}" == "true" ]] || die "namespace ${TEST_NAMESPACE} exists but is not marked as managed; refusing to delete it"
    [[ "${namespace_test_id}" == "${TEST_ID}" ]] || die "namespace ${TEST_NAMESPACE} belongs to TEST_ID=${namespace_test_id}, not ${TEST_ID}; refusing to delete it"
    kubectl delete namespace "${TEST_NAMESPACE}" --wait=true --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  fi
  log "managed resources cleaned: id=${TEST_ID} namespace=${TEST_NAMESPACE}"
}

create_namespace() {
  jq -n \
    --arg name "${TEST_NAMESPACE}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg annotation_key "${MANAGED_ANNOTATION}" \
    --arg id "${TEST_ID}" '
      {
        apiVersion: "v1",
        kind: "Namespace",
        metadata: {
          name: $name,
          labels: {($label_key): $id},
          annotations: {($annotation_key): "true"}
        }
      }
    ' | kubectl apply -f - >/dev/null
}

create_headless_service() {
  jq -n \
    --arg namespace "${TEST_NAMESPACE}" \
    --arg name "${HEADLESS_SERVICE}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" '
      {
        apiVersion: "v1",
        kind: "Service",
        metadata: {namespace: $namespace, name: $name, labels: {($label_key): $id}},
        spec: {
          clusterIP: "None",
          selector: {($label_key): $id}
        }
      }
    ' | kubectl apply -f - >/dev/null
}

node_hostname() {
  kubectl get node "$1" -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}'
}

hostnames_json() {
  local node
  for node in "${NODES[@]}"; do
    node_hostname "${node}"
    printf '\n'
  done | jq -R 'select(length > 0)' | jq -s .
}

create_statefulset() {
  local name="$1" role="$2" cards="$3"
  jq -n \
    --arg namespace "${TEST_NAMESPACE}" \
    --arg name "${name}" \
    --arg service "${HEADLESS_SERVICE}" \
    --arg scheduler "${VOLCANO_SCHEDULER_NAME}" \
    --arg image "${TEST_IMAGE}" \
    --arg resource "${ACCELERATOR_RESOURCE}" \
    --arg cards "${cards}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" \
    --arg role "${role}" \
    --argjson hostnames "$(hostnames_json)" \
    --argjson tolerations "${TOLERATIONS_JSON}" '
      {
        apiVersion: "apps/v1",
        kind: "StatefulSet",
        metadata: {namespace: $namespace, name: $name, labels: {($label_key): $id}},
        spec: {
          serviceName: $service,
          replicas: 1,
          podManagementPolicy: "Parallel",
          updateStrategy: {type: "OnDelete"},
          selector: {matchLabels: {($label_key): $id, "repack-test.volcano.sh/role": $role}},
          template: {
            metadata: {labels: {($label_key): $id, "repack-test.volcano.sh/role": $role}},
            spec: {
              schedulerName: $scheduler,
              restartPolicy: "Always",
              terminationGracePeriodSeconds: 0,
              affinity: {
                nodeAffinity: {
                  requiredDuringSchedulingIgnoredDuringExecution: {
                    nodeSelectorTerms: [{
                      matchExpressions: [{
                        key: "kubernetes.io/hostname",
                        operator: "In",
                        values: $hostnames
                      }]
                    }]
                  }
                }
              },
              tolerations: $tolerations,
              containers: [{
                name: "holder",
                image: $image,
                imagePullPolicy: "IfNotPresent",
                command: ["sh", "-c", "while true; do sleep 3600; done"],
                resources: {
                  requests: {($resource): $cards},
                  limits: {($resource): $cards}
                }
              }]
            }
          }
        }
      }
    ' | kubectl apply -f - >/dev/null
}

requested_cards_on_node() {
  local node="$1"
  kubectl get pods -n "${TEST_NAMESPACE}" -o json | jq -r \
    --arg node "${node}" \
    --arg resource "${ACCELERATOR_RESOURCE}" '
      [.items[]
       | select(.spec.nodeName == $node)
       | .spec.containers[]?
       | (.resources.requests[$resource] // .resources.limits[$resource] // "0")
       | tonumber]
      | add // 0
    '
}

create_filler() {
  local name="$1" batch="$2" node="$3" cards="$4" hostname
  hostname="$(node_hostname "${node}")"
  jq -n \
    --arg namespace "${TEST_NAMESPACE}" \
    --arg name "${name}" \
    --arg batch "${batch}" \
    --arg image "${TEST_IMAGE}" \
    --arg resource "${ACCELERATOR_RESOURCE}" \
    --arg cards "${cards}" \
    --arg hostname "${hostname}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" \
    --argjson tolerations "${TOLERATIONS_JSON}" '
      {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          namespace: $namespace,
          name: $name,
          labels: {
            ($label_key): $id,
            "repack-test.volcano.sh/role": "fixture-filler",
            "repack-test.volcano.sh/placement": $batch
          }
        },
        spec: {
          schedulerName: "default-scheduler",
          restartPolicy: "Never",
          preemptionPolicy: "Never",
          nodeSelector: {"kubernetes.io/hostname": $hostname},
          tolerations: $tolerations,
          containers: [{
            name: "holder",
            image: $image,
            imagePullPolicy: "IfNotPresent",
            command: ["sh", "-c", "while true; do sleep 3600; done"],
            resources: {
              requests: {($resource): $cards},
              limits: {($resource): $cards}
            }
          }]
        }
      }
    ' | kubectl apply -f - >/dev/null
}

place_statefulset() {
  local name="$1" role="$2" target="$3" cards="$4"
  local index node used free filler selector
  selector="$(selector),repack-test.volcano.sh/placement=${role}"

  for index in "${!NODES[@]}"; do
    node="${NODES[$index]}"
    [[ "${node}" == "${target}" ]] && continue
    used="$(requested_cards_on_node "${node}")"
    free=$(( CARDS_PER_NODE - used ))
    (( free >= 0 )) || die "node ${node} test resource requests exceed ${CARDS_PER_NODE}"
    if (( free > 0 )); then
      filler="repack-fill-${role}-${index}"
      create_filler "${filler}" "${role}" "${node}" "${free}"
    fi
  done

  if kubectl get pods -n "${TEST_NAMESPACE}" -l "${selector}" -o json | jq -e '.items | length > 0' >/dev/null; then
    kubectl wait -n "${TEST_NAMESPACE}" --for=condition=Ready pod -l "${selector}" --timeout="${TIMEOUT_SECONDS}s" >/dev/null || {
      kubectl get pods -n "${TEST_NAMESPACE}" -l "${selector}" -o wide >&2 || true
      die "temporary filler Pods for ${role} did not become Ready"
    }
  fi

  create_statefulset "${name}" "${role}" "${cards}"
  wait_for_fixture_pod "${name}-0" "${target}"

  kubectl delete pods -n "${TEST_NAMESPACE}" -l "${selector}" --wait=true --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  log "placed ${name}-0 on ${target} without a single-node scheduling constraint; temporary fillers removed"
}

wait_for_fixture_pod() {
  local pod="$1" expected_node="$2"
  kubectl wait -n "${TEST_NAMESPACE}" --for=condition=Ready "pod/${pod}" --timeout="${TIMEOUT_SECONDS}s" >/dev/null || {
    kubectl get pod -n "${TEST_NAMESPACE}" "${pod}" -o wide >&2 || true
    kubectl describe pod -n "${TEST_NAMESPACE}" "${pod}" >&2 || true
    die "fixture pod ${pod} did not become Ready"
  }
  local actual_node
  actual_node="$(kubectl get pod -n "${TEST_NAMESPACE}" "${pod}" -o jsonpath='{.spec.nodeName}')"
  [[ "${actual_node}" == "${expected_node}" ]] || die "pod ${pod} landed on ${actual_node}, expected ${expected_node}"
}

pod_group_for_pod() {
  local pod="$1" pg
  pg="$(kubectl get pod -n "${TEST_NAMESPACE}" "${pod}" -o json | jq -r '.metadata.annotations["scheduling.k8s.io/group-name"] // ""')"
  [[ -n "${pg}" ]] || die "pod ${pod} has no scheduling.k8s.io/group-name annotation"
  kubectl get podgroups.scheduling.volcano.sh -n "${TEST_NAMESPACE}" "${pg}" >/dev/null || die "PodGroup ${TEST_NAMESPACE}/${pg} not found"
  printf '%s/%s' "${TEST_NAMESPACE}" "${pg}"
}

configure_replacement_hold() {
  local hold_moving="$1" moving_patch
  if [[ "${hold_moving}" == "true" ]]; then
    moving_patch="$(jq -nc --arg gate "${HOLD_GATE}" '{spec:{template:{spec:{schedulingGates:[{name:$gate}]}}}}')"
    kubectl patch statefulset.apps -n "${TEST_NAMESPACE}" "${MOVING_STS}" --type=merge -p "${moving_patch}" >/dev/null
  fi
}

prepare_fixture() {
  local hold_moving="$1"
  cleanup_managed
  create_namespace
  create_headless_service

  RECEIVER_NODE="${NODES[0]}"
  MOVING_NODE="${NODES[1]}"
  place_statefulset "${RECEIVER_STS}" receiver "${RECEIVER_NODE}" "${RECEIVER_CARDS}"
  place_statefulset "${MOVING_STS}" moving "${MOVING_NODE}" "${MOVING_CARDS}"

  RECEIVER_UID="$(kubectl get pod -n "${TEST_NAMESPACE}" "${RECEIVER_STS}-0" -o jsonpath='{.metadata.uid}')"
  MOVING_UID="$(kubectl get pod -n "${TEST_NAMESPACE}" "${MOVING_STS}-0" -o jsonpath='{.metadata.uid}')"
  RECEIVER_PG="$(pod_group_for_pod "${RECEIVER_STS}-0")"
  MOVING_PG="$(pod_group_for_pod "${MOVING_STS}-0")"
  configure_replacement_hold "${hold_moving}"

  log "fixture ready: ${RECEIVER_STS}-0=${RECEIVER_NODE}/${RECEIVER_CARDS} cards, PG=${RECEIVER_PG}"
  log "fixture ready: ${MOVING_STS}-0=${MOVING_NODE}/${MOVING_CARDS} cards, PG=${MOVING_PG}"
  log "fixture Pods remain movable across all 8 selected nodes"
  if (( CACHE_SETTLE_SECONDS > 0 )); then
    log "waiting ${CACHE_SETTLE_SECONDS}s for scheduler/repack caches to observe filler deletion"
    sleep "${CACHE_SETTLE_SECONDS}"
  fi
}

run_spec_json() {
  local mode="$1"
  jq -n \
    --arg mode "${mode}" \
    --arg resource "${ACCELERATOR_RESOURCE}" \
    --arg moving_pg "${MOVING_PG}" \
    --arg moving_cards "${MOVING_CARDS}" \
    --argjson nodes "$(nodes_json)" '
      {
        mode: $mode,
        goals: [{resource: $resource, minFragImprovementPercent: 0}],
        scope: {
          podGroups: {include: {names: [$moving_pg]}},
          nodes: {include: {names: $nodes}}
        },
        maxPerRun: {
          podGroups: 1,
          resources: {($resource): $moving_cards}
        },
        eviction: {gracePeriodSeconds: 0}
      }
    '
}

create_run() {
  local name="$1" mode="$2" spec
  spec="$(run_spec_json "${mode}")"
  jq -n \
    --arg name "${name}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" \
    --argjson spec "${spec}" '
      {
        apiVersion: "repack.volcano.sh/v1alpha1",
        kind: "RepackRun",
        metadata: {name: $name, labels: {($label_key): $id}},
        spec: $spec
      }
    ' | kubectl apply -f - >/dev/null
  log "created RepackRun ${name} mode=${mode}"
}

wait_terminal() {
  local name="$1" start now phase reason
  start="$(date +%s)"
  while true; do
    phase="$(kubectl get repackruns.repack.volcano.sh "${name}" -o json | jq -r '.status.phase // ""')"
    reason="$(kubectl get repackruns.repack.volcano.sh "${name}" -o json | jq -r '[.status.conditions[]? | select(.status == "True") | .reason][-1] // ""')"
    case "${phase}" in
      Succeeded|PartiallySucceeded|Failed)
        log "RepackRun ${name} terminal: phase=${phase}, reason=${reason}"
        return 0
        ;;
    esac
    now="$(date +%s)"
    if (( now - start >= TIMEOUT_SECONDS )); then
      kubectl get repackruns.repack.volcano.sh "${name}" -o yaml >&2 || true
      die "RepackRun ${name} did not become terminal within ${TIMEOUT_SECONDS}s"
    fi
    log "waiting RepackRun ${name}: phase=${phase:-<empty>} reason=${reason:-<empty>}"
    sleep 10
  done
}

assert_dry_run_plan() {
  local name="$1" json phase reason planned target move_from
  json="$(kubectl get repackruns.repack.volcano.sh "${name}" -o json)"
  phase="$(jq -r '.status.phase // ""' <<<"${json}")"
  reason="$(jq -r '[.status.conditions[]? | select(.type == "Complete" and .status == "True")][0].reason // ""' <<<"${json}")"
  [[ "${phase}" == "Succeeded" ]] || die "DryRun phase=${phase}, expected Succeeded"
  [[ "${reason}" == "RepackRecommended" ]] || die "DryRun reason=${reason}, expected RepackRecommended"
  planned="$(jq '.status.plan.freedNodes | length' <<<"${json}")"
  [[ "${planned}" -eq 1 ]] || die "expected exactly one planned freed node, got ${planned}"
  [[ "$(jq '.status.plan.moves | length' <<<"${json}")" -eq 1 ]] || die "expected exactly one planned move"
  target="$(jq -r '.status.plan.freedNodes[0]' <<<"${json}")"
  move_from="$(jq -r '.status.plan.moves[0].pods[0].fromNode' <<<"${json}")"
  [[ "${target}" == "${MOVING_NODE}" ]] || die "planner chose ${target} to free, expected fixture source ${MOVING_NODE}; refusing Execute"
  [[ "${move_from}" == "${MOVING_NODE}" ]] || die "planned move source ${move_from}, expected ${MOVING_NODE}"
  printf '%s' "${target}"
}

run_plan_probe() {
  PLAN_RUN="$(run_name plan)"
  create_run "${PLAN_RUN}" DryRun
  wait_terminal "${PLAN_RUN}"
  PLANNED_FREED_NODE="$(assert_dry_run_plan "${PLAN_RUN}")"
  log "DryRun safety gate passed: planned freed node=${PLANNED_FREED_NODE}"
}

create_blocker() {
  local target="$1" hostname
  hostname="$(node_hostname "${target}")"
  jq -n \
    --arg namespace "${TEST_NAMESPACE}" \
    --arg name "${BLOCKER_POD}" \
    --arg image "${TEST_IMAGE}" \
    --arg resource "${ACCELERATOR_RESOURCE}" \
    --arg cards "${CARDS_PER_NODE}" \
    --arg hostname "${hostname}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" \
    --argjson tolerations "${TOLERATIONS_JSON}" '
      {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          namespace: $namespace,
          name: $name,
          labels: {($label_key): $id, "repack-test.volcano.sh/role": "new-user-job"}
        },
        spec: {
          schedulerName: "default-scheduler",
          restartPolicy: "Never",
          preemptionPolicy: "Never",
          nodeSelector: {"kubernetes.io/hostname": $hostname},
          tolerations: $tolerations,
          containers: [{
            name: "holder",
            image: $image,
            imagePullPolicy: "IfNotPresent",
            command: ["sh", "-c", "while true; do sleep 3600; done"],
            resources: {
              requests: {($resource): $cards},
              limits: {($resource): $cards}
            }
          }]
        }
      }
    ' | kubectl apply -f - >/dev/null

  sleep 10
  local assigned
  assigned="$(kubectl get pod -n "${TEST_NAMESPACE}" "${BLOCKER_POD}" -o jsonpath='{.spec.nodeName}')"
  [[ -z "${assigned}" ]] || die "blocker scheduled before Execute on ${assigned}; fixture is not isolated as expected"
  log "new user blocker is Pending for all ${CARDS_PER_NODE} cards on ${target}"
}

wait_blocker_bound() {
  local target="$1" execute_run="${2:-}" start now assigned phase
  start="$(date +%s)"
  while true; do
    assigned="$(kubectl get pod -n "${TEST_NAMESPACE}" "${BLOCKER_POD}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
    if [[ "${assigned}" == "${target}" ]]; then
      log "new user blocker is bound to planned freed node ${target}"
      return 0
    fi
    [[ -z "${assigned}" ]] || die "blocker landed on unexpected node ${assigned}"
    if [[ -n "${execute_run}" ]]; then
      phase="$(kubectl get repackruns.repack.volcano.sh "${execute_run}" -o json 2>/dev/null | jq -r '.status.phase // ""' || true)"
      case "${phase}" in
        Failed|Succeeded|PartiallySucceeded)
          kubectl get repackruns.repack.volcano.sh "${execute_run}" -o yaml >&2 || true
          die "RepackRun ${execute_run} became terminal (${phase}) before blocker reached ${target}"
          ;;
      esac
    fi
    now="$(date +%s)"
    if (( now - start >= TIMEOUT_SECONDS )); then
      kubectl describe pod -n "${TEST_NAMESPACE}" "${BLOCKER_POD}" >&2 || true
      die "blocker was not bound to ${target} within ${TIMEOUT_SECONDS}s"
    fi
    sleep 5
  done
}

wait_replacement_and_release_hold() {
  local pod="${MOVING_STS}-0" start now json uid gate_index
  start="$(date +%s)"
  while true; do
    json="$(kubectl get pod -n "${TEST_NAMESPACE}" "${pod}" -o json 2>/dev/null || true)"
    if [[ -n "${json}" ]]; then
      uid="$(jq -r '.metadata.uid // ""' <<<"${json}")"
      gate_index="$(jq -r --arg gate "${HOLD_GATE}" '(.spec.schedulingGates // [] | map(.name) | index($gate)) // ""' <<<"${json}")"
      if [[ -n "${uid}" && "${uid}" != "${MOVING_UID}" && -n "${gate_index}" ]]; then
        kubectl patch pod -n "${TEST_NAMESPACE}" "${pod}" --type=json \
          -p "$(jq -nc --argjson index "${gate_index}" '[{op:"remove",path:("/spec/schedulingGates/" + ($index|tostring))}]')" >/dev/null
        log "released custom hold gate from replacement Pod ${pod} uid=${uid}"
        return 0
      fi
    fi
    now="$(date +%s)"
    if (( now - start >= TIMEOUT_SECONDS )); then
      kubectl get pods -n "${TEST_NAMESPACE}" -o yaml >&2 || true
      die "replacement Pod with custom hold gate was not observed"
    fi
    sleep 3
  done
}

assert_success() {
  local name="$1" json
  json="$(kubectl get repackruns.repack.volcano.sh "${name}" -o json)"
  jq -e '
    .status.phase == "Succeeded" and
    .status.result.metricsVerified == true and
    (.status.plan.freedNodes | length) == 1 and
    (.status.result.freedNodes | length) == 1 and
    .status.result.freedNodes == .status.plan.freedNodes and
    ([.status.conditions[]? | select(.type == "Complete" and .status == "True") | .reason]
      | any(. == "ExecutionCompleted" or . == "ExecutionCompletedWithAlternativePlacement")) and
    ([.status.conditions[]? | select(.type == "Failed" and .status == "True")] | length) == 0
  ' <<<"${json}" >/dev/null || {
    jq '.status' <<<"${json}" >&2
    die "Succeeded contract assertion failed for ${name}"
  }
  log "Succeeded contract verified for ${name}"
}

assert_partially_succeeded() {
  local name="$1" json
  json="$(kubectl get repackruns.repack.volcano.sh "${name}" -o json)"
  jq -e '
    .status.phase == "PartiallySucceeded" and
    .status.result.metricsVerified == true and
    (.status.plan.freedNodes | length) == 1 and
    .status.result.freedNodeCount == 0 and
    (.status.result.freedNodes | length) == 0 and
    ([.status.conditions[]? | select(.type == "Complete" and .status == "True" and .reason == "BenefitNotRealized")] | length) == 1 and
    ([.status.conditions[]? | select(.type == "Failed" and .status == "True")] | length) == 0
  ' <<<"${json}" >/dev/null || {
    jq '.status' <<<"${json}" >&2
    die "PartiallySucceeded contract assertion failed for ${name}"
  }
  log "PartiallySucceeded contract verified: planned=1 actual=0 Complete/BenefitNotRealized=true Failed!=true"
}

print_run_summary() {
  local name="$1"
  kubectl get repackruns.repack.volcano.sh "${name}" -o json | jq '{
    name: .metadata.name,
    phase: .status.phase,
    message: .status.message,
    conditions: [.status.conditions[]? | {type, status, reason, message}],
    plan: {
      freedNodes: .status.plan.freedNodes,
      moves: [.status.plan.moves[]? | {
        podGroupName,
        cards,
        pods: [.pods[]? | {namespace, name, fromNode, toNode}]
      }]
    },
    result: .status.result,
    relocations: [.status.relocations[]? | {
      victimPodName,
      plannedNodeName,
      eviction: .eviction.phase,
      placement: .placement
    }]
  }'
}

create_policy() {
  local name="$1" spec
  spec="$(run_spec_json Execute)"
  jq -n \
    --arg name "${name}" \
    --arg label_key "${MANAGED_LABEL}" \
    --arg id "${TEST_ID}" \
    --argjson spec "${spec}" '
      {
        apiVersion: "repack.volcano.sh/v1alpha1",
        kind: "RepackPolicy",
        metadata: {name: $name, labels: {($label_key): $id}},
        spec: {
          trigger: {cronSchedule: "* * * * *"},
          runTemplate: {
            metadata: {labels: {($label_key): $id}},
            spec: $spec
          },
          successfulRunsHistoryLimit: 3,
          failedRunsHistoryLimit: 3,
          suspend: false
        }
      }
    ' | kubectl apply -f - >/dev/null
  log "created RepackPolicy ${name}; waiting for the next minute trigger"
}

wait_policy_run_and_suspend() {
  local policy="$1" start now count run
  start="$(date +%s)"
  while true; do
    count="$(kubectl get repackruns.repack.volcano.sh -l "repack.volcano.sh/repack-policy=${policy}" -o json | jq '.items | length')"
    if [[ "${count}" -ge 1 ]]; then
      run="$(kubectl get repackruns.repack.volcano.sh -l "repack.volcano.sh/repack-policy=${policy}" -o json | jq -r '.items | sort_by(.metadata.creationTimestamp) | .[0].metadata.name')"
      kubectl patch repackpolicies.repack.volcano.sh "${policy}" --type=merge -p '{"spec":{"suspend":true}}' >/dev/null
      log "policy derived ${run}; policy suspended to prevent a second trigger"
      printf '%s' "${run}"
      return 0
    fi
    now="$(date +%s)"
    (( now - start < TIMEOUT_SECONDS )) || die "policy ${policy} did not derive a run"
    sleep 5
  done
}

assert_policy_accounting() {
  local policy="$1" run="$2" start now json
  start="$(date +%s)"
  while true; do
    json="$(kubectl get repackpolicies.repack.volcano.sh "${policy}" -o json)"
    if jq -e --arg run "${run}" '
      .status.lastSuccessfulTime != null and
      .status.lastRunStatus.name == $run and
      .status.lastRunStatus.phase == "PartiallySucceeded"
    ' <<<"${json}" >/dev/null; then
      log "policy accounting verified: PartiallySucceeded updated lastSuccessfulTime and lastRunStatus"
      return 0
    fi
    now="$(date +%s)"
    (( now - start < TIMEOUT_SECONDS )) || {
      jq '.status' <<<"${json}" >&2
      die "policy ${policy} did not account ${run} as successful"
    }
    sleep 5
  done
}
