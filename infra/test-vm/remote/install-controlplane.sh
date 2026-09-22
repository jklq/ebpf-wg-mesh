#!/usr/bin/env bash
set -euo pipefail

CONTROLPLANE_BIN=${CONTROLPLANE_BIN:-/opt/ebpf-wg-mesh/controlplane}
STATE_DIR=${STATE_DIR:-/var/lib/ebpf-wg-mesh/controlplane}
COCKROACH_VERSION=${COCKROACH_VERSION:-v26.1.0}
AGENT_BOOTSTRAP_TOKENS=${AGENT_BOOTSTRAP_TOKENS:?AGENT_BOOTSTRAP_TOKENS must contain comma-separated agent_id=token bindings}
INTERNAL_LISTEN=${INTERNAL_LISTEN:-0.0.0.0:9443}
PUBLIC_ADDR=${PUBLIC_ADDR:-platform.local}
# vm-user is an operator: e2e probes call operator-only RPCs such as ListAgents.
BOOTSTRAP_USER=${BOOTSTRAP_USER:-vm-user:vm@example.com+operator}
SERVICE_NAME=${SERVICE_NAME:-ebpf-wg-mesh-controlplane}
XDS_LISTEN=${XDS_LISTEN:-127.0.0.1:18000}
REPLICA_ADDRESSES=${REPLICA_ADDRESSES:-}
ADVERTISE_ADDR=${ADVERTISE_ADDR:-}

if [[ ! "${SERVICE_NAME}" =~ ^[a-zA-Z0-9_.@-]+$ ]]; then
  echo "invalid SERVICE_NAME: ${SERVICE_NAME}" >&2
  exit 1
fi

mkdir -p /opt/ebpf-wg-mesh "${STATE_DIR}" /var/lib/ebpf-wg-mesh/cockroach

if ! command -v cockroach >/dev/null 2>&1; then
  tmpdir=$(mktemp -d)
  trap 'rm -rf "${tmpdir}"' EXIT
  curl -fsSL "https://binaries.cockroachdb.com/cockroach-${COCKROACH_VERSION}.linux-amd64.tgz" -o "${tmpdir}/cockroach.tgz"
  tar -xzf "${tmpdir}/cockroach.tgz" -C "${tmpdir}"
  install "${tmpdir}/cockroach-${COCKROACH_VERSION}.linux-amd64/cockroach" /usr/local/bin/cockroach
fi

cat >/etc/systemd/system/ebpf-wg-mesh-cockroach.service <<EOF
[Unit]
Description=ebpf-wg-mesh CockroachDB
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/cockroach start-single-node --insecure --listen-addr=127.0.0.1:26257 --http-addr=127.0.0.1:8081 --store=/var/lib/ebpf-wg-mesh/cockroach
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat >"/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=ebpf-wg-mesh controlplane (${SERVICE_NAME})
After=network-online.target ebpf-wg-mesh-cockroach.service
Wants=network-online.target
Requires=ebpf-wg-mesh-cockroach.service

[Service]
Type=simple
ExecStart=${CONTROLPLANE_BIN} -profile development -internal-listen ${INTERNAL_LISTEN} -replica-addresses "${REPLICA_ADDRESSES}" -advertise-addr "${ADVERTISE_ADDR}" -agent-bootstrap-tokens ${AGENT_BOOTSTRAP_TOKENS} -db-url postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable -state-dir ${STATE_DIR} -ingress-xds-listen ${XDS_LISTEN} -ingress-public-addr ${PUBLIC_ADDR} -bootstrap-user ${BOOTSTRAP_USER}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

chmod 0755 "${CONTROLPLANE_BIN}"
systemctl daemon-reload
systemctl enable --now ebpf-wg-mesh-cockroach.service "${SERVICE_NAME}.service"
