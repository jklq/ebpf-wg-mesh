#!/usr/bin/env bash
set -euo pipefail

CONTROLPLANE_BIN=${CONTROLPLANE_BIN:-/opt/ebpf-wg-mesh/controlplane}
AGENT_BOOTSTRAP_TOKENS=${AGENT_BOOTSTRAP_TOKENS:?AGENT_BOOTSTRAP_TOKENS must contain comma-separated agent_id=token bindings}

state_dir=$(mktemp -d /tmp/ebpf-wg-mesh-replica-local-state.XXXXXX)
trap 'rm -rf "${state_dir}"' EXIT

set +e
output=$(timeout 20s "${CONTROLPLANE_BIN}" \
  -profile development \
  -internal-listen 127.0.0.1:9555 \
  -agent-bootstrap-tokens "${AGENT_BOOTSTRAP_TOKENS}" \
  -db-url 'postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable' \
  -state-dir "${state_dir}" \
  -ingress-public-addr platform.local \
  -bootstrap-user vm-user:vm@example.com 2>&1)
status=$?
set -e

printf '%s\n' "${output}"
if [[ ${status} -eq 0 || ${status} -eq 124 ]]; then
  echo "replica using local state did not fail promptly (status=${status})" >&2
  exit 1
fi
grep -Fq 'control-plane state directory is not the shared directory registered by this database' <<<"${output}"
