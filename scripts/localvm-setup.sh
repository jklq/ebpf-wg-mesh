#!/usr/bin/env bash
# One-time privileged host preparation for the local (QEMU/KVM) testvm provider.
# Must run as root, for example: sudo bash scripts/localvm-setup.sh [user]
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
	echo "local VM setup is supported only on Linux" >&2
	exit 1
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root: sudo bash $0 [user]" >&2
  exit 1
fi

TARGET_USER="${1:-${SUDO_USER:-}}"
if [[ -z "${TARGET_USER}" || "${TARGET_USER}" == "root" ]]; then
  echo "specify the unprivileged user that will run the harness: $0 <user>" >&2
  exit 1
fi
if ! id "${TARGET_USER}" >/dev/null 2>&1; then
  echo "unknown user ${TARGET_USER}" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER_SRC="${SCRIPT_DIR}/localvm-net.sh"
HELPER_DST="/usr/local/bin/ebpf-wg-mesh-localnet"
SUDOERS="/etc/sudoers.d/ebpf-wg-mesh-localvm"

echo "== installing packages =="
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  qemu-system-x86 \
  qemu-utils \
  genisoimage \
  libguestfs-tools \
  iproute2 \
  iptables \
	python3 \
  curl \
  ca-certificates

echo "== enabling hardware virtualization =="
KV_MOD=""
if grep -qE '(^| )vmx( |$)' /proc/cpuinfo; then
  KV_MOD="kvm_intel"
elif grep -qE '(^| )svm( |$)' /proc/cpuinfo; then
  KV_MOD="kvm_amd"
fi
if [[ -z "${KV_MOD}" ]]; then
  echo "CPU exposes neither vmx nor svm; nested virtualization may be disabled" >&2
  exit 1
fi
modprobe "${KV_MOD}"
cat >/etc/modules-load.d/ebpf-wg-mesh-kvm.conf <<EOF
${KV_MOD}
EOF

cat >/etc/udev/rules.d/99-ebpf-wg-mesh-kvm.rules <<'EOF'
KERNEL=="kvm", SUBSYSTEM=="misc", GROUP="kvm", MODE="0660"
EOF
groupadd -f kvm
usermod -aG kvm "${TARGET_USER}"
udevadm control --reload-rules
udevadm trigger --subsystem-match=misc --action=add || true
if [[ ! -e /dev/kvm ]]; then
  echo "warning: /dev/kvm still absent; a reboot may be required" >&2
fi

echo "== enabling IPv4 forwarding =="
cat >/etc/sysctl.d/99-ebpf-wg-mesh-localvm.conf <<'EOF'
net.ipv4.ip_forward=1
EOF
sysctl -q --system

echo "== installing network helper =="
if [[ ! -f "${HELPER_SRC}" ]]; then
  echo "missing ${HELPER_SRC}" >&2
  exit 1
fi
install -m 0755 "${HELPER_SRC}" "${HELPER_DST}"

SUDOERS_TMP="$(mktemp)"
trap 'rm -f "${SUDOERS_TMP}"' EXIT
cat >"${SUDOERS_TMP}" <<EOF
# Allow the unprivileged harness user to manage only local testvm networking.
${TARGET_USER} ALL=(root) NOPASSWD: ${HELPER_DST}
EOF
chmod 0440 "${SUDOERS_TMP}"
visudo -cf "${SUDOERS_TMP}" >/dev/null
install -m 0440 "${SUDOERS_TMP}" "${SUDOERS}"

echo
echo "setup complete for user ${TARGET_USER}"
echo "helper: ${HELPER_DST}"
echo "note: log out and back in (or run 'newgrp kvm') so KVM access applies"
