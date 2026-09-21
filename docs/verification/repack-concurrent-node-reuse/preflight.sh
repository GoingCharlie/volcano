#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

require_command kubectl
require_command jq
require_command sed
require_command sort
validate_config
verify_context
render_all

[ ! -e "$ENGINE_DEPLOYMENT_STATE_FILE" ] && [ ! -e "$ENGINE_REPLICAS_STATE_FILE" ] ||
  fail "检测到上次测试未恢复的 Engine 状态，请先执行 ./cleanup.sh"

log "检查 Repack/Volcano CRD、组件和操作权限"
kubectl get crd repackruns.repack.volcano.sh >/dev/null
kubectl get crd podgroups.scheduling.volcano.sh >/dev/null
kubectl get crd jobs.batch.volcano.sh >/dev/null

queue_state="$(kubectl get queue "$QUEUE_NAME" -o json | jq -r '.status.state // ""')"
[ "$queue_state" = "Open" ] || fail "Volcano Queue $QUEUE_NAME 不是 Open，实际状态 '$queue_state'"

engine_json="$(kubectl -n "$SYSTEM_NAMESPACE" get deployment -l "$ENGINE_LABEL_SELECTOR" -o json)"
engine_count="$(printf '%s' "$engine_json" | jq '.items | length')"
[ "$engine_count" -eq 1 ] || fail "$SYSTEM_NAMESPACE 中 $ENGINE_LABEL_SELECTOR 必须恰好对应 1 个 Deployment，实际 $engine_count"

engine_name="$(printf '%s' "$engine_json" | jq -r '.items[0].metadata.name')"
engine_replicas="$(printf '%s' "$engine_json" | jq -r '.items[0].spec.replicas // 1')"
engine_image="$(printf '%s' "$engine_json" | jq -r '[.items[0].spec.template.spec.containers[] | select(.name | contains("repack-engine"))][0].image // ""')"
[ -n "$engine_image" ] || fail "Deployment $engine_name 中未找到 repack-engine 容器"
[ "$engine_image" = "$EXPECTED_ENGINE_IMAGE" ] || fail "Engine 实际镜像 '$engine_image' 不等于预期修复镜像 '$EXPECTED_ENGINE_IMAGE'"
[[ "$engine_replicas" =~ ^[1-9][0-9]*$ ]] || fail "Engine 必须至少有 1 个副本，实际 $engine_replicas"
kubectl -n "$SYSTEM_NAMESPACE" rollout status "deployment/$engine_name" --timeout=120s >/dev/null

controller_ready="$(kubectl -n "$SYSTEM_NAMESPACE" get pod -l "$CONTROLLER_LABEL_SELECTOR" -o json | jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length')"
scheduler_ready="$(kubectl -n "$SYSTEM_NAMESPACE" get pod -l "$SCHEDULER_LABEL_SELECTOR" -o json | jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length')"
admission_ready="$(kubectl -n "$SYSTEM_NAMESPACE" get pod -l "$ADMISSION_LABEL_SELECTOR" -o json | jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length')"
[ "$controller_ready" -ge 1 ] || fail "未找到 Ready 的 volcano-controller Pod"
[ "$scheduler_ready" -ge 1 ] || fail "未找到 Ready 的 volcano-scheduler Pod"
[ "$admission_ready" -ge 1 ] || fail "未找到 Ready 的 volcano-admission Pod"

pod_mutate_webhooks="$(kubectl get mutatingwebhookconfigurations -o json | jq '[.items[].webhooks[]? | select(.clientConfig.service.path == "/pods/mutate")] | length')"
[ "$pod_mutate_webhooks" -ge 1 ] || fail "未找到 /pods/mutate MutatingWebhook，replacement 无法被 placement gate 稳定拦住"

[ "$(kubectl auth can-i patch nodes)" = "yes" ] || fail "当前账号无权 patch Node 标签/污点"
[ "$(kubectl auth can-i update deployments.apps/scale -n "$SYSTEM_NAMESPACE")" = "yes" ] || fail "当前账号无权暂停和恢复 Repack Engine Deployment"
[ "$(kubectl auth can-i create namespaces)" = "yes" ] || fail "当前账号无权创建 Namespace"
[ "$(kubectl auth can-i create repackruns.repack.volcano.sh)" = "yes" ] || fail "当前账号无权创建 RepackRun"
[ "$(kubectl auth can-i create jobs.batch.volcano.sh -n "$TEST_NAMESPACE")" = "yes" ] || fail "当前账号无权创建 Volcano Job"

log "检查集群节点集合、Ready 状态、每节点 8 卡和空闲状态"
actual_node_count="$(kubectl get nodes -o json | jq '.items | length')"
[ "$actual_node_count" -eq 8 ] || fail "集群必须恰好有 8 个节点，实际 $actual_node_count"

for node in "${NODES[@]}"; do
  node_json="$(kubectl get node "$node" -o json)" || fail "节点不存在: $node"
  ready="$(printf '%s' "$node_json" | jq -r '[.status.conditions[] | select(.type == "Ready")][0].status // "Unknown"')"
  unschedulable="$(printf '%s' "$node_json" | jq -r '.spec.unschedulable // false')"
  capacity="$(printf '%s' "$node_json" | jq -r --arg resource "$TARGET_RESOURCE" '.status.capacity[$resource] // "0"')"
  allocatable="$(printf '%s' "$node_json" | jq -r --arg resource "$TARGET_RESOURCE" '.status.allocatable[$resource] // "0"')"
  has_test_taint="$(printf '%s' "$node_json" | jq --arg first "$INITIAL_TAINT_KEY" --arg second "$RECEIVER_HOLD_TAINT_KEY" 'any(.spec.taints[]?; .key == $first or .key == $second)')"
  has_test_label="$(printf '%s' "$node_json" | jq --arg key "$SOURCE_NODE_LABEL_KEY" '.metadata.labels[$key] != null')"
  consumers="$(target_consumers_on_node "$node")"

  [ "$ready" = "True" ] || fail "$node 不是 Ready"
  [ "$unschedulable" = "false" ] || fail "$node 已被 cordon"
  [ "$capacity" = "$EXPECTED_CARDS_PER_NODE" ] || fail "$node 的 $TARGET_RESOURCE capacity=$capacity，预期 $EXPECTED_CARDS_PER_NODE"
  [ "$allocatable" = "$EXPECTED_CARDS_PER_NODE" ] || fail "$node 的 $TARGET_RESOURCE allocatable=$allocatable，预期 $EXPECTED_CARDS_PER_NODE"
  [ "$has_test_taint" = "false" ] || fail "$node 残留了测试污点，请先执行 ./cleanup.sh"
  [ "$has_test_label" = "false" ] || fail "$node 残留了测试标签，请先执行 ./cleanup.sh"
  [ -z "$consumers" ] || fail "$node 上仍有 $TARGET_RESOURCE 消费者: $consumers"
  log "PASS $node: Ready, schedulable, $TARGET_RESOURCE=$allocatable, idle"
done

configured_matches=0
while IFS= read -r actual_node; do
  for configured_node in "${NODES[@]}"; do
    if [ "$actual_node" = "$configured_node" ]; then
      configured_matches=$((configured_matches + 1))
      break
    fi
  done
done < <(kubectl get nodes -o json | jq -r '.items[].metadata.name')
[ "$configured_matches" -eq 8 ] || fail "config.sh 的 NODES 与集群实际 8 节点不完全一致"

active_runs="$(kubectl get repackrun -o json | jq -r '[.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed") | .metadata.name] | join(",")')"
[ -z "$active_runs" ] || fail "集群存在活动 RepackRun: $active_runs"
if kubectl get crd repackpolicies.repack.volcano.sh >/dev/null 2>&1; then
  active_policies="$(kubectl get repackpolicy -o json | jq -r '[.items[] | select(.spec.suspend != true) | .metadata.name] | join(",")')"
  [ -z "$active_policies" ] || fail "存在未 suspend 的 RepackPolicy: $active_policies"
fi
kubectl get namespace "$TEST_NAMESPACE" >/dev/null 2>&1 && fail "测试 namespace $TEST_NAMESPACE 已存在，请先执行 ./cleanup.sh"
kubectl get repackrun "$RUN_NAME" >/dev/null 2>&1 && fail "RepackRun $RUN_NAME 已存在，请先执行 ./cleanup.sh"

log "检查渲染后 YAML"
for manifest in "$RENDERED_DIR"/*.yaml; do
  kubectl apply --dry-run=client -f "$manifest" >/dev/null
done

log "预检通过"
printf 'Context: %s\nEngine: %s (%s), replicas=%s\nSource/reused node: %s\nReceiver: %s\n' \
  "$EXPECTED_KUBE_CONTEXT" "$engine_name" "$engine_image" "$engine_replicas" "$SOURCE_NODE" "$RECEIVER_NODE"
