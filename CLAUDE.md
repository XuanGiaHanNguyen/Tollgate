# Tollgate — working rules

Full spec: `docs/SPEC.md`. Decisions log: `docs/DESIGN.md`.

1. Work on one milestone at a time, in order. Start each session by reading `docs/SPEC.md` and the current milestone's acceptance checks.
2. Before writing a component, write a short plan in `docs/DESIGN.md`: data structures, interfaces, tradeoffs considered. Keep a running log of decisions there.
3. Write tests alongside the code, not after. A milestone is done only when all its acceptance checks pass.
4. Respect the "Not allowed" list. Never add Redis, etcd, a Raft library, a parser generator or an expression library, even to unblock something.
5. Never invent, round up or hand-edit a benchmark number. All numbers come from `bench/` scripts and land in `bench/results/`; `METRICS.md` is generated.
6. If a measured number misses a goal, report it as measured and note the cause in `DESIGN.md`; do not tune the benchmark to hide it.
7. Keep the request hot path free of global locks and measure allocations with `go test -benchmem` when touching it.
8. Small commits with clear messages; one pull request per milestone, with its results summarized in the description.
9. Keep secrets and AWS credentials out of the repo; Terraform reads them from the environment.
10. When unsure about a design choice, write the options in `DESIGN.md` and ask before building. Benchmarks, tests and CI use mockllm only; never call a paid model API from them.
