#!/usr/bin/env bash
# Bring up the full docker-compose stack and check it end to end:
# Raft leader, proxy and AI routes, kill-a-backend chaos, killing the Raft
# leader, a canary rollout, Prometheus and Grafana. Exits non-zero on the
# first failure (and prints logs). Used by .github/workflows/compose.yml.
set -euo pipefail
cd "$(dirname "$0")/.."
C="docker compose -f deploy/docker-compose.yml"
CTL="bin/tollgatectl -endpoints 127.0.0.1:7201,127.0.0.1:7202,127.0.0.1:7203"
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; $C ps; $C logs --tail 50; exit 1; }
waitfor() { # url, seconds
  for _ in $(seq "$2"); do curl -sf -o /dev/null "$1" && return 0; sleep 1; done; return 1; }

make -s build
$C up -d --build
trap '$C down -v >/dev/null 2>&1 || true' EXIT

waitfor http://127.0.0.1:9090/readyz 180 || fail "gateway-1 not ready (rules from the control plane?)"
for p in 9091 9092; do waitfor http://127.0.0.1:$p/readyz 60 || fail "gateway on :$p not ready"; done
pass "3 gateways ready with rules from the control plane"

$CTL status | tee /dev/stderr | grep -q leader || fail "no Raft leader"
pass "Raft leader elected"

code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/hello)
[ "$code" = 200 ] || fail "proxy route returned $code"
pass "proxy route through gateway-1 -> backends"

out=$(curl -sN -H 'Authorization: Bearer demo-pro-key' http://127.0.0.1:8081/v1/chat/completions \
  -d '{"model":"m","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hello"}]}')
echo "$out" | grep -q 'data: \[DONE\]' || fail "AI stream did not finish: $out"
pass "AI route streams through gateway-2 to the mock LLM"

code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8082/v1/chat/completions -d '{"model":"m"}')
[ "$code" = 401 ] || fail "AI route without a key returned $code, want 401"
pass "rules enforced (401 without API key)"

MODE=docker bench/chaos/kill-backend.sh || fail "kill-backend chaos check"
pass "killing a backend shifts traffic (M1 acceptance, now verified in compose)"

leader=$($CTL status | awk '$2=="leader"{print $1}' | head -1)
n=${leader##*:72}; n=${n#0}
$C kill "ctl-$n" >/dev/null
sleep 3
$CTL put compose-check ok >/dev/null || fail "write after killing Raft leader ctl-$n"
$C start "ctl-$n" >/dev/null
pass "killed Raft leader ctl-$n; writes continue"

# Canary rollout with traffic on every gateway: must reach a decision.
( for i in $(seq 1500); do for p in 8080 8081 8082; do curl -s -o /dev/null http://127.0.0.1:$p/x?i=$i & done; sleep 0.02; done; wait ) &
TRAFFIC=$!
$CTL rules rollout deploy/rules.tg gateway-1 || fail "start rollout"
state=""
for _ in $(seq 60); do
  state=$($CTL rollout status | head -1)
  case "$state" in *promoted*|*rolled_back*) break;; esac
  sleep 1
done
kill $TRAFFIC 2>/dev/null || true
echo "$state" | grep -Eq 'promoted|rolled_back' || fail "rollout undecided: $state"
pass "canary rollout decided automatically: $state"

waitfor http://127.0.0.1:9095/-/ready 30 || fail "Prometheus not ready"
waitfor http://127.0.0.1:3000/api/health 60 || fail "Grafana not healthy"
pass "Prometheus and Grafana up"
echo "compose check: all passed"
