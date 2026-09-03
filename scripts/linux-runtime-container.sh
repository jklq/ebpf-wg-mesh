#!/usr/bin/env bash
# Provisions a Linux container so the agent/firewall runtime tests can run for
# real: containerd, the mesh CNI config, bpffs, and delegated cgroup v2
# controllers. Mirrors infra/test-vm/remote/install-agent.sh so the tests see
# the same network the VM e2e does. Runs inside the container; the remaining
# arguments are exec'd once the environment is up.
set -euo pipefail

WORKLOAD_CNI_IPV4_POOL=${WORKLOAD_CNI_IPV4_POOL:-10.200.0.0/16}
WORKLOAD_CNI_IPV6_POOL=${WORKLOAD_CNI_IPV6_POOL:-fd00:200::/48}

echo "==> mounting bpffs"
mountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf

# Nested cgroup v2: this container's cgroup root holds our own processes, which
# makes it a "domain" that runc cannot create child cgroups under. Move
# everything into an /init leaf and delegate the controllers to the subtree.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  echo "==> delegating nested cgroup2 controllers"
  mkdir -p /sys/fs/cgroup/init
  while read -r pid; do
    echo "$pid" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
  done < /sys/fs/cgroup/cgroup.procs
  for ctrl in $(cat /sys/fs/cgroup/cgroup.controllers); do
    echo "+$ctrl" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
  done
fi

echo "==> installing containerd and CNI plugins"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends \
  containerd containernetworking-plugins iproute2 iptables ca-certificates >/dev/null

echo "==> writing mesh CNI config"
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
          [{"subnet": "${WORKLOAD_CNI_IPV4_POOL}"}],
          [{"subnet": "${WORKLOAD_CNI_IPV6_POOL}"}]
        ],
        "routes": [{"dst": "0.0.0.0/0"}, {"dst": "::/0"}]
      }
    },
    {"type": "loopback"}
  ]
}
EOF

sysctl -qw net.ipv4.ip_forward=1
sysctl -qw net.ipv6.conf.all.forwarding=1
sysctl -qw net.ipv6.conf.all.disable_ipv6=0

echo "==> starting containerd"
containerd --log-level warn >/tmp/containerd.log 2>&1 &
for _ in $(seq 1 50); do
  [ -S /run/containerd/containerd.sock ] && break
  sleep 0.2
done
if [ ! -S /run/containerd/containerd.sock ]; then
  echo "containerd failed to start:" >&2
  cat /tmp/containerd.log >&2
  exit 1
fi

exec "$@"
