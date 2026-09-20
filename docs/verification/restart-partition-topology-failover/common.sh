#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=nodes.sh
source "${SCRIPT_DIR}/nodes.sh"

NODES=("$N0" "$N1" "$N2" "$N3" "$N4" "$N5" "$N6" "$N7")
RECEIVER_LABEL="test.volcano.sh/migration-case"
TOPOLOGY_PREFIX="hypernode-restartmigration-"
STATE_DIR="${SCRIPT_DIR}/.state"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate_local_config() {
  require_command kubectl
  require_command jq
  require_command sed

  local node unique_count ready allocatable queue_state
  unique_count=$(printf '%s\n' "${NODES[@]}" | sort -u | wc -l | tr -d ' ')
  [[ "$unique_count" == "8" ]] || die "nodes.sh must contain 8 distinct node names"

  for node in "${NODES[@]}"; do
    [[ -n "$node" && "$node" != replace-with-* ]] || \
      die "edit nodes.sh and replace every node placeholder"

    kubectl get node "$node" >/dev/null

    ready=$(kubectl get node "$node" -o json | jq -r '
      [.status.conditions[] | select(.type == "Ready") | .status][0] // "False"')
    [[ "$ready" == "True" ]] || die "node $node is not Ready"

    allocatable=$(kubectl get node "$node" \
      -o go-template="{{index .status.allocatable \"${ACCELERATOR_RESOURCE}\"}}")
    [[ "$allocatable" == "$CARDS_PER_NODE" ]] || \
      die "node $node allocatable ${ACCELERATOR_RESOURCE}=$allocatable, expected $CARDS_PER_NODE"
  done

  kubectl get crd jobs.batch.volcano.sh >/dev/null
  kubectl get crd podgroups.scheduling.volcano.sh >/dev/null
  kubectl get crd hypernodes.topology.volcano.sh >/dev/null
  kubectl get namespace "$JOB_NAMESPACE" >/dev/null

  queue_state=$(kubectl get queues.scheduling.volcano.sh "$QUEUE_NAME" \
    -o jsonpath='{.status.state}')
  [[ "$queue_state" == "Open" ]] || \
    die "queue $QUEUE_NAME state is '$queue_state', expected Open"
}

new_evidence_dir() {
  local name=$1 dir
  dir="${EVIDENCE_ROOT}/$(date +%Y%m%d-%H%M%S)-${name}"
  mkdir -p "$dir"
  printf '%s\n' "$dir"
}

clear_receiver_labels() {
  kubectl label nodes --all "${RECEIVER_LABEL}-" >/dev/null 2>&1 || true
}

set_receivers() {
  local case_name=$1
  shift
  clear_receiver_labels
  kubectl label nodes "$@" "${RECEIVER_LABEL}=${case_name}" --overwrite >/dev/null
}

render_job() {
  local case_name=$1 tier=$2 output=$3
  sed \
    -e "s|__CASE__|${case_name}|g" \
    -e "s|__TIER__|${tier}|g" \
    -e "s|__JOB_NAMESPACE__|${JOB_NAMESPACE}|g" \
    -e "s|__SCHEDULER_NAME__|${SCHEDULER_NAME}|g" \
    -e "s|__QUEUE_NAME__|${QUEUE_NAME}|g" \
    -e "s|__TEST_IMAGE__|${TEST_IMAGE}|g" \
    -e "s|__ACCELERATOR_RESOURCE__|${ACCELERATOR_RESOURCE}|g" \
    -e "s|__CARDS_PER_POD__|${CARDS_PER_POD}|g" \
    "${SCRIPT_DIR}/job-template.yaml" > "$output"
}

wait_for_pod_count() {
  local case_name=$1 expected=$2 timeout_seconds=${3:-300}
  local deadline=$((SECONDS + timeout_seconds)) count

  while (( SECONDS < deadline )); do
    count=$(kubectl get pod -n "$JOB_NAMESPACE" \
      -l "test.volcano.sh/case=${case_name}" \
      -o json 2>/dev/null | jq '.items | length')
    if [[ "$count" == "$expected" ]]; then
      return 0
    fi
    sleep 2
  done

  die "timed out waiting for $expected pods for case $case_name"
}

wait_case_ready() {
  local case_name=$1
  wait_for_pod_count "$case_name" 4 300
  kubectl wait -n "$JOB_NAMESPACE" --for=condition=Ready \
    pod -l "test.volcano.sh/case=${case_name}" --timeout=300s >/dev/null
}

wait_retry_count_one() {
  local job_name=$1 deadline=$((SECONDS + 180)) retry_count

  while (( SECONDS < deadline )); do
    retry_count=$(kubectl get jobs.batch.volcano.sh "$job_name" \
      -n "$JOB_NAMESPACE" -o jsonpath='{.status.retryCount}' 2>/dev/null || true)
    if [[ "$retry_count" == "1" ]]; then
      return 0
    fi
    if [[ -n "$retry_count" && "$retry_count" -gt 1 ]]; then
      die "$job_name retryCount unexpectedly increased to $retry_count"
    fi
    sleep 2
  done

  die "timed out waiting for $job_name retryCount=1"
}

uids_for_partition() {
  local case_name=$1 partition_id=$2
  kubectl get pod -n "$JOB_NAMESPACE" \
    -l "test.volcano.sh/case=${case_name},volcano.sh/partition-id=${partition_id}" \
    -o json | jq -r '.items[].metadata.uid' | sort
}

nodes_for_partition() {
  local case_name=$1 partition_id=$2
  kubectl get pod -n "$JOB_NAMESPACE" \
    -l "test.volcano.sh/case=${case_name},volcano.sh/partition-id=${partition_id}" \
    -o json | jq -r '.items[].spec.nodeName' | sort
}

assert_partition_nodes() {
  local case_name=$1 partition_id=$2 expected_a=$3 expected_b=$4
  local actual expected
  actual=$(nodes_for_partition "$case_name" "$partition_id")
  expected=$(printf '%s\n%s\n' "$expected_a" "$expected_b" | sort)
  [[ "$actual" == "$expected" ]] || {
    echo "Expected nodes:" >&2
    echo "$expected" >&2
    echo "Actual nodes:" >&2
    echo "$actual" >&2
    die "partition $partition_id has unexpected placement"
  }
}

assert_uid_files_disjoint() {
  local before=$1 after=$2 common_file=$3
  comm -12 "$before" "$after" > "$common_file"
  [[ ! -s "$common_file" ]] || die "fault partition still contains old Pod UID(s)"
}

assert_uid_files_equal() {
  local before=$1 after=$2 diff_file=$3
  if ! diff -u "$before" "$after" > "$diff_file"; then
    die "control partition Pod UID(s) changed"
  fi
}

wait_fault_partition_recreated_pending() {
  local case_name=$1 partition_id=$2 before_uid_file=$3 evidence_dir=$4
  local deadline=$((SECONDS + 180)) current_uid_file phases count common_count
  current_uid_file="${evidence_dir}/${case_name}-fault-invalid-current.uid"

  while (( SECONDS < deadline )); do
    uids_for_partition "$case_name" "$partition_id" > "$current_uid_file"
    count=$(wc -l < "$current_uid_file" | tr -d ' ')
    common_count=$(comm -12 "$before_uid_file" "$current_uid_file" | wc -l | tr -d ' ')
    phases=$(kubectl get pod -n "$JOB_NAMESPACE" \
      -l "test.volcano.sh/case=${case_name},volcano.sh/partition-id=${partition_id}" \
      -o json | jq -r '[.items[].status.phase] | unique | join(",")')

    if [[ "$count" == "2" && "$common_count" == "0" && "$phases" == "Pending" ]]; then
      return 0
    fi
    sleep 2
  done

  die "fault partition did not settle as two recreated Pending pods"
}

wait_for_hypernodes() {
  local deadline=$((SECONDS + 180)) count
  while (( SECONDS < deadline )); do
    count=$(kubectl get hypernodes -o json 2>/dev/null | jq \
      --arg prefix "$TOPOLOGY_PREFIX" \
      '[.items[] | select(.metadata.name | startswith($prefix))] | length')
    if [[ "$count" == "7" ]]; then
      return 0
    fi
    sleep 2
  done
  die "expected 7 HyperNodes with prefix $TOPOLOGY_PREFIX"
}
