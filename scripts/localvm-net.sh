#!/usr/bin/env bash
# Privileged network helper for the local (QEMU/KVM) testvm provider.
# Installed to /usr/local/bin/ebpf-wg-mesh-localnet by localvm-setup.sh and
# invoked through a narrow passwordless sudoers rule. Keep the interface small
# and validate every argument so the sudoers surface stays auditable.
set -euo pipefail
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

usage() {
  cat >&2 <<'EOF'
usage:
  ebpf-wg-mesh-localnet bridge-up   <bridge> <gateway-cidr> <network-cidr> [uplink]
  ebpf-wg-mesh-localnet bridge-down <bridge> <network-cidr> [uplink]
  ebpf-wg-mesh-localnet tap-add     <bridge> <tap> <user>
  ebpf-wg-mesh-localnet tap-del     <tap>
EOF
  exit 2
}

valid_bridge() {
	[[ "$1" =~ ^ebm[0-9a-f]{8}$ ]]
}

valid_tap() {
	[[ "$1" =~ ^ebt[0-9a-f]{6}[0-9]{1,2}$ ]]
}

valid_uplink_name() {
	[[ "$1" =~ ^[a-zA-Z0-9_.-]{1,15}$ ]] &&
		[[ "$1" != "lo" && "$1" != ebm* && "$1" != ebt* ]]
}

valid_uplink() {
	valid_uplink_name "$1" && ip link show "$1" >/dev/null 2>&1
}

valid_cidr() {
	/usr/bin/python3 -c 'import ipaddress,sys
n=ipaddress.ip_interface(sys.argv[1]); a=n.ip; p=n.network
r=[ipaddress.ip_network(x) for x in ("10.0.0.0/8","172.16.0.0/12","192.168.0.0/16")]
sys.exit(0 if a.version == 4 and any(p.subnet_of(x) for x in r) and not (a.is_loopback or a.is_link_local or a.is_multicast or a.is_unspecified) and p.prefixlen <= 24 else 1)' "$1" >/dev/null 2>&1
}

valid_gateway_network() {
	/usr/bin/python3 -c 'import ipaddress,sys
g=ipaddress.ip_interface(sys.argv[1]); n=ipaddress.ip_network(sys.argv[2], strict=True)
sys.exit(0 if g.network == n and g.ip != n.network_address and g.ip != n.broadcast_address else 1)' "$1" "$2" >/dev/null 2>&1
}

valid_user() {
  [[ "$1" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]
}

default_uplink() {
	ip -o route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}'
}

command="${1:-}"
case "$command" in
  bridge-up)
    [[ $# -ge 4 && $# -le 5 ]] || usage
    bridge="$2"; gateway="$3"; network="$4"; uplink="${5:-}"
		valid_bridge "$bridge" || { echo "invalid bridge name" >&2; exit 2; }
    valid_cidr "$gateway" || { echo "invalid gateway cidr" >&2; exit 2; }
    valid_cidr "$network" || { echo "invalid network cidr" >&2; exit 2; }
		valid_gateway_network "$gateway" "$network" || { echo "gateway is not in network" >&2; exit 2; }
    if [[ -z "$uplink" ]]; then
      uplink="$(default_uplink)"
    fi
    if [[ -z "$uplink" ]]; then
      echo "no default route uplink found; pass one explicitly" >&2
      exit 1
    fi
		valid_uplink "$uplink" || { echo "invalid uplink name" >&2; exit 2; }
		rollback=1
		rollback_bridge_up() {
			if [[ "${rollback}" -eq 0 ]]; then return; fi
			iptables -t nat -D POSTROUTING -s "$network" -o "$uplink" -j MASQUERADE 2>/dev/null || true
			iptables -D FORWARD -i "$bridge" -o "$uplink" -j ACCEPT 2>/dev/null || true
			iptables -D FORWARD -i "$uplink" -o "$bridge" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
			iptables -t mangle -D FORWARD -s "$network" -i "$bridge" -o "$uplink" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360 2>/dev/null || true
			ip link del "$bridge" 2>/dev/null || true
		}
		trap rollback_bridge_up EXIT

    if ! ip link show "$bridge" >/dev/null 2>&1; then
      ip link add name "$bridge" type bridge
    fi
    ip link set "$bridge" up
    ip addr replace "$gateway" dev "$bridge"

    sysctl -q -w net.ipv4.ip_forward=1

    if ! iptables -t nat -C POSTROUTING -s "$network" -o "$uplink" -j MASQUERADE 2>/dev/null; then
      iptables -t nat -I POSTROUTING 1 -s "$network" -o "$uplink" -j MASQUERADE
    fi
    if ! iptables -C FORWARD -i "$bridge" -o "$uplink" -j ACCEPT 2>/dev/null; then
      iptables -I FORWARD 1 -i "$bridge" -o "$uplink" -j ACCEPT
    fi
    if ! iptables -C FORWARD -i "$uplink" -o "$bridge" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
      iptables -I FORWARD 1 -i "$uplink" -o "$bridge" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    fi
    # Clamp TCP MSS so NATed guests cannot black-hole on a smaller path MTU
    # than their local 1500-byte NIC assumes.
		if ! iptables -t mangle -C FORWARD -s "$network" -i "$bridge" -o "$uplink" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360 2>/dev/null; then
			iptables -t mangle -I FORWARD 1 -s "$network" -i "$bridge" -o "$uplink" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360
		fi
		rollback=0
    echo "bridge $bridge up gateway=$gateway network=$network uplink=$uplink"
    ;;

  bridge-down)
    [[ $# -eq 3 || $# -eq 4 ]] || usage
    bridge="$2"; network="$3"; uplink="${4:-}"
		valid_bridge "$bridge" || { echo "invalid bridge name" >&2; exit 2; }
    valid_cidr "$network" || { echo "invalid network cidr" >&2; exit 2; }
    if [[ -n "$uplink" ]]; then
			valid_uplink_name "$uplink" || { echo "invalid uplink name" >&2; exit 2; }
    else
      uplink="$(default_uplink)"
    fi
    if [[ -n "$uplink" ]]; then
      iptables -t nat -D POSTROUTING -s "$network" -o "$uplink" -j MASQUERADE 2>/dev/null || true
      iptables -D FORWARD -i "$bridge" -o "$uplink" -j ACCEPT 2>/dev/null || true
      iptables -D FORWARD -i "$uplink" -o "$bridge" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
			iptables -t mangle -D FORWARD -s "$network" -i "$bridge" -o "$uplink" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360 2>/dev/null || true
    fi
    ip link del "$bridge" 2>/dev/null || true
    echo "bridge $bridge down"
    ;;

  tap-add)
    [[ $# -eq 4 ]] || usage
    bridge="$2"; tap="$3"; user="$4"
		valid_bridge "$bridge" || { echo "invalid bridge name" >&2; exit 2; }
		valid_tap "$tap" || { echo "invalid tap name" >&2; exit 2; }
    valid_user "$user" || { echo "invalid user" >&2; exit 2; }
    if ip link show "$tap" >/dev/null 2>&1; then
      ip link del "$tap" || true
    fi
    ip tuntap add dev "$tap" mode tap user "$user"
    ip link set "$tap" master "$bridge"
    ip link set "$tap" up
    echo "tap $tap attached to $bridge for $user"
    ;;

  tap-del)
    [[ $# -eq 2 ]] || usage
    tap="$2"
		valid_tap "$tap" || { echo "invalid tap name" >&2; exit 2; }
    ip link del "$tap" 2>/dev/null || true
    echo "tap $tap removed"
    ;;

  *)
    usage
    ;;
esac
