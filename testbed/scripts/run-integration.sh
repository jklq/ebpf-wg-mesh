#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${ROOT_DIR}/testbed/docker-compose.yml"
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

NODE_SERVICES=(clustera-node1 clustera-node2 clusterb-node1 clusterb-node2)
WORKLOAD_IMAGE="docker.io/library/busybox:1.36"

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}

wait_for_health() {
  local svc="$1"
  local tries=90
  for ((i = 1; i <= tries; i++)); do
    local cid state
    cid=$("${COMPOSE[@]}" ps -q "$svc" 2>/dev/null || true)
    state=""
    if [[ -n "$cid" ]]; then
      state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)
    fi
    if [[ "$state" == "healthy" ]]; then
      return 0
    fi
    sleep 2
  done
  echo "service $svc did not become healthy" >&2
  return 1
}

node_exec() {
  local node="$1"
  shift
  "${COMPOSE[@]}" exec -T "$node" bash -lc "$*"
}

wait_container_running() {
  local node="$1"
  local name="$2"
  local tries=30
  local i
  for ((i = 1; i <= tries; i++)); do
    local status
    status=$(node_exec "$node" "nerdctl -n default inspect -f '{{.State.Status}}' ${name} 2>/dev/null || true")
    if [[ "$status" == "running" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "container ${name} on ${node} did not reach running state" >&2
  return 1
}

wait_service_ready() {
  local node="$1"
  local name="$2"
  local tries=30
  local i
  for ((i = 1; i <= tries; i++)); do
    if node_exec "$node" "nerdctl -n default exec ${name} sh -lc 'wget -qO- --timeout=1 http://127.0.0.1:8080 >/dev/null'"; then
      return 0
    fi
    sleep 1
  done
  echo "service ${name} on ${node} did not become ready" >&2
  return 1
}

wait_container_stopped() {
  local node="$1"
  local name="$2"
  local tries=30
  local i
  for ((i = 1; i <= tries; i++)); do
    local status
    status=$(node_exec "$node" "nerdctl -n default inspect -f '{{.State.Status}}' ${name} 2>/dev/null || true")
    if [[ "$status" == "exited" || "$status" == "stopped" || "$status" == "dead" || -z "$status" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "container ${name} on ${node} did not stop in time" >&2
  return 1
}

retry_node_exec() {
  local node="$1"
  local attempts="$2"
  local delay_s="$3"
  shift 3
  local cmd="$*"

  local i
  for ((i = 1; i <= attempts; i++)); do
    if node_exec "$node" "$cmd"; then
      return 0
    fi
    sleep "$delay_s"
  done
  return 1
}

retry_until_fail() {
  local node="$1"
  local attempts="$2"
  local delay_s="$3"
  shift 3
  local cmd="$*"

  local i
  for ((i = 1; i <= attempts; i++)); do
    if ! node_exec "$node" "$cmd"; then
      return 0
    fi
    sleep "$delay_s"
  done
  return 1
}

create_workload() {
  local node="$1"
  local name="$2"
  local ip="$3"
  local project="$4"
  local public="$5"
  local mode="$6"

  node_exec "$node" "nerdctl -n default rm -f ${name} >/dev/null 2>&1 || true"

  if [[ "$mode" == "service" ]]; then
    node_exec "$node" "nerdctl -n default --snapshotter native run -d --name ${name} --net mesh-cni --ip ${ip} --label mesh.project_id=${project} --label mesh.ipv4=${ip} --label mesh.public_service=${public} ${WORKLOAD_IMAGE} sh -lc 'mkdir -p /www && echo ${name} > /www/index.html && httpd -f -p 8080 -h /www'"
  else
    node_exec "$node" "nerdctl -n default --snapshotter native run -d --name ${name} --net mesh-cni --ip ${ip} --label mesh.project_id=${project} --label mesh.ipv4=${ip} --label mesh.public_service=${public} ${WORKLOAD_IMAGE} sh -lc 'trap : TERM INT; while true; do sleep 3600; done'"
  fi

  wait_container_running "$node" "$name"
  if [[ "$mode" == "service" ]]; then
    wait_service_ready "$node" "$name"
  fi
}

trap cleanup EXIT

cleanup
"${COMPOSE[@]}" up -d --build

for svc in "${NODE_SERVICES[@]}"; do
  wait_for_health "$svc"
done

echo "[setup] pull workload image on all nodes"
for svc in "${NODE_SERVICES[@]}"; do
  node_exec "$svc" "nerdctl -n default --snapshotter native pull ${WORKLOAD_IMAGE} >/dev/null 2>&1"
done

echo "[setup] create labeled workloads across two clusters"
create_workload clustera-node1 a1-client 10.200.1.11 100 false worker
create_workload clustera-node1 a1-local 10.200.1.12 100 false service
create_workload clustera-node2 a2-service 10.200.2.20 100 true service
create_workload clusterb-node1 b1-service 10.201.1.20 100 true service
create_workload clusterb-node2 b2-client 10.201.2.30 100 false worker
create_workload clusterb-node2 b2-tenant2 10.201.2.40 200 true worker

sleep 6

echo "[1/6] host-local same-project service path bypasses WireGuard"
before_tx=$(node_exec clustera-node1 "cat /sys/class/net/wg0/statistics/tx_packets")
retry_node_exec clustera-node1 12 1 "nerdctl -n default exec a1-client sh -lc 'wget -qO- --timeout=4 http://10.200.1.12:8080 | grep -q a1-local'"
after_tx=$(node_exec clustera-node1 "cat /sys/class/net/wg0/statistics/tx_packets")
delta_tx=$((after_tx - before_tx))
if [[ "$delta_tx" -gt 1 ]]; then
  echo "expected near-zero wg0 tx packets for host-local flow, delta=${delta_tx}" >&2
  exit 1
fi

echo "[2/6] same-project inter-node flow works inside cluster-a"
retry_node_exec clustera-node1 12 1 "nerdctl -n default exec a1-client sh -lc 'wget -qO- --timeout=4 http://10.200.2.20:8080 | grep -q a2-service'"

echo "[3/6] same-project inter-cluster flow works"
retry_node_exec clustera-node1 12 1 "nerdctl -n default exec a1-client sh -lc 'wget -qO- --timeout=4 http://10.201.1.20:8080 | grep -q b1-service'"

echo "[4/6] cross-project access is denied"
if ! retry_until_fail clusterb-node2 8 1 "nerdctl -n default exec b2-tenant2 sh -lc 'wget -qO- --timeout=4 http://10.201.1.20:8080 >/dev/null'"; then
  echo "expected cross-project request to fail, but it succeeded" >&2
  exit 1
fi

echo "[5/6] TaskExit removes remote reachability"
node_exec clustera-node2 "nerdctl -n default kill -s KILL a2-service >/dev/null 2>&1 || true"
wait_container_stopped clustera-node2 a2-service
node_exec clustera-node2 "nerdctl -n default rm a2-service >/dev/null 2>&1 || true"
if ! retry_until_fail clustera-node1 10 1 "nerdctl -n default exec a1-client sh -lc 'wget -qO- --timeout=4 http://10.200.2.20:8080 >/dev/null'"; then
  echo "expected request to stopped service to fail" >&2
  exit 1
fi

echo "[6/6] TaskStart restores policy and connectivity"
create_workload clustera-node2 a2-service 10.200.2.20 100 true service
retry_node_exec clustera-node1 12 1 "nerdctl -n default exec a1-client sh -lc 'wget -qO- --timeout=4 http://10.200.2.20:8080 | grep -q a2-service'"

echo "integration testbed passed"
