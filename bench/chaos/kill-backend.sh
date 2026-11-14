#!/usr/bin/env bash
# M1 acceptance check: kill one backend under steady traffic and report how
# long until the gateway stops sending it requests.
#   MODE=docker  uses deploy/docker-compose.yml (run `make up` first)
#   MODE=local   starts the binaries from bin/ as local processes
set -euo pipefail
cd "$(dirname "$0")/../.."
MODE="${MODE:-docker}"

if [[ "$MODE" == docker ]]; then
  exec go run ./bench/chaos/shiftcheck -url http://127.0.0.1:8080/ -victim backend-3 \
    -admin http://127.0.0.1:9090 -victim-url backend-3 \
    -kill "docker compose -f deploy/docker-compose.yml kill backend-3"
fi

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT
for i in 1 2 3; do
  bin/mockbackend -addr "127.0.0.1:910$i" -name "backend-$i" 2>/dev/null &
  pids+=($!)
done
victim_pid=${pids[2]}
bin/tollgate-gw -config deploy/local/gateway.yaml -log-level warn &
pids+=($!)
for _ in $(seq 50); do
  curl -sf http://127.0.0.1:19090/readyz >/dev/null && break
  sleep 0.1
done
go run ./bench/chaos/shiftcheck -url http://127.0.0.1:18080/ -victim backend-3 \
  -admin http://127.0.0.1:19090 -victim-url 127.0.0.1:9103 -kill "kill -9 $victim_pid"
