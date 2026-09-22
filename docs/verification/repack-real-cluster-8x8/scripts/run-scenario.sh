#!/usr/bin/env bash

set -Eeuo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

usage() {
  cat <<'EOF'
Usage:
  run-scenario.sh dry-run
  run-scenario.sh success
  run-scenario.sh partial
  run-scenario.sh partial-policy

Scenarios:
  dry-run        Verify the expected one-node consolidation plan without eviction.
  success        Execute the plan without external node reuse; expect Succeeded.
  partial        Bind a new 8-card Pod to the planned freed node; expect
                 PartiallySucceeded with planned=1 and actual=0.
  partial-policy Run the same partial scenario from a RepackPolicy and also verify
                 lastSuccessfulTime/lastRunStatus accounting.
EOF
}

scenario="${1:-}"
case "${scenario}" in
  dry-run|success|partial|partial-policy) ;;
  *) usage; exit 2 ;;
esac

preflight

case "${scenario}" in
  dry-run)
    prepare_fixture false
    run_plan_probe
    print_run_summary "${PLAN_RUN}"
    log "PASS: DryRun produced the expected safe one-node consolidation plan"
    ;;

  success)
    prepare_fixture false
    run_plan_probe
    execute_run="$(run_name success)"
    create_run "${execute_run}" Execute
    wait_terminal "${execute_run}"
    assert_success "${execute_run}"
    print_run_summary "${execute_run}"
    log "PASS: all planned freed nodes were realized, phase=Succeeded"
    ;;

  partial)
    prepare_fixture true
    run_plan_probe
    create_blocker "${PLANNED_FREED_NODE}"
    execute_run="$(run_name partial)"
    create_run "${execute_run}" Execute
    wait_blocker_bound "${PLANNED_FREED_NODE}" "${execute_run}"
    wait_replacement_and_release_hold
    wait_terminal "${execute_run}"
    assert_partially_succeeded "${execute_run}"
    print_run_summary "${execute_run}"
    log "PASS: a new user Pod reused the planned node, phase=PartiallySucceeded"
    ;;

  partial-policy)
    prepare_fixture true
    run_plan_probe
    create_blocker "${PLANNED_FREED_NODE}"
    policy="$(run_name policy)"
    create_policy "${policy}"
    execute_run="$(wait_policy_run_and_suspend "${policy}")"
    wait_blocker_bound "${PLANNED_FREED_NODE}" "${execute_run}"
    wait_replacement_and_release_hold
    wait_terminal "${execute_run}"
    assert_partially_succeeded "${execute_run}"
    assert_policy_accounting "${policy}" "${execute_run}"
    print_run_summary "${execute_run}"
    kubectl get repackpolicies.repack.volcano.sh "${policy}" -o json | jq '{
      name: .metadata.name,
      suspend: .spec.suspend,
      successfulRunsHistoryLimit: .spec.successfulRunsHistoryLimit,
      lastSuccessfulTime: .status.lastSuccessfulTime,
      lastRunStatus: {
        name: .status.lastRunStatus.name,
        phase: .status.lastRunStatus.phase,
        completionTime: .status.lastRunStatus.completionTime
      }
    }'
    log "PASS: Policy treated PartiallySucceeded as successful"
    ;;
esac

log "resources are intentionally retained for inspection; run scripts/cleanup.sh when finished"
