#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'EOF'
Usage:
  ./prepare-topology.sh
  ./prepare-topology.sh --apply-controller-config

Without the flag, the script labels and validates nodes but does not overwrite
volcano-controller.conf. Use the flag only when controller-topology.yaml may
replace the current controller config; the previous ConfigMap is backed up.
EOF
}

apply_controller_config=false
case "${1:-}" in
  "") ;;
  --apply-controller-config) apply_controller_config=true ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

validate_local_config
evidence_dir=$(new_evidence_dir setup)
echo "Evidence directory: $evidence_dir"

kubectl version -o yaml > "${evidence_dir}/kubernetes-version.yaml"
kubectl get nodes -o wide > "${evidence_dir}/nodes-before.txt"

kubectl get configmap "${VOLCANO_RELEASE}-scheduler-configmap" \
  -n "$VOLCANO_NAMESPACE" -o yaml \
  > "${evidence_dir}/scheduler-configmap-before.yaml"

kubectl get configmap "${VOLCANO_RELEASE}-controller-configmap" \
  -n "$VOLCANO_NAMESPACE" -o yaml \
  > "${evidence_dir}/controller-configmap-before.yaml"

scheduler_config=$(kubectl get configmap "${VOLCANO_RELEASE}-scheduler-configmap" \
  -n "$VOLCANO_NAMESPACE" \
  -o jsonpath='{.data.volcano-scheduler\.conf}')

for plugin in gang predicates network-topology-aware; do
  grep -Eq "name:[[:space:]]*${plugin}([[:space:]]|$)" <<<"$scheduler_config" || \
    die "$plugin is not enabled in the scheduler ConfigMap"
done

kubectl label node "$N0" "$N1" \
  topology.volcano.sh/tier1=leaf-a \
  topology.volcano.sh/tier2=spine-a \
  topology.volcano.sh/tier3=fabric-a --overwrite >/dev/null

kubectl label node "$N2" "$N3" \
  topology.volcano.sh/tier1=leaf-b \
  topology.volcano.sh/tier2=spine-a \
  topology.volcano.sh/tier3=fabric-a --overwrite >/dev/null

kubectl label node "$N4" "$N5" \
  topology.volcano.sh/tier1=leaf-c \
  topology.volcano.sh/tier2=spine-b \
  topology.volcano.sh/tier3=fabric-a --overwrite >/dev/null

kubectl label node "$N6" "$N7" \
  topology.volcano.sh/tier1=leaf-d \
  topology.volcano.sh/tier2=spine-b \
  topology.volcano.sh/tier3=fabric-a --overwrite >/dev/null

kubectl get nodes "${NODES[@]}" \
  -L topology.volcano.sh/tier1 \
  -L topology.volcano.sh/tier2 \
  -L topology.volcano.sh/tier3 \
  -o wide | tee "${evidence_dir}/nodes-with-topology-labels.txt"

kubectl get nodes "${NODES[@]}" -o json | jq -r \
  --arg resource "$ACCELERATOR_RESOURCE" '
    .items[]
    | [.metadata.name, (.status.capacity[$resource] // "0"), (.status.allocatable[$resource] // "0")]
    | @tsv
  ' | {
    printf 'NODE\tCAPACITY(%s)\tALLOCATABLE(%s)\n' "$ACCELERATOR_RESOURCE" "$ACCELERATOR_RESOURCE"
    cat
  } | tee "${evidence_dir}/ascend-capacity.txt"

if $apply_controller_config; then
  rendered="${evidence_dir}/controller-topology.yaml"
  sed \
    -e "s|__VOLCANO_RELEASE__|${VOLCANO_RELEASE}|g" \
    -e "s|__VOLCANO_NAMESPACE__|${VOLCANO_NAMESPACE}|g" \
    "${SCRIPT_DIR}/controller-topology.yaml" > "$rendered"

  kubectl apply -f "$rendered"
  kubectl rollout restart deployment/"${VOLCANO_RELEASE}-controllers" \
    -n "$VOLCANO_NAMESPACE"
  kubectl rollout status deployment/"${VOLCANO_RELEASE}-controllers" \
    -n "$VOLCANO_NAMESPACE" --timeout=180s
fi

wait_for_hypernodes

kubectl get hypernodes -o json | jq -r \
  --arg prefix "$TOPOLOGY_PREFIX" '
    .items[]
    | select(.metadata.name | startswith($prefix))
    | [
        .metadata.name,
        ("tier=" + (.spec.tier | tostring)),
        ([.spec.members[].selector.exactMatch.name] | join(","))
      ]
    | @tsv
  ' | sort | tee "${evidence_dir}/hypernodes.txt"

kubectl get hypernodes -o json | jq -e \
  --arg prefix "$TOPOLOGY_PREFIX" '
    [.items[] | select(.metadata.name | startswith($prefix)) | .spec.tier]
    | (map(select(. == 1)) | length) == 4
      and (map(select(. == 2)) | length) == 2
      and (map(select(. == 3)) | length) == 1
  ' >/dev/null || die "HyperNode tier counts are not 4/2/1"

echo "PASS: topology and ${ACCELERATOR_RESOURCE} capacity checks completed"
