# Tollgate

A distributed AI gateway written in Go: a fleet of gateway nodes that route, protect and rate-limit traffic to LLM providers and other backends, configured by a small rule language compiled to bytecode, with config replicated by a hand-built Raft control plane.

See [`docs/SPEC.md`](docs/SPEC.md) for the full build spec and [`docs/DESIGN.md`](docs/DESIGN.md) for decisions made along the way.

## Status

| Milestone | State |
| --- | --- |
| M1 Single-node gateway | in progress |
| M2 Metrics and first numbers | not started |
| M3 Rule language | not started |
| M4 Distributed rate limiting | not started |
| M5 AI gateway | not started |
| M6 Raft control plane | not started |
| M7 Safe rollout | not started |
| M8 AWS and polish | not started |
