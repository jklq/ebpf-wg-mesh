#!/usr/bin/env bash
set -euo pipefail

XDS_PROBE_BIN=${XDS_PROBE_BIN:-/opt/ebpf-wg-mesh/xds-probe}
PROBE_DIR=${PROBE_DIR:-/var/lib/ebpf-wg-mesh/xds-probe}
XDS_ADDRS=${XDS_ADDRS:?XDS_ADDRS must list comma-separated control-plane xDS addresses}

mkdir -p /opt/ebpf-wg-mesh "${PROBE_DIR}"

# A probe is an operator-owned ingress subscriber, never a fleet agent.
/opt/ebpf-wg-mesh/controlplane signing-keys issue-client-cert \
  --db-url 'postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable' \
  --state-dir /var/lib/ebpf-wg-mesh/controlplane \
  --caller-class ingress --caller-id testvm-xds-probe --ttl 24h \
  --out-dir "${PROBE_DIR}/identity"

cat >/etc/systemd/system/ebpf-wg-mesh-xds-probe.service <<EOF
[Unit]
Description=ebpf-wg-mesh xDS probe
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${XDS_PROBE_BIN} -xds-addrs ${XDS_ADDRS} -probe-dir ${PROBE_DIR} -identity-dir ${PROBE_DIR}/identity -server-name controlplane
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF

chmod 0755 "${XDS_PROBE_BIN}"
systemctl daemon-reload
systemctl enable --now ebpf-wg-mesh-xds-probe.service
