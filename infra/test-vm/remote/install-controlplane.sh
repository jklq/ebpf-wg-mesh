#!/usr/bin/env bash
set -euo pipefail

CONTROLPLANE_BIN=${CONTROLPLANE_BIN:-/opt/ebpf-wg-mesh/controlplane}
STATE_DIR=${STATE_DIR:-/var/lib/ebpf-wg-mesh/controlplane}
POSTGRES_VERSION=${POSTGRES_VERSION:-16}
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

DATA_DIR=/var/lib/ebpf-wg-mesh/postgresql/data
mkdir -p /opt/ebpf-wg-mesh "${STATE_DIR}" /var/lib/ebpf-wg-mesh/postgresql

PGBIN="/usr/lib/postgresql/${POSTGRES_VERSION}/bin"
if [[ ! -x "${PGBIN}/postgres" ]]; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "postgresql-${POSTGRES_VERSION}" "postgresql-client-${POSTGRES_VERSION}"
fi

id -u platform-postgres >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin platform-postgres
mkdir -p "${DATA_DIR}"
chown -R platform-postgres /var/lib/ebpf-wg-mesh/postgresql
chmod 0700 "${DATA_DIR}"
if [[ ! -f "${DATA_DIR}/PG_VERSION" ]]; then
  runuser -u platform-postgres -- "${PGBIN}/initdb" -D "${DATA_DIR}" -U postgres --auth=trust --data-checksums --no-instructions >/dev/null
fi

cat >/etc/systemd/system/ebpf-wg-mesh-postgresql.service <<EOF
[Unit]
Description=ebpf-wg-mesh PostgreSQL
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=platform-postgres
ExecStart=${PGBIN}/postgres -D ${DATA_DIR} -c listen_addresses=127.0.0.1 -c port=5432 -c unix_socket_directories=/var/lib/ebpf-wg-mesh/postgresql -c wal_level=replica -c archive_mode=on -c archive_command=true
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat >"/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=ebpf-wg-mesh controlplane (${SERVICE_NAME})
After=network-online.target ebpf-wg-mesh-postgresql.service
Wants=network-online.target
Requires=ebpf-wg-mesh-postgresql.service

[Service]
Type=simple
ExecStart=${CONTROLPLANE_BIN} -profile development -internal-listen ${INTERNAL_LISTEN} -replica-addresses "${REPLICA_ADDRESSES}" -advertise-addr "${ADVERTISE_ADDR}" -agent-bootstrap-tokens ${AGENT_BOOTSTRAP_TOKENS} -db-url postgresql://postgres@127.0.0.1:5432/platform?sslmode=disable -state-dir ${STATE_DIR} -ingress-xds-listen ${XDS_LISTEN} -ingress-public-addr ${PUBLIC_ADDR} -bootstrap-user ${BOOTSTRAP_USER}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

chmod 0755 "${CONTROLPLANE_BIN}"
systemctl daemon-reload
systemctl enable --now ebpf-wg-mesh-postgresql.service
# The control plane creates its schemas inside the one platform database;
# the database itself is provisioned here, once the server accepts SQL.
for _ in $(seq 1 50); do
  if "${PGBIN}/psql" -h 127.0.0.1 -p 5432 -U postgres -d postgres -tAc 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
"${PGBIN}/psql" -h 127.0.0.1 -p 5432 -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='platform'" | grep -q 1 ||
  "${PGBIN}/createdb" -h 127.0.0.1 -p 5432 -U postgres platform
systemctl enable --now "${SERVICE_NAME}.service"
