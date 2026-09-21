#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

require_command kubectl
require_command jq
validate_config
verify_context
render_all

[ "$(pod_node "$SOURCE_POD_NAME")" = "$SOURCE_NODE" ] || fail "source fixture 不在 $SOURCE_NODE，请重新执行 prepare-fixture.sh"
[ "$(pod_node "$RECEIVER_POD_NAME")" = "$RECEIVER_NODE" ] || fail "receiver fixture 不在 $RECEIVER_NODE，请重新执行 prepare-fixture.sh"
[ "$(pod_target_request "$SOURCE_POD_NAME")" -eq "$TEST_POD_CARDS" ] || fail "source fixture 的目标资源请求已变化"
[ "$(pod_target_request "$RECEIVER_POD_NAME")" -eq "$TEST_POD_CARDS" ] || fail "receiver fixture 的目标资源请求已变化"
kubectl get repackrun "$RUN_NAME" >/dev/null 2>&1 && fail "RepackRun $RUN_NAME 已存在，请先执行 cleanup.sh 并重建 fixture"

evidence_dir="$EVIDENCE_ROOT/$(date '+%Y%m%d-%H%M%S')"
mkdir -p "$evidence_dir"

on_exit() {
  local rc=$?
  trap - EXIT
  set +e
  log "测试未正常完成，收集现场到 $evidence_dir"
  collect_evidence "$evidence_dir"
  remove_taint_from_all_nodes "$RECEIVER_HOLD_TAINT_KEY"
  exit "$rc"
}
trap on_exit EXIT

log "临时禁止 N2-N7 作为接收节点，保证计划唯一为 $SOURCE_NODE -> $RECEIVER_NODE"
hold_auxiliary_receivers

old_uid="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json | jq -r '.metadata.uid')"
log "创建 Execute RepackRun: $RUN_NAME，victim UID=$old_uid"
kubectl create -f "$RENDERED_DIR/40-repack-run.yaml" >/dev/null

log "等待 source victim 进入 Terminating，最长 ${RUN_TIMEOUT_SECONDS}s"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待 victim 进入 Terminating 超时"

  run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
  phase="$(printf '%s' "$run_json" | jq -r '.status.phase // ""')"
  if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then
    fail "victim 尚未进入 Terminating，RepackRun 已终态: phase=$phase reason=$(terminal_reason)"
  fi

  if pod_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json 2>/dev/null)"; then
    current_uid="$(printf '%s' "$pod_json" | jq -r '.metadata.uid')"
    deletion_timestamp="$(printf '%s' "$pod_json" | jq -r '.metadata.deletionTimestamp // ""')"
    [ "$current_uid" = "$old_uid" ] || fail "源 Pod 已在更新模板前被重建，本轮无法证明目标场景"
    if [ -n "$deletion_timestamp" ]; then
      break
    fi
  else
    fail "源 Pod 在更新模板前已消失，本轮无法证明目标场景"
  fi
  sleep 1
done

log "victim 已进入 Terminating；把 StatefulSet 模板改为不申请 $TARGET_RESOURCE，并强制 replacement 回到 $SOURCE_NODE"
kubectl apply -f "$RENDERED_DIR/30-source-after.yaml" >/dev/null

template_json="$(kubectl -n "$TEST_NAMESPACE" get statefulset "$SOURCE_STS" -o json)"
template_cards="$(printf '%s' "$template_json" | jq -r --arg resource "$TARGET_RESOURCE" '[.spec.template.spec.containers[]? | [((.resources.requests[$resource] // "0") | tonumber), ((.resources.limits[$resource] // "0") | tonumber)] | max] | add // 0')"
template_selector="$(printf '%s' "$template_json" | jq -r --arg key "$SOURCE_NODE_LABEL_KEY" '.spec.template.spec.nodeSelector[$key] // ""')"
[ "$template_cards" -eq 0 ] || fail "StatefulSet 新模板仍申请 $template_cards 张目标卡"
[ "$template_selector" = "$SOURCE_NODE_LABEL_VALUE" ] || fail "StatefulSet 新模板未正确绑定源节点标签"

log "等待同名 replacement Pod 出现、Ready 并落回源节点"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
new_uid=""
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待 replacement Pod Ready 超时"
  if pod_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json 2>/dev/null)"; then
    current_uid="$(printf '%s' "$pod_json" | jq -r '.metadata.uid')"
    ready="$(printf '%s' "$pod_json" | jq -r 'any(.status.conditions[]?; .type == "Ready" and .status == "True")')"
    if [ "$current_uid" != "$old_uid" ] && [ "$ready" = "true" ]; then
      new_uid="$current_uid"
      break
    fi
  fi
  sleep 2
done

actual_node="$(pod_node "$SOURCE_POD_NAME")"
actual_cards="$(pod_target_request "$SOURCE_POD_NAME")"
[ "$actual_node" = "$SOURCE_NODE" ] || fail "replacement 实际落在 $actual_node，预期回到 $SOURCE_NODE"
[ "$actual_cards" -eq 0 ] || fail "replacement 实际仍申请 $actual_cards 张目标卡"

log "replacement UID=$new_uid 已落到 $actual_node，等待 RepackRun 终态"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待 RepackRun 终态超时"
  run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
  phase="$(printf '%s' "$run_json" | jq -r '.status.phase // ""')"
  if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then
    break
  fi
  sleep 2
done

reason="$(terminal_reason)"
relocation_json="$(printf '%s' "$run_json" | jq -c --arg victim "$SOURCE_POD_NAME" '[.status.relocations[]? | select(.victimPodName == $victim)][0] // {}')"

[ "$phase" = "Succeeded" ] || fail "RepackRun 失败: phase=$phase reason=$reason message=$(printf '%s' "$run_json" | jq -r '.status.message // ""')"
[ "$reason" = "ExecutionCompletedWithAlternativePlacement" ] || fail "终态 reason=$reason，预期 ExecutionCompletedWithAlternativePlacement"

printf '%s' "$run_json" | jq -e --arg source "$SOURCE_NODE" --arg receiver "$RECEIVER_NODE" --argjson cards "$TEST_POD_CARDS" '
  (.status.plan.freedNodes == [$source]) and
  (.status.plan.summary.freedNodeCount == 1) and
  (.status.plan.summary.movedCardCount == $cards) and
  (any(.status.plan.moves[]?.pods[]?; .fromNode == $source and .toNode == $receiver and .cards == $cards)) and
  (.status.result.metricsVerified == true) and
  (.status.result.freedNodes == [$source]) and
  (.status.result.freedNodeCount == 1) and
  (.status.result.movedCardCount == $cards)
' >/dev/null || fail "plan/result 不符合单 Pod 4 卡从 N0 迁到 N1、N0 被释放的预期"

printf '%s' "$relocation_json" | jq -e \
  --arg receiver "$RECEIVER_NODE" \
  --arg source "$SOURCE_NODE" \
  --arg oldUID "$old_uid" \
  --arg newUID "$new_uid" '
    (.victimPodUID == $oldUID) and
    (.plannedNodeName == $receiver) and
    (.eviction.phase == "Accepted") and
    (.placement.phase == "Placed") and
    (.placement.selectedNodeName == $receiver) and
    (.placement.actualNodeName == $source) and
    (.placement.replacementPodUID == $newUID)
  ' >/dev/null || fail "relocation 记录不符合 selected=N1、actual=N0 的预期"

source_consumers="$(target_consumers_on_node "$SOURCE_NODE")"
[ -z "$source_consumers" ] || fail "源节点仍有目标资源消费者: $source_consumers"

collect_evidence "$evidence_dir"
remove_taint_from_all_nodes "$RECEIVER_HOLD_TAINT_KEY"
trap - EXIT

log "PASS: 修复已通过真实集群验证"
printf 'phase=%s\nreason=%s\nselectedNode=%s\nactualNode=%s\nreplacementCards=%s\nfreedNodes=%s\nevidence=%s\n' \
  "$phase" "$reason" "$RECEIVER_NODE" "$SOURCE_NODE" "$actual_cards" "$SOURCE_NODE" "$evidence_dir"
printf '\n资源保留以便检查；确认后执行 ./cleanup.sh\n'
