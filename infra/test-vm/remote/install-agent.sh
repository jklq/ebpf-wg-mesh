#!/usr/bin/env bash
set -euo pipefail

AGENT_BIN=${AGENT_BIN:-/opt/ebpf-wg-mesh/agent}
STATE_DIR=${STATE_DIR:-/var/lib/ebpf-wg-mesh/agent}
CA_FILE=${CA_FILE:-/opt/ebpf-wg-mesh/controlplane-ca.crt}
NODE_ID=${NODE_ID:?NODE_ID is required}
NODE_NAME=${NODE_NAME:?NODE_NAME is required}
ADVERTISE_ADDR=${ADVERTISE_ADDR:?ADVERTISE_ADDR is required}
MESH_ADVERTISE_ENDPOINT=${MESH_ADVERTISE_ENDPOINT:?MESH_ADVERTISE_ENDPOINT is required}
CONTROLPLANE_ADDRESSES=${CONTROLPLANE_ADDRESSES:?CONTROLPLANE_ADDRESSES is required}
BOOTSTRAP_TOKEN=${BOOTSTRAP_TOKEN:-vm-bootstrap-token}
WORKLOAD_CNI_IPV4_POOL=${WORKLOAD_CNI_IPV4_POOL:-10.200.0.0/16}
WORKLOAD_CNI_IPV6_POOL=${WORKLOAD_CNI_IPV6_POOL:-fd00:200::/48}

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
      "ipMasq": false,
      "promiscMode": true,
      "capabilities": {"ips": true},
      "ipam": {
        "type": "host-local",
        "ranges": [
          [
            {
              "subnet": "${WORKLOAD_CNI_IPV4_POOL}"
            }
          ],
          [
            {
              "subnet": "${WORKLOAD_CNI_IPV6_POOL}"
            }
          ]
        ],
        "routes": [
          {
            "dst": "0.0.0.0/0"
          },
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

install -d -m 0755 /etc/ebpf-wg-mesh
umask 077
printf 'AGENT_BOOTSTRAP_TOKEN=%s\n' "${BOOTSTRAP_TOKEN}" >/etc/ebpf-wg-mesh/agent.env
chmod 0600 /etc/ebpf-wg-mesh/agent.env

cat >/etc/systemd/system/ebpf-wg-mesh-agent.service <<EOF
[Unit]
Description=ebpf-wg-mesh agent
After=network-online.target containerd.service
Wants=network-online.target
Requires=containerd.service

[Service]
Type=simple
EnvironmentFile=/etc/ebpf-wg-mesh/agent.env
ExecStart=${AGENT_BIN} -profile development -node-id ${NODE_ID} -node-name ${NODE_NAME} -advertise-addr ${ADVERTISE_ADDR} -mesh-advertise-endpoint "${MESH_ADVERTISE_ENDPOINT}" -controlplane-addresses "${CONTROLPLANE_ADDRESSES}" -ca-file ${CA_FILE} -data-dir ${STATE_DIR}
Restart=always
RestartSec=3
OOMScoreAdjust=-500
MemoryMin=512M
CPUWeight=10000
Delegate=yes

[Install]
WantedBy=multi-user.target
EOF

chmod 0755 "${AGENT_BIN}"
systemctl daemon-reload
systemctl enable --now containerd
systemctl enable --now ebpf-wg-mesh-agent.service
