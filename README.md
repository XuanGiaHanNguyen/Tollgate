# Tollgate

A distributed AI gateway written in Go: a fleet of gateway nodes that route, protect and rate-limit traffic to LLM providers and other backends, configured by a small rule language compiled to bytecode, with config replicated by a hand-built Raft control plane.

See [`docs/SPEC.md`](docs/SPEC.md) for the full build spec and [`docs/DESIGN.md`](docs/DESIGN.md) for decisions made along the way.

## Status

| Milestone | State |
| --- | --- |
| M1 Single-node gateway | done (compose run not yet verified) |
| M2 Metrics and first numbers | done: see [`docs/METRICS.md`](docs/METRICS.md) |
| M3 Rule language | not started |
| M4 Distributed rate limiting | not started |
| M5 AI gateway | not started |
| M6 Raft control plane | not started |
| M7 Safe rollout | not started |
| M8 AWS and polish | not started |

## Quick start

```sh
make build                     # bin/tollgate-gw, bin/mockbackend
make race                      # unit tests with the race detector
make chaos-kill-backend-local  # kill a backend under load (no Docker needed)

make bench-e1 && make report   # load test (k6), regenerate docs/METRICS.md

make up                        # docker-compose: 1 gateway + 3 backends, Prometheus :9091, Grafana :3000
curl localhost:8080/hello      # proxied; X-Backend header shows which backend
curl localhost:9090/admin/upstreams
make chaos-kill-backend        # same kill check against compose
make down
```

Gateway config is YAML (`deploy/gateway.yaml`); it is replaced by the rule language in M3 and by the control plane in M6.
