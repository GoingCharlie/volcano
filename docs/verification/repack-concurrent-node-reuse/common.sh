#!/usr/bin/env bash

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
source "$SCRIPT_DIR/config.sh"

MANIFEST_DIR="$SCRIPT_DIR/manifests"
RENDERED_DIR="$SCRIPT_DIR/.rendered"
EVIDENCE_ROOT="$SCRIPT_DIR/evidence"
STATE_DIR="$SCRIPT_DIR/.state"
ENGINE_DEPLOYMENT_STATE_FILE="$STATE_DIR/engine-deployment"
ENGINE_REPLICAS_STATE_FILE="$STATE_DIR/engine-replicas"
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

validate_positive_integer() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || fail "$name 必须是正整数，实际为 '$value'"
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

  validate_positive_integer EXPECTED_CARDS_PER_NODE "$EXPECTED_CARDS_PER_NODE"
  validate_positive_integer SOURCE_POD_CARDS "$SOURCE_POD_CARDS"
  validate_positive_integer RECEIVER_POD_CARDS "$RECEIVER_POD_CARDS"
  validate_positive_integer REUSE_JOB_CARDS "$REUSE_JOB_CARDS"
  [ $((SOURCE_POD_CARDS + RECEIVER_POD_CARDS)) -eq "$EXPECTED_CARDS_PER_NODE" ] ||
    fail "SOURCE_POD_CARDS + RECEIVER_POD_CARDS 必须恰好等于单节点卡数 $EXPECTED_CARDS_PER_NODE"
  [ "$REUSE_JOB_CARDS" -le "$EXPECTED_CARDS_PER_NODE" ] || fail "REUSE_JOB_CARDS 不能超过单节点卡数"
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
    -e "s|__REUSE_JOB_NAME__|$REUSE_JOB_NAME|g" \
    -e "s|__SCHEDULER_NAME__|$SCHEDULER_NAME|g" \
    -e "s|__QUEUE_NAME__|$QUEUE_NAME|g" \
    -e "s|__TEST_IMAGE__|$TEST_IMAGE|g" \
    -e "s|__TARGET_RESOURCE__|$TARGET_RESOURCE|g" \
    -e "s|__SOURCE_POD_CARDS__|$SOURCE_POD_CARDS|g" \
    -e "s|__RECEIVER_POD_CARDS__|$RECEIVER_POD_CARDS|g" \
    -e "s|__REUSE_JOB_CARDS__|$REUSE_JOB_CARDS|g" \
    -e "s|__SOURCE_NODE_LABEL_KEY__|$SOURCE_NODE_LABEL_KEY|g" \
    -e "s|__SOURCE_NODE_LABEL_VALUE__|$SOURCE_NODE_LABEL_VALUE|g" \
    -e "s|__RUN_NAME__|$RUN_NAME|g" \
    -e "s|__SOURCE_NODE__|$SOURCE_NODE|g" \
    "$input" >"$output"
}

render_all() {
  mkdir -p "$RENDERED_DIR"
  render_manifest "$MANIFEST_DIR/00-namespace.yaml" "$RENDERED_DIR/00-namespace.yaml"
  render_manifest "$MANIFEST_DIR/10-source.yaml" "$RENDERED_DIR/10-source.yaml"
  render_manifest "$MANIFEST_DIR/20-receiver.yaml" "$RENDERED_DIR/20-receiver.yaml"
  render_manifest "$MANIFEST_DIR/30-reuse-job.yaml" "$RENDERED_DIR/30-reuse-job.yaml"
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

pod_uid() {
  kubectl -n "$TEST_NAMESPACE" get pod "$1" -o json | jq -r '.metadata.uid // ""'
}

pod_target_request() {
  kubectl -n "$TEST_NAMESPACE" get pod "$1" -o json | jq -r --arg resource "$TARGET_RESOURCE" \
    '[.spec.containers[]?
      | [((.resources.requests[$resource] // "0") | tonumber),
         ((.resources.limits[$resource] // "0") | tonumber)]
      | max
    ] | add // 0'
}

single_pod_name_by_label() {
  local selector="$1"
  kubectl -n "$TEST_NAMESPACE" get pods -l "$selector" -o json | jq -r '
    if (.items | length) == 1 then .items[0].metadata.name else "" end'
}

wait_for_one_source_podgroup() {
  local deadline count
  deadline=$(( $(date +%s) + POD_READY_TIMEOUT_SECONDS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    count="$(kubectl -n "$TEST_NAMESPACE" get podgroup -l repack-reuse-test=source -o json | jq '.items | length')"
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
    ] | sort | join(",")'
}

terminal_reason() {
  kubectl get repackrun "$RUN_NAME" -o json | jq -r '
    [.status.conditions[]?
      | select(.status == "True" and (.type == "Complete" or .type == "Failed"))
      | .reason
    ][-1] // ""'
}

engine_deployment_name() {
  kubectl -n "$SYSTEM_NAMESPACE" get deployment -l "$ENGINE_LABEL_SELECTOR" -o json | jq -r '
    if (.items | length) == 1 then .items[0].metadata.name else "" end'
}

pause_repack_engine() {
  local deployment replicas deadline pod_count
  [ ! -e "$ENGINE_DEPLOYMENT_STATE_FILE" ] && [ ! -e "$ENGINE_REPLICAS_STATE_FILE" ] ||
    fail "检测到未恢复的 Engine 状态，请先执行 ./cleanup.sh"

  deployment="$(engine_deployment_name)"
  [ -n "$deployment" ] || fail "无法唯一定位 Repack Engine Deployment"
  replicas="$(kubectl -n "$SYSTEM_NAMESPACE" get deployment "$deployment" -o json | jq -r '.spec.replicas // 1')"
  [[ "$replicas" =~ ^[1-9][0-9]*$ ]] || fail "Repack Engine 原副本数无效: $replicas"

  mkdir -p "$STATE_DIR"
  printf '%s\n' "$deployment" >"$ENGINE_DEPLOYMENT_STATE_FILE"
  printf '%s\n' "$replicas" >"$ENGINE_REPLICAS_STATE_FILE"

  log "暂停 Repack Engine Deployment $deployment，记录原副本数 $replicas"
  kubectl -n "$SYSTEM_NAMESPACE" scale "deployment/$deployment" --replicas=0 >/dev/null
  deadline=$(( $(date +%s) + ENGINE_ROLLOUT_TIMEOUT_SECONDS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    pod_count="$(kubectl -n "$SYSTEM_NAMESPACE" get pods -l "$ENGINE_LABEL_SELECTOR" -o json | jq '.items | length')"
    if [ "$pod_count" -eq 0 ]; then
      log "Repack Engine 已暂停"
      return 0
    fi
    sleep 1
  done
  fail "等待 Repack Engine Pod 缩容到 0 超时"
}

assert_repack_engine_paused() {
  local deployment desired_replicas pod_count
  [ -s "$ENGINE_DEPLOYMENT_STATE_FILE" ] || fail "缺少 Engine Deployment 恢复状态文件"
  IFS= read -r deployment <"$ENGINE_DEPLOYMENT_STATE_FILE"
  desired_replicas="$(kubectl -n "$SYSTEM_NAMESPACE" get deployment "$deployment" -o json | jq -r '.spec.replicas // 0')"
  pod_count="$(kubectl -n "$SYSTEM_NAMESPACE" get pods -l "$ENGINE_LABEL_SELECTOR" -o json | jq '.items | length')"
  [ "$desired_replicas" -eq 0 ] && [ "$pod_count" -eq 0 ] ||
    fail "Repack Engine 在测试检查点前被外部系统恢复: desiredReplicas=$desired_replicas, pods=$pod_count"
}

restore_repack_engine() {
  local deployment replicas
  if [ ! -e "$ENGINE_DEPLOYMENT_STATE_FILE" ] && [ ! -e "$ENGINE_REPLICAS_STATE_FILE" ]; then
    return 0
  fi
  if [ ! -s "$ENGINE_DEPLOYMENT_STATE_FILE" ] || [ ! -s "$ENGINE_REPLICAS_STATE_FILE" ]; then
    log "ERROR: Engine 恢复状态文件不完整: $STATE_DIR"
    return 1
  fi

  IFS= read -r deployment <"$ENGINE_DEPLOYMENT_STATE_FILE"
  IFS= read -r replicas <"$ENGINE_REPLICAS_STATE_FILE"
  if [ -z "$deployment" ] || ! [[ "$replicas" =~ ^[1-9][0-9]*$ ]]; then
    log "ERROR: Engine 恢复状态无效: deployment='$deployment', replicas='$replicas'"
    return 1
  fi

  log "恢复 Repack Engine Deployment $deployment 到 $replicas 副本"
  kubectl -n "$SYSTEM_NAMESPACE" scale "deployment/$deployment" --replicas="$replicas" >/dev/null || return 1
  kubectl -n "$SYSTEM_NAMESPACE" rollout status "deployment/$deployment" \
    --timeout="${ENGINE_ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null || return 1
  rm -f "$ENGINE_DEPLOYMENT_STATE_FILE" "$ENGINE_REPLICAS_STATE_FILE"
  rmdir "$STATE_DIR" >/dev/null 2>&1 || true
  log "Repack Engine 已恢复"
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
  kubectl -n "$TEST_NAMESPACE" get job.batch.volcano.sh "$REUSE_JOB_NAME" -o yaml >"$evidence_dir/reuse-vcjob.yaml" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"$evidence_dir/events.txt" 2>&1 || true

  engine_deployment="$(engine_deployment_name 2>/dev/null || true)"
  if [ -n "$engine_deployment" ]; then
    kubectl -n "$SYSTEM_NAMESPACE" logs "deployment/$engine_deployment" --all-containers --tail=500 >"$evidence_dir/repack-engine.log" 2>&1 || true
    kubectl -n "$SYSTEM_NAMESPACE" get deployment "$engine_deployment" -o yaml >"$evidence_dir/repack-engine-deployment.yaml" 2>&1 || true
  fi

  kubectl -n "$SYSTEM_NAMESPACE" logs -l "$CONTROLLER_LABEL_SELECTOR" --all-containers --tail=500 --prefix >"$evidence_dir/volcano-controller.log" 2>&1 || true
  kubectl -n "$SYSTEM_NAMESPACE" logs -l "$SCHEDULER_LABEL_SELECTOR" --all-containers --tail=500 --prefix >"$evidence_dir/volcano-scheduler.log" 2>&1 || true
  kubectl -n "$SYSTEM_NAMESPACE" logs -l "$ADMISSION_LABEL_SELECTOR" --all-containers --tail=500 --prefix >"$evidence_dir/volcano-admission.log" 2>&1 || true
  cp "$RENDERED_DIR"/*.yaml "$evidence_dir/" 2>/dev/null || true
}
