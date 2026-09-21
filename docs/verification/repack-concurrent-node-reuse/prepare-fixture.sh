#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

require_command kubectl
require_command jq
validate_config
verify_context

"$SCRIPT_DIR/preflight.sh"

clear_initial_taints() {
  remove_taint_from_all_nodes "$INITIAL_TAINT_KEY"
}
trap clear_initial_taints EXIT

log "创建独立测试 namespace，并为源节点添加新作业专用标签"
kubectl apply -f "$RENDERED_DIR/00-namespace.yaml" >/dev/null
kubectl label node "$SOURCE_NODE" "$SOURCE_NODE_LABEL_KEY=$SOURCE_NODE_LABEL_VALUE" --overwrite >/dev/null

log "通过临时 NoSchedule 污点将 source ${SOURCE_POD_CARDS} 卡 Pod 定位到 $SOURCE_NODE"
taint_all_except "$SOURCE_NODE" "$INITIAL_TAINT_KEY"
kubectl apply -f "$RENDERED_DIR/10-source.yaml" >/dev/null
wait_for_pod_ready "$SOURCE_POD_NAME"
[ "$(pod_node "$SOURCE_POD_NAME")" = "$SOURCE_NODE" ] || fail "$SOURCE_POD_NAME 未落到 $SOURCE_NODE"
[ "$(pod_target_request "$SOURCE_POD_NAME")" -eq "$SOURCE_POD_CARDS" ] || fail "$SOURCE_POD_NAME 未申请 $SOURCE_POD_CARDS 张目标卡"
clear_initial_taints

log "通过临时 NoSchedule 污点将 receiver ${RECEIVER_POD_CARDS} 卡 Pod 定位到 $RECEIVER_NODE"
taint_all_except "$RECEIVER_NODE" "$INITIAL_TAINT_KEY"
kubectl apply -f "$RENDERED_DIR/20-receiver.yaml" >/dev/null
wait_for_pod_ready "$RECEIVER_POD_NAME"
[ "$(pod_node "$RECEIVER_POD_NAME")" = "$RECEIVER_NODE" ] || fail "$RECEIVER_POD_NAME 未落到 $RECEIVER_NODE"
[ "$(pod_target_request "$RECEIVER_POD_NAME")" -eq "$RECEIVER_POD_CARDS" ] || fail "$RECEIVER_POD_NAME 未申请 $RECEIVER_POD_CARDS 张目标卡"
clear_initial_taints

log "等待 pg-controller 为 source StatefulSet 创建带筛选标签的 PodGroup"
wait_for_one_source_podgroup

source_consumers="$(target_consumers_on_node "$SOURCE_NODE")"
receiver_consumers="$(target_consumers_on_node "$RECEIVER_NODE")"
[ "$source_consumers" = "$TEST_NAMESPACE/$SOURCE_POD_NAME" ] || fail "$SOURCE_NODE 的目标资源消费者不符合预期: $source_consumers"
[ "$receiver_consumers" = "$TEST_NAMESPACE/$RECEIVER_POD_NAME" ] || fail "$RECEIVER_NODE 的目标资源消费者不符合预期: $receiver_consumers"

log "测试布局已就绪"
kubectl -n "$TEST_NAMESPACE" get pod,podgroup -o wide
printf '\n预期布局: %s=%s 卡, %s=%s 卡, 其余节点=0 卡\n' \
  "$SOURCE_NODE" "$SOURCE_POD_CARDS" "$RECEIVER_NODE" "$RECEIVER_POD_CARDS"
printf '下一步: ./run-test.sh\n'

