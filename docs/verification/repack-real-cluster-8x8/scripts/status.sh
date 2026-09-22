#!/usr/bin/env bash

set -Eeuo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

require_command kubectl
require_command jq
assert_config

log "fixture Pods"
kubectl get pods -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true

log "managed RepackRuns"
kubectl get repackruns.repack.volcano.sh -l "$(selector)" 2>/dev/null || true

for name in $(kubectl get repackruns.repack.volcano.sh -l "$(selector)" -o json 2>/dev/null | jq -r '.items[].metadata.name'); do
  print_run_summary "${name}"
done

log "managed RepackPolicies"
kubectl get repackpolicies.repack.volcano.sh -l "$(selector)" -o wide 2>/dev/null || true

log "recent namespace events"
kubectl get events -n "${TEST_NAMESPACE}" --sort-by=.lastTimestamp 2>/dev/null | tail -n 80 || true
