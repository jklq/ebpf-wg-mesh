#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <config-path>" >&2
  exit 1
fi

CONFIG_PATH="$1"
NODE_NAME="${NODE_NAME:-node}"
CNI_NETWORK_NAME="${CNI_NETWORK_NAME:-mesh-cni}"
CNI_SUBNET="${CNI_SUBNET:-fd00:200::/64}"
CNI_GATEWAY="${CNI_GATEWAY:-fd00:200::1}"
CNI_BRIDGE="${CNI_BRIDGE:-cni0}"
MESH_ROUTE_CIDR="${MESH_ROUTE_CIDR:-fd00:200::/48}"

mkdir -p /run/containerd /var/lib/containerd /etc/containerd /etc/cni/net.d /var/log

cat >/etc/containerd/config.toml <<'EOC'
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "native"
[plugins."io.containerd.grpc.v1.cri".cni]
  bin_dir = "/usr/lib/cni"
  conf_dir = "/etc/cni/net.d"
EOC

cat >/etc/cni/net.d/10-mesh.conflist <<EOC
{
  "cniVersion": "0.4.0",
  "name": "${CNI_NETWORK_NAME}",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "${CNI_BRIDGE}",
      "isGateway": true,
      "ipMasq": false,
      "hairpinMode": true,
      "promiscMode": false,
      "ipam": {
        "type": "host-local",
        "ranges": [[{ "subnet": "${CNI_SUBNET}", "gateway": "${CNI_GATEWAY}" }]],
        "routes": [{ "dst": "::/0" }]
      }
    },
    {
      "type": "portmap",
      "capabilities": { "portMappings": true }
    }
  ]
}
EOC

containerd >/var/log/containerd.log 2>&1 &
CONTAINERD_PID=$!

cleanup() {
  kill -TERM "$CONTAINERD_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for _ in $(seq 1 60); do
  if ctr version >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! ctr version >/dev/null 2>&1; then
  echo "containerd failed to start" >&2
  exit 1
fi

# Ensure mesh-bound workload ranges route through WireGuard once wg0 exists.
(
  for _ in $(seq 1 120); do
    if ip link show wg0 >/dev/null 2>&1; then
      ip -6 route replace "${MESH_ROUTE_CIDR}" dev wg0 || true
      exit 0
    fi
    sleep 1
  done
) &

exec /usr/local/bin/meshd -config "$CONFIG_PATH"
