#!/usr/bin/env bash
set -euo pipefail

AGENT_BIN=${AGENT_BIN:-/opt/ebpf-wg-mesh/agent}
STATE_DIR=${STATE_DIR:-/var/lib/ebpf-wg-mesh/agent}
CA_FILE=${CA_FILE:-/opt/ebpf-wg-mesh/controlplane-ca.crt}
NODE_ID=${NODE_ID:?NODE_ID is required}
NODE_NAME=${NODE_NAME:?NODE_NAME is required}
ADVERTISE_ADDR=${ADVERTISE_ADDR:?ADVERTISE_ADDR is required}
CONTROLPLANE_ADDRESS=${CONTROLPLANE_ADDRESS:?CONTROLPLANE_ADDRESS is required}
BOOTSTRAP_TOKEN=${BOOTSTRAP_TOKEN:-vm-bootstrap-token}
WORKLOAD_CNI_SUBNET=${WORKLOAD_CNI_SUBNET:-fd00:200::/48}

mkdir -p /opt/ebpf-wg-mesh "${STATE_DIR}"

ensure_package() {
  local pkg=$1
  if dpkg -s "${pkg}" >/dev/null 2>&1; then
    return 0
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends "${pkg}"
}

ensure_cni() {
  ensure_package containernetworking-plugins
  ensure_package iproute2
  ensure_package iptables

  install -d -m 0755 /etc/cni/net.d
  cat >/etc/cni/net.d/10-mesh-cni.conflist <<EOF
{
  "cniVersion": "1.0.0",
  "name": "mesh-cni",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "mesh0",
      "isGateway": true,
      "hairpinMode": true,
      "ipMasq": false,
      "promiscMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [
            {
              "subnet": "${WORKLOAD_CNI_SUBNET}"
            }
          ]
        ],
        "routes": [
          {
            "dst": "::/0"
          }
        ]
      }
    },
    {
      "type": "loopback"
    }
  ]
}
EOF

  cat >/etc/sysctl.d/99-ebpf-wg-mesh-agent.conf <<EOF
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
EOF
  sysctl --system >/dev/null
}

ensure_cni

cat >/etc/systemd/system/ebpf-wg-mesh-agent.service <<EOF
[Unit]
Description=ebpf-wg-mesh agent
After=network-online.target containerd.service
Wants=network-online.target
Requires=containerd.service

[Service]
Type=simple
ExecStart=${AGENT_BIN} -node-id ${NODE_ID} -node-name ${NODE_NAME} -advertise-addr ${ADVERTISE_ADDR} -controlplane-address ${CONTROLPLANE_ADDRESS} -ca-file ${CA_FILE} -bootstrap-token ${BOOTSTRAP_TOKEN} -data-dir ${STATE_DIR}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

chmod 0755 "${AGENT_BIN}"
systemctl daemon-reload
systemctl enable --now containerd
systemctl enable --now ebpf-wg-mesh-agent.service
