#!/usr/bin/env bash

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$SCRIPT_DIR/config.sh"

MANIFEST_DIR="$SCRIPT_DIR/manifests"
RENDERED_DIR="$SCRIPT_DIR/.rendered"
EVIDENCE_ROOT="$SCRIPT_DIR/evidence"
SOURCE_NODE="${NODES[0]:-}"
RECEIVER_NODE="${NODES[1]:-}"
SOURCE_POD_NAME="${SOURCE_STS}-0"
RECEIVER_POD_NAME="${RECEIVER_STS}-0"

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令: $1"
}

validate_config() {
  [ "$EXPECTED_KUBE_CONTEXT" != "REPLACE_WITH_KUBE_CONTEXT" ] || fail "请先修改 config.sh 中的 EXPECTED_KUBE_CONTEXT"
  [ "$EXPECTED_ENGINE_IMAGE" != "REPLACE_WITH_PATCHED_ENGINE_IMAGE" ] || fail "请先在 config.sh 填写包含修复的 EXPECTED_ENGINE_IMAGE"
  [ "${#NODES[@]}" -eq 8 ] || fail "config.sh 必须配置恰好 8 个节点"

  local node
  for node in "${NODES[@]}"; do
    case "$node" in
      ""|REPLACE_WITH_NODE_*) fail "请先在 config.sh 填写 8 个真实节点名" ;;
    esac
  done

  local unique_count
  unique_count="$(printf '%s\n' "${NODES[@]}" | sort -u | wc -l | tr -d ' ')"
  [ "$unique_count" -eq 8 ] || fail "config.sh 中的 8 个节点名必须互不相同"
}

verify_context() {
  local current_context
  current_context="$(kubectl config current-context)"
  [ "$current_context" = "$EXPECTED_KUBE_CONTEXT" ] || fail "kubectl context 为 '$current_context'，预期 '$EXPECTED_KUBE_CONTEXT'"
}

render_manifest() {
  local input="$1"
  local output="$2"
  sed \
    -e "s|__TEST_NAMESPACE__|$TEST_NAMESPACE|g" \
    -e "s|__SOURCE_STS__|$SOURCE_STS|g" \
    -e "s|__RECEIVER_STS__|$RECEIVER_STS|g" \
    -e "s|__SCHEDULER_NAME__|$SCHEDULER_NAME|g" \
    -e "s|__QUEUE_NAME__|$QUEUE_NAME|g" \
    -e "s|__TEST_IMAGE__|$TEST_IMAGE|g" \
    -e "s|__TARGET_RESOURCE__|$TARGET_RESOURCE|g" \
    -e "s|__TEST_POD_CARDS__|$TEST_POD_CARDS|g" \
    -e "s|__SOURCE_NODE_LABEL_KEY__|$SOURCE_NODE_LABEL_KEY|g" \
    -e "s|__SOURCE_NODE_LABEL_VALUE__|$SOURCE_NODE_LABEL_VALUE|g" \
    -e "s|__RUN_NAME__|$RUN_NAME|g" \
    -e "s|__SOURCE_NODE__|$SOURCE_NODE|g" \
    "$input" >"$output"
}

render_all() {
  mkdir -p "$RENDERED_DIR"
  render_manifest "$MANIFEST_DIR/00-namespace.yaml" "$RENDERED_DIR/00-namespace.yaml"
  render_manifest "$MANIFEST_DIR/10-source-before.yaml" "$RENDERED_DIR/10-source-before.yaml"
  render_manifest "$MANIFEST_DIR/20-receiver.yaml" "$RENDERED_DIR/20-receiver.yaml"
  render_manifest "$MANIFEST_DIR/30-source-after.yaml" "$RENDERED_DIR/30-source-after.yaml"
  render_manifest "$MANIFEST_DIR/40-repack-run.yaml" "$RENDERED_DIR/40-repack-run.yaml"
}

remove_taint_from_all_nodes() {
  local key="$1"
  local node
  for node in "${NODES[@]}"; do
    kubectl taint node "$node" "${key}-" >/dev/null 2>&1 || true
  done
}

taint_all_except() {
  local allowed_node="$1"
  local key="$2"
  local node
  for node in "${NODES[@]}"; do
    if [ "$node" != "$allowed_node" ]; then
      kubectl taint node "$node" "$key=true:NoSchedule" --overwrite >/dev/null
    fi
  done
}

hold_auxiliary_receivers() {
  local index
  for index in 2 3 4 5 6 7; do
    kubectl taint node "${NODES[$index]}" "$RECEIVER_HOLD_TAINT_KEY=true:NoSchedule" --overwrite >/dev/null
  done
}

wait_for_pod_ready() {
  local pod_name="$1"
  local deadline remaining
  deadline=$(( $(date +%s) + POD_READY_TIMEOUT_SECONDS ))
  while ! kubectl -n "$TEST_NAMESPACE" get pod "$pod_name" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "等待 Pod $pod_name 创建超时"
    sleep 2
  done
  remaining=$(( deadline - $(date +%s) ))
  [ "$remaining" -gt 0 ] || fail "等待 Pod $pod_name Ready 超时"
  kubectl -n "$TEST_NAMESPACE" wait "pod/$pod_name" --for=condition=Ready --timeout="${remaining}s" >/dev/null
}

pod_node() {
  kubectl -n "$TEST_NAMESPACE" get pod "$1" -o json | jq -r '.spec.nodeName // ""'
}

pod_target_request() {
  kubectl -n "$TEST_NAMESPACE" get pod "$1" -o json | jq -r --arg resource "$TARGET_RESOURCE" \
    '[.spec.containers[]?
      | [((.resources.requests[$resource] // "0") | tonumber),
         ((.resources.limits[$resource] // "0") | tonumber)]
      | max
    ] | add // 0'
}

wait_for_one_source_podgroup() {
  local deadline count
  deadline=$(( $(date +%s) + POD_READY_TIMEOUT_SECONDS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    count="$(kubectl -n "$TEST_NAMESPACE" get podgroup -l repack-fix-test=source -o json | jq '.items | length')"
    if [ "$count" -eq 1 ]; then
      return 0
    fi
    sleep 2
  done
  fail "等待 source StatefulSet 的自动 PodGroup 超时"
}

target_consumers_on_node() {
  local node="$1"
  kubectl get pods -A --field-selector "spec.nodeName=$node" -o json | jq -r --arg resource "$TARGET_RESOURCE" '
    [.items[]
      | select(.status.phase != "Succeeded" and .status.phase != "Failed")
      | select(
          any(.spec.containers[]?;
            (((.resources.requests[$resource] // "0") | tonumber) > 0) or
            (((.resources.limits[$resource] // "0") | tonumber) > 0)
          ) or
          any(.spec.initContainers[]?;
            (((.resources.requests[$resource] // "0") | tonumber) > 0) or
            (((.resources.limits[$resource] // "0") | tonumber) > 0)
          )
        )
      | "\(.metadata.namespace)/\(.metadata.name)"
    ] | join(",")'
}

terminal_reason() {
  kubectl get repackrun "$RUN_NAME" -o json | jq -r '
    [.status.conditions[]?
      | select(.status == "True" and (.type == "Complete" or .type == "Failed"))
      | .reason
    ][-1] // ""'
}

collect_evidence() {
  local evidence_dir="$1"
  local engine_deployment
  mkdir -p "$evidence_dir"

  kubectl get nodes -o wide >"$evidence_dir/nodes-wide.txt" 2>&1 || true
  kubectl get nodes -o yaml >"$evidence_dir/nodes.yaml" 2>&1 || true
  kubectl get repackrun "$RUN_NAME" -o yaml >"$evidence_dir/repackrun.yaml" 2>&1 || true
  kubectl describe repackrun "$RUN_NAME" >"$evidence_dir/repackrun-describe.txt" 2>&1 || true
  kubectl -n "$TEST_NAMESPACE" get statefulset,pod,podgroup -o wide >"$evidence_dir/workloads-wide.txt" 2>&1 || true
  kubectl -n "$TEST_NAMESPACE" get statefulset,pod,podgroup -o yaml >"$evidence_dir/workloads.yaml" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"$evidence_dir/events.txt" 2>&1 || true

  engine_deployment="$(kubectl -n "$SYSTEM_NAMESPACE" get deployment -l app=volcano-repack-engine -o json 2>/dev/null | jq -r '.items[0].metadata.name // ""')"
  if [ -n "$engine_deployment" ]; then
    kubectl -n "$SYSTEM_NAMESPACE" logs "deployment/$engine_deployment" --all-containers --tail=500 >"$evidence_dir/repack-engine.log" 2>&1 || true
    kubectl -n "$SYSTEM_NAMESPACE" get deployment "$engine_deployment" -o yaml >"$evidence_dir/repack-engine-deployment.yaml" 2>&1 || true
  fi

  kubectl -n "$SYSTEM_NAMESPACE" logs -l app=volcano-controller --all-containers --tail=500 --prefix >"$evidence_dir/volcano-controller.log" 2>&1 || true
  kubectl -n "$SYSTEM_NAMESPACE" logs -l app=volcano-scheduler --all-containers --tail=500 --prefix >"$evidence_dir/volcano-scheduler.log" 2>&1 || true
  cp "$RENDERED_DIR"/*.yaml "$evidence_dir/" 2>/dev/null || true
}
