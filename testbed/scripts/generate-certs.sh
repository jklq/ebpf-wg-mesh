#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CERT_DIR="${ROOT_DIR}/certs"
mkdir -p "${CERT_DIR}"

if [[ -f "${CERT_DIR}/ca.crt" ]]; then
  exit 0
fi

openssl genrsa -out "${CERT_DIR}/ca.key" 2048 >/dev/null 2>&1
openssl req -x509 -new -nodes -key "${CERT_DIR}/ca.key" -sha256 -days 3650 \
  -subj "/CN=mesh-dev-ca" -out "${CERT_DIR}/ca.crt" >/dev/null 2>&1

issue_cert() {
  local name="$1"
  local san="$2"
  openssl genrsa -out "${CERT_DIR}/${name}.key" 2048 >/dev/null 2>&1
  openssl req -new -key "${CERT_DIR}/${name}.key" -subj "/CN=${name}" -out "${CERT_DIR}/${name}.csr" >/dev/null 2>&1
  cat >"${CERT_DIR}/${name}.ext" <<EOF
subjectAltName=${san}
extendedKeyUsage=serverAuth,clientAuth
EOF
  openssl x509 -req -in "${CERT_DIR}/${name}.csr" -CA "${CERT_DIR}/ca.crt" -CAkey "${CERT_DIR}/ca.key" \
    -CAcreateserial -out "${CERT_DIR}/${name}.crt" -days 3650 -sha256 -extfile "${CERT_DIR}/${name}.ext" >/dev/null 2>&1
}

issue_cert "controlplane-external" "DNS:controlplane,IP:127.0.0.1"
issue_cert "controlplane-internal" "DNS:controlplane-internal,DNS:controlplane,IP:127.0.0.1"
issue_cert "agent-node1" "DNS:agent-node1,IP:127.0.0.1"
issue_cert "agent-node2" "DNS:agent-node2,IP:127.0.0.1"
