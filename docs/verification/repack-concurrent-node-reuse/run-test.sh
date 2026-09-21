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
[ "$(pod_target_request "$SOURCE_POD_NAME")" -eq "$SOURCE_POD_CARDS" ] || fail "source fixture 的目标资源请求已变化"
[ "$(pod_target_request "$RECEIVER_POD_NAME")" -eq "$RECEIVER_POD_CARDS" ] || fail "receiver fixture 的目标资源请求已变化"
kubectl get repackrun "$RUN_NAME" >/dev/null 2>&1 && fail "RepackRun $RUN_NAME 已存在，请执行 cleanup.sh 后重建 fixture"
kubectl -n "$TEST_NAMESPACE" get job.batch.volcano.sh "$REUSE_JOB_NAME" >/dev/null 2>&1 && fail "复用测试 Job $REUSE_JOB_NAME 已存在"
[ ! -e "$ENGINE_DEPLOYMENT_STATE_FILE" ] && [ ! -e "$ENGINE_REPLICAS_STATE_FILE" ] ||
  fail "检测到上次测试未恢复的 Engine 状态，请先执行 ./cleanup.sh"

evidence_dir="$EVIDENCE_ROOT/$(date '+%Y%m%d-%H%M%S')"
mkdir -p "$evidence_dir"

on_exit() {
  local rc=$?
  trap - EXIT
  set +e
  log "测试未正常完成，收集现场到 $evidence_dir"
  collect_evidence "$evidence_dir"
  restore_repack_engine
  remove_taint_from_all_nodes "$RECEIVER_HOLD_TAINT_KEY"
  exit "$rc"
}
trap on_exit EXIT

log "临时禁止 N2-N7 作为接收节点，保证计划唯一为 $SOURCE_NODE -> $RECEIVER_NODE"
hold_auxiliary_receivers

old_uid="$(pod_uid "$SOURCE_POD_NAME")"
log "创建 Execute RepackRun: $RUN_NAME，victim UID=$old_uid"
kubectl create -f "$RENDERED_DIR/40-repack-run.yaml" >/dev/null

log "等待 source victim 进入 Terminating 且 Eviction=Accepted，最长 ${RUN_TIMEOUT_SECONDS}s"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待 victim Terminating 且 Eviction=Accepted 超时"

  run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
  phase="$(printf '%s' "$run_json" | jq -r '.status.phase // ""')"
  eviction_phase="$(printf '%s' "$run_json" | jq -r --arg victim "$SOURCE_POD_NAME" '
    [.status.relocations[]? | select(.victimPodName == $victim)][0].eviction.phase // ""')"
  if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then
    fail "到达稳定暂停点前 RepackRun 已终态: phase=$phase reason=$(terminal_reason)"
  fi

  if pod_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json 2>/dev/null)"; then
    current_uid="$(printf '%s' "$pod_json" | jq -r '.metadata.uid')"
    deletion_timestamp="$(printf '%s' "$pod_json" | jq -r '.metadata.deletionTimestamp // ""')"
    [ "$current_uid" = "$old_uid" ] || fail "源 Pod 已在暂停 Engine 前被重建，本轮无法证明目标场景"
    if [ -n "$deletion_timestamp" ] && [ "$eviction_phase" = "Accepted" ]; then
      break
    fi
  else
    fail "源 Pod 在暂停 Engine 前已消失，本轮无法证明目标场景"
  fi
  sleep 1
done

run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
printf '%s' "$run_json" | jq -e --arg source "$SOURCE_NODE" --arg receiver "$RECEIVER_NODE" --argjson cards "$SOURCE_POD_CARDS" '
  (.status.plan.freedNodes == [$source]) and
  (.status.plan.summary.freedNodeCount == 1) and
  (.status.plan.summary.movedCardCount == $cards) and
  (any(.status.plan.moves[]?.pods[]?; .fromNode == $source and .toNode == $receiver and .cards == $cards))
' >/dev/null || fail "Repack 计划不是预期的单 Pod $SOURCE_NODE -> $RECEIVER_NODE"

engine_deployment="$(engine_deployment_name)"
kubectl -n "$SYSTEM_NAMESPACE" logs "deployment/$engine_deployment" --all-containers --tail=500 \
  >"$evidence_dir/repack-engine-before-pause.log" 2>&1 || true
pause_repack_engine

run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
phase="$(printf '%s' "$run_json" | jq -r '.status.phase // ""')"
[ "$phase" != "Succeeded" ] && [ "$phase" != "Failed" ] || fail "Engine 暂停前 Run 已终态: $phase"

log "等待旧 victim 删除、StatefulSet replacement 被 placement gate 拦住"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
replacement_uid=""
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待被 gate 拦住的 replacement 超时"

  if replacement_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json 2>/dev/null)"; then
    current_uid="$(printf '%s' "$replacement_json" | jq -r '.metadata.uid')"
    replacement_node="$(printf '%s' "$replacement_json" | jq -r '.spec.nodeName // ""')"
    has_gate="$(printf '%s' "$replacement_json" | jq -r 'any(.spec.schedulingGates[]?; .name == "repack.volcano.sh/placement")')"
    run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
    placement_phase="$(printf '%s' "$run_json" | jq -r --arg victim "$SOURCE_POD_NAME" '
      [.status.relocations[]? | select(.victimPodName == $victim)][0].placement.phase // ""')"
    recorded_replacement_uid="$(printf '%s' "$run_json" | jq -r --arg victim "$SOURCE_POD_NAME" '
      [.status.relocations[]? | select(.victimPodName == $victim)][0].placement.replacementPodUID // ""')"
    selected_node="$(printf '%s' "$run_json" | jq -r --arg victim "$SOURCE_POD_NAME" '
      [.status.relocations[]? | select(.victimPodName == $victim)][0].placement.selectedNodeName // ""')"

    if [ "$current_uid" != "$old_uid" ] && [ "$has_gate" = "true" ] && [ -z "$replacement_node" ] &&
      [ "$placement_phase" = "WaitingForNodeSelection" ] && [ "$recorded_replacement_uid" = "$current_uid" ] &&
      [ -z "$selected_node" ]; then
      replacement_uid="$current_uid"
      break
    fi
  fi
  sleep 2
done

assert_repack_engine_paused
[ -z "$(target_consumers_on_node "$SOURCE_NODE")" ] || fail "提交新作业前 $SOURCE_NODE 仍有目标资源消费者"
log "replacement UID=$replacement_uid 已被稳定拦住；提交新用户 Volcano Job $REUSE_JOB_NAME"
kubectl create -f "$RENDERED_DIR/30-reuse-job.yaml" >/dev/null

log "等待新作业 Pod 由 Volcano Scheduler 绑定到已释放的 $SOURCE_NODE"
deadline=$(( $(date +%s) + POD_READY_TIMEOUT_SECONDS ))
reuse_pod_name=""
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待新作业 Pod Ready 超时"
  reuse_pod_name="$(single_pod_name_by_label 'repack-reuse-test=concurrent-user')"
  if [ -n "$reuse_pod_name" ]; then
    reuse_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$reuse_pod_name" -o json)"
    reuse_ready="$(printf '%s' "$reuse_json" | jq -r 'any(.status.conditions[]?; .type == "Ready" and .status == "True")')"
    reuse_node="$(printf '%s' "$reuse_json" | jq -r '.spec.nodeName // ""')"
    if [ "$reuse_ready" = "true" ] && [ -n "$reuse_node" ]; then
      break
    fi
  fi
  sleep 2
done

reuse_uid="$(pod_uid "$reuse_pod_name")"
[ "$reuse_node" = "$SOURCE_NODE" ] || fail "新作业 Pod 落到 $reuse_node，预期 $SOURCE_NODE"
[ "$(pod_target_request "$reuse_pod_name")" -eq "$REUSE_JOB_CARDS" ] || fail "新作业 Pod 未申请 $REUSE_JOB_CARDS 张目标卡"
[ "$reuse_uid" != "$replacement_uid" ] || fail "新作业 Pod 与 Repack replacement UID 意外相同"
[ "$(target_consumers_on_node "$SOURCE_NODE")" = "$TEST_NAMESPACE/$reuse_pod_name" ] ||
  fail "$SOURCE_NODE 上的目标资源消费者不是唯一的新作业 Pod"
assert_repack_engine_paused

run_json="$(kubectl get repackrun "$RUN_NAME" -o json)"
phase="$(printf '%s' "$run_json" | jq -r '.status.phase // ""')"
[ "$phase" != "Succeeded" ] && [ "$phase" != "Failed" ] || fail "恢复 Engine 前 RepackRun 已意外终态: $phase"

log "新作业 Pod $reuse_pod_name (UID=$reuse_uid) 已复用 $SOURCE_NODE；恢复 Repack Engine"
restore_repack_engine || fail "Repack Engine 恢复失败，请立即手工检查 Deployment"

log "等待 Repack replacement Ready 并落到计划接收节点 $RECEIVER_NODE"
deadline=$(( $(date +%s) + RUN_TIMEOUT_SECONDS ))
while true; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "等待 Repack replacement Ready 超时"
  replacement_json="$(kubectl -n "$TEST_NAMESPACE" get pod "$SOURCE_POD_NAME" -o json)"
  current_uid="$(printf '%s' "$replacement_json" | jq -r '.metadata.uid')"
  replacement_ready="$(printf '%s' "$replacement_json" | jq -r 'any(.status.conditions[]?; .type == "Ready" and .status == "True")')"
  replacement_node="$(printf '%s' "$replacement_json" | jq -r '.spec.nodeName // ""')"
  if [ "$current_uid" = "$replacement_uid" ] && [ "$replacement_ready" = "true" ] && [ -n "$replacement_node" ]; then
    break
  fi
  sleep 2
done
[ "$replacement_node" = "$RECEIVER_NODE" ] || fail "Repack replacement 落到 $replacement_node，预期 $RECEIVER_NODE"
[ "$(pod_target_request "$SOURCE_POD_NAME")" -eq "$SOURCE_POD_CARDS" ] || fail "Repack replacement 的目标资源请求已变化"

log "replacement 已完成绑定，等待 RepackRun 终态"
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
message="$(printf '%s' "$run_json" | jq -r '.status.message // ""')"
relocation_json="$(printf '%s' "$run_json" | jq -c --arg victim "$SOURCE_POD_NAME" '[.status.relocations[]? | select(.victimPodName == $victim)][0] // {}')"

[ "$phase" = "Succeeded" ] || fail "RepackRun 失败: phase=$phase reason=$reason message=$message"
[ "$reason" = "ExecutionCompleted" ] || fail "终态 reason=$reason，预期 ExecutionCompleted"

printf '%s' "$run_json" | jq -e --arg source "$SOURCE_NODE" --arg receiver "$RECEIVER_NODE" --argjson cards "$SOURCE_POD_CARDS" '
  (.status.plan.freedNodes == [$source]) and
  (.status.plan.summary.freedNodeCount == 1) and
  (.status.plan.summary.movedCardCount == $cards) and
  (any(.status.plan.moves[]?.pods[]?; .fromNode == $source and .toNode == $receiver and .cards == $cards)) and
  (.status.result.metricsVerified == true) and
  (((.status.result.freedNodes // []) | length) == 0) and
  (.status.result.freedNodeCount == 0) and
  (.status.result.movedCardCount == $cards)
' >/dev/null || fail "plan/result 不符合“计划释放 N0，终态 N0 已被复用”的预期"

printf '%s' "$relocation_json" | jq -e \
  --arg receiver "$RECEIVER_NODE" \
  --arg oldUID "$old_uid" \
  --arg newUID "$replacement_uid" '
    (.victimPodUID == $oldUID) and
    (.plannedNodeName == $receiver) and
    (.eviction.phase == "Accepted") and
    (.placement.phase == "Placed") and
    (.placement.selectedNodeName == $receiver) and
    (.placement.actualNodeName == $receiver) and
    (.placement.replacementPodUID == $newUID)
  ' >/dev/null || fail "relocation 记录不符合 replacement 按计划落到 N1 的预期"

case "$message" in
  *"already reused by unrelated workloads"*"$SOURCE_NODE"*) ;;
  *) fail "status.message 未明确记录 $SOURCE_NODE 被无关作业复用: $message" ;;
esac

[ "$(pod_uid "$reuse_pod_name")" = "$reuse_uid" ] || fail "新作业 Pod UID 在终态前变化"
[ "$(pod_node "$reuse_pod_name")" = "$SOURCE_NODE" ] || fail "新作业 Pod 在终态时不在 $SOURCE_NODE"
[ "$(target_consumers_on_node "$SOURCE_NODE")" = "$TEST_NAMESPACE/$reuse_pod_name" ] ||
  fail "终态时 $SOURCE_NODE 上的目标资源消费者不符合预期"

collect_evidence "$evidence_dir"
remove_taint_from_all_nodes "$RECEIVER_HOLD_TAINT_KEY"
trap - EXIT

log "PASS: 已释放节点被新用户作业复用时，RepackRun 仍成功"
printf 'phase=%s\nreason=%s\nplannedFreedNodes=%s\nterminalCurrentlyFreeNodes=[]\nreusedNode=%s\nreusePod=%s\nreplacementNode=%s\nevidence=%s\n' \
  "$phase" "$reason" "$SOURCE_NODE" "$SOURCE_NODE" "$TEST_NAMESPACE/$reuse_pod_name" "$replacement_node" "$evidence_dir"
printf '\n资源保留以便检查；确认后执行 ./cleanup.sh\n'
