#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${ROOT_DIR}/testbed/docker-compose.yml"
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}

wait_for_health() {
  local svc="$1"
  local tries=50
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

trap cleanup EXIT

cleanup
"${COMPOSE[@]}" up -d --build

for svc in node1 node2 node3 ext; do
  wait_for_health "$svc"
done

echo "[1/4] mesh trust path: node1 -> node3 ping"
"${COMPOSE[@]}" exec -T node1 ping -c 2 -W 2 10.44.0.3 >/dev/null

echo "[2/4] outbound external flow tracked and allowed"
"${COMPOSE[@]}" exec -T node1 curl -fsS --max-time 5 http://172.31.0.10:8080 >/dev/null

echo "[3/4] state sync control plane listeners online"
"${COMPOSE[@]}" exec -T node1 bash -lc "ss -lun | grep -q ':7001'"
"${COMPOSE[@]}" exec -T node2 bash -lc "ss -lun | grep -q ':7002'"
"${COMPOSE[@]}" exec -T node3 bash -lc "ss -lun | grep -q ':7003'"

echo "[4/4] unsolicited external ingress denied"
"${COMPOSE[@]}" exec -T node1 bash -lc 'rm -f /tmp/unsolicited.log /tmp/unsolicited.err; nohup sh -c "timeout 8 nc -l -p 9091 > /tmp/unsolicited.log" >/tmp/unsolicited.err 2>&1 &' 
"${COMPOSE[@]}" exec -T ext bash -lc 'echo blocked | timeout 3 nc -w 2 10.44.0.1 9091 || true'
sleep 2
"${COMPOSE[@]}" exec -T node1 bash -lc 'test ! -s /tmp/unsolicited.log'

echo "integration testbed passed"
