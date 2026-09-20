#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  echo "Usage: ./run-case.sh <1|2|3>" >&2
}

tier=${1:-}
case "$tier" in
  1|2|3) ;;
  *) usage; exit 2 ;;
esac

validate_local_config
wait_for_hypernodes

case_name="hta${tier}"
job_name="partition-migrate-${case_name}"
evidence_dir=$(new_evidence_dir "$case_name")
marker="${STATE_DIR}/${case_name}-n0-cordoned"
mkdir -p "$STATE_DIR"

if kubectl get jobs.batch.volcano.sh "$job_name" \
  -n "$JOB_NAMESPACE" >/dev/null 2>&1; then
  die "$JOB_NAMESPACE/$job_name already exists; run cleanup.sh or inspect it manually"
fi

if [[ "$(kubectl get node "$N0" -o jsonpath='{.spec.unschedulable}')" == "true" ]]; then
  die "$N0 was already cordoned before the test; refusing to alter its state"
fi

on_exit() {
  local rc=$?
  trap - EXIT
  if [[ -f "$marker" ]]; then
    kubectl uncordon "$N0" >/dev/null 2>&1 || true
    rm -f "$marker"
  fi
  if (( rc != 0 )); then
    echo "FAILED: case $case_name; resources are retained for diagnosis" >&2
    echo "Evidence directory: $evidence_dir" >&2
  fi
  exit "$rc"
}
trap on_exit EXIT

echo "Running highestTierAllowed=$tier"
echo "Evidence directory: $evidence_dir"

set_receivers "$case_name" "$N0" "$N1" "$N6" "$N7"
rendered_job="${evidence_dir}/${job_name}.yaml"
render_job "$case_name" "$tier" "$rendered_job"

kubectl apply --dry-run=server -f "$rendered_job" >/dev/null
kubectl apply -f "$rendered_job" >/dev/null
wait_case_ready "$case_name"

kubectl get pod -n "$JOB_NAMESPACE" \
  -l "test.volcano.sh/case=${case_name}" \
  -L volcano.sh/partition-id -o wide \
  | tee "${evidence_dir}/pods-before.txt"

pod_json=$(kubectl get pod -n "$JOB_NAMESPACE" \
  -l "test.volcano.sh/case=${case_name}" -o json)

fail_pod=$(jq -r --arg node "$N0" \
  '[.items[] | select(.spec.nodeName == $node) | .metadata.name] | if length == 1 then .[0] else "" end' \
  <<<"$pod_json")
[[ -n "$fail_pod" ]] || die "expected exactly one test Pod on $N0"

fault_part=$(jq -r --arg pod "$fail_pod" \
  '.items[] | select(.metadata.name == $pod) | .metadata.labels["volcano.sh/partition-id"]' \
  <<<"$pod_json")

control_part=$(jq -r --arg part "$fault_part" '
  [.items[].metadata.labels["volcano.sh/partition-id"]]
  | unique
  | map(select(. != $part))
  | if length == 1 then .[0] else "" end
  ' <<<"$pod_json")
[[ -n "$fault_part" && -n "$control_part" ]] || die "failed to identify fault/control partitions"

echo "Fault Pod: $fail_pod"
echo "Fault partition: $fault_part"
echo "Control partition: $control_part"

assert_partition_nodes "$case_name" "$fault_part" "$N0" "$N1"
assert_partition_nodes "$case_name" "$control_part" "$N6" "$N7"

fault_before="${evidence_dir}/fault-before.uid"
control_before="${evidence_dir}/control-before.uid"
uids_for_partition "$case_name" "$fault_part" > "$fault_before"
uids_for_partition "$case_name" "$control_part" > "$control_before"

case "$tier" in
  1)
    invalid_a=$N1
    invalid_b=$N2
    valid_a=$N2
    valid_b=$N3
    ;;
  2)
    invalid_a=$N1
    invalid_b=$N4
    valid_a=$N1
    valid_b=$N2
    ;;
  3)
    invalid_a=""
    invalid_b=""
    valid_a=$N1
    valid_b=$N4
    ;;
esac

if [[ -n "$invalid_a" ]]; then
  echo "Setting intentionally invalid receivers: $invalid_a $invalid_b"
  set_receivers "$case_name" "$invalid_a" "$invalid_b"
else
  echo "Setting tier3 receivers: $valid_a $valid_b"
  set_receivers "$case_name" "$valid_a" "$valid_b"
fi

kubectl cordon "$N0" >/dev/null
touch "$marker"
kubectl exec -n "$JOB_NAMESPACE" "$fail_pod" -- touch /work/fail
wait_retry_count_one "$job_name"

if [[ -n "$invalid_a" ]]; then
  wait_fault_partition_recreated_pending \
    "$case_name" "$fault_part" "$fault_before" "$evidence_dir"

  kubectl get pod -n "$JOB_NAMESPACE" \
    -l "test.volcano.sh/case=${case_name},volcano.sh/partition-id=${fault_part}" \
    -o wide | tee "${evidence_dir}/invalid-receivers-pending.txt"

  uids_for_partition "$case_name" "$control_part" \
    > "${evidence_dir}/control-during-invalid.uid"
  assert_uid_files_equal "$control_before" \
    "${evidence_dir}/control-during-invalid.uid" \
    "${evidence_dir}/control-during-invalid.diff"

  echo "Negative boundary passed; setting valid receivers: $valid_a $valid_b"
  set_receivers "$case_name" "$valid_a" "$valid_b"
fi

wait_case_ready "$case_name"
assert_partition_nodes "$case_name" "$fault_part" "$valid_a" "$valid_b"
assert_partition_nodes "$case_name" "$control_part" "$N6" "$N7"

fault_after="${evidence_dir}/fault-after.uid"
control_after="${evidence_dir}/control-after.uid"
uids_for_partition "$case_name" "$fault_part" > "$fault_after"
uids_for_partition "$case_name" "$control_part" > "$control_after"

assert_uid_files_disjoint "$fault_before" "$fault_after" \
  "${evidence_dir}/fault-common.uid"
assert_uid_files_equal "$control_before" "$control_after" \
  "${evidence_dir}/control-after.diff"

retry_count=$(kubectl get jobs.batch.volcano.sh "$job_name" \
  -n "$JOB_NAMESPACE" -o jsonpath='{.status.retryCount}')
[[ "$retry_count" == "1" ]] || die "final retryCount=$retry_count, expected 1"

kubectl get pod -n "$JOB_NAMESPACE" \
  -l "test.volcano.sh/case=${case_name}" \
  -L volcano.sh/partition-id -o wide \
  | tee "${evidence_dir}/pods-after.txt"

kubectl get jobs.batch.volcano.sh "$job_name" -n "$JOB_NAMESPACE" -o yaml \
  > "${evidence_dir}/job-after.yaml"
kubectl describe jobs.batch.volcano.sh "$job_name" -n "$JOB_NAMESPACE" \
  > "${evidence_dir}/job-describe.txt"

grep -Eq 'ExecuteAction.*RestartPartition|RestartPartition.*ExecuteAction|Start to execute action RestartPartition' \
  "${evidence_dir}/job-describe.txt" || \
  die "Job Events do not contain the RestartPartition ExecuteAction evidence"

kubectl logs -n "$VOLCANO_NAMESPACE" \
  deployment/"${VOLCANO_RELEASE}-controllers" --since=20m \
  > "${evidence_dir}/controller.log" 2>&1 || true
kubectl logs -n "$VOLCANO_NAMESPACE" \
  deployment/"${VOLCANO_RELEASE}-scheduler" --since=20m \
  > "${evidence_dir}/scheduler.log" 2>&1 || true

cat > "${evidence_dir}/result.md" <<EOF
# ${case_name} verification result

- highestTierAllowed: ${tier}
- accelerator resource per Pod: ${ACCELERATOR_RESOURCE}=${CARDS_PER_POD}
- fault partition: ${fault_part}
- control partition: ${control_part}
- source nodes: ${N0}, ${N1}
- final fault-partition nodes: ${valid_a}, ${valid_b}
- control nodes: ${N6}, ${N7}
- retryCount: ${retry_count}
- RestartPartition ExecuteAction event: PASS
- fault partition old/new UIDs disjoint: PASS
- control partition UIDs unchanged: PASS
- result: PASS
EOF

kubectl delete jobs.batch.volcano.sh "$job_name" \
  -n "$JOB_NAMESPACE" --wait=true >/dev/null
clear_receiver_labels
kubectl uncordon "$N0" >/dev/null
rm -f "$marker"

echo "PASS: highestTierAllowed=$tier"
echo "Result: ${evidence_dir}/result.md"
