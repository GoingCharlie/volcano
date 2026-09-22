#!/usr/bin/env bash

set -Eeuo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

require_command kubectl
require_command jq
assert_config

current_context="$(kubectl config current-context)"
[[ "${current_context}" == "${EXPECTED_CONTEXT}" ]] || die "kubectl context is ${current_context}, expected ${EXPECTED_CONTEXT}"
cleanup_managed
