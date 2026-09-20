#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

require_command kubectl
[[ -n "$N0" && "$N0" != replace-with-* ]] || \
  die "edit nodes.sh and replace every node placeholder before cleanup"

for case_name in hta1 hta2 hta3; do
  kubectl delete jobs.batch.volcano.sh "partition-migrate-${case_name}" \
    -n "$JOB_NAMESPACE" --ignore-not-found=true --wait=true
done

clear_receiver_labels

for marker in "${STATE_DIR}"/*-n0-cordoned; do
  [[ -e "$marker" ]] || continue
  kubectl uncordon "$N0" >/dev/null
  rm -f "$marker"
done

echo "Test Jobs and temporary receiver labels were removed."
echo "Topology labels and HyperNodes were retained for inspection."
