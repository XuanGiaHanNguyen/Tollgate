# Tollgate

A distributed AI gateway written in Go: a fleet of gateway nodes that route, protect and rate-limit traffic to LLM providers and other backends, configured by a small rule language compiled to bytecode, with config replicated by a hand-built Raft control plane.

See [`docs/SPEC.md`](docs/SPEC.md) for the full build spec and [`docs/DESIGN.md`](docs/DESIGN.md) for decisions made along the way.

## Status

| Milestone | State |
| --- | --- |
| M1 Single-node gateway | done (compose run not yet verified) |
| M2 Metrics and first numbers | done: see [`docs/METRICS.md`](docs/METRICS.md) |
| M3 Rule language | done: bytecode VM 2.56× faster than a fair interpreter on large rule sets (goal was 3×; see METRICS.md) |
| M4 Distributed rate limiting | done: 3 gateways over-admit +0.07% vs the limit; limits hold when a node dies (see METRICS.md) |
| M5 AI gateway | done: cost budgets hold within 0.3% (vs +15% for count limits); hedging cuts TTFT p99 49% for +0.77% cost; filter catches 100% of split phrases (see METRICS.md) |
| M6 Raft control plane | done: Raft from scratch, 5,000-seed simulation, 0 entries lost across 30 leader kills, config reaches gateways in ~5 ms (see METRICS.md) |
| M7 Safe rollout | done: bad configs rolled back automatically in 1.2–1.6 s (2.2–2.7 s across a leader kill); exposure is 1/N, not under (see METRICS.md) |
| M8 AWS and polish | not started |

## Quick start

```sh
make build                     # bin/tollgate-gw, bin/mockbackend
make race                      # unit tests with the race detector
make chaos-kill-backend-local  # kill a backend under load (no Docker needed)

make bench-e1 && make report   # load test (k6), regenerate docs/METRICS.md

make up                        # docker-compose: 3 control nodes + 3 gateways + backends + mock LLMs, Prometheus :9095, Grafana :3000
bin/tollgatectl -endpoints localhost:7201 status            # Raft cluster state
bin/tollgatectl -endpoints localhost:7201 rules push deploy/rules.tg          # immediate, everywhere
bin/tollgatectl -endpoints localhost:7201 rules rollout deploy/rules.tg gateway-1   # canary first, auto promote/rollback
bin/tollgatectl -endpoints localhost:7201 rollout status
curl localhost:8080/hello      # proxied; X-Backend header shows which backend
curl localhost:9090/admin/upstreams
make chaos-kill-backend        # same kill check against compose
make down
```

Gateway config is YAML (`deploy/gateway.yaml`); policy (auth, limits, deny, headers) is written in the rule language (`deploy/rules.tg`) and hot-reloaded. Try it with the demo keys: `curl -H 'Authorization: Bearer demo-free-key' localhost:8080/api/ai/chat`.

AI route (M5), streaming through the mock providers with budgets, hedging and the safety filter:

```sh
curl -N -H 'Authorization: Bearer demo-pro-key' localhost:8080/v1/chat/completions \
  -d '{"model":"m","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```
