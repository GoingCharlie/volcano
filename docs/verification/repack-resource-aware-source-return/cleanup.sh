#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

require_command kubectl
validate_config
verify_context

cleanup_node_metadata() {
  remove_taint_from_all_nodes "$INITIAL_TAINT_KEY"
  remove_taint_from_all_nodes "$RECEIVER_HOLD_TAINT_KEY"
  local node
  for node in "${NODES[@]}"; do
    kubectl label node "$node" "${SOURCE_NODE_LABEL_KEY}-" >/dev/null 2>&1 || true
  done
}
trap cleanup_node_metadata EXIT

log "删除本测试的 RepackRun 和专用 namespace"
kubectl delete repackrun "$RUN_NAME" --ignore-not-found --wait=true --timeout=180s
kubectl delete namespace "$TEST_NAMESPACE" --ignore-not-found --wait=true --timeout=300s

log "清理本测试添加的污点和节点标签"
cleanup_node_metadata
trap - EXIT

log "集群测试资源已清理；evidence 目录保留"
