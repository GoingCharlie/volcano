#!/usr/bin/env bash

set -Eeuo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

preflight

log "selected nodes:"
printf '  %s\n' "${NODES[@]}"

log "Volcano/Repack related images:"
engine_namespace="$(repack_engine_json | jq -r '.items[0].metadata.namespace')"
kubectl get deployments.apps -n "${engine_namespace}" -o json | jq -r '
  .items[]
  | select(.metadata.name | test("controller|repack|scheduler|admission"))
  | "  \(.metadata.name): \([.spec.template.spec.containers[].image] | join(", ")) ready=\(.status.readyReplicas // 0)/\(.spec.replicas // 0)"
'
