# Tollgate: Distributed AI Gateway in Go — Build Spec

Nov 13, 2026 · @Someone

## Overview

Tollgate is a distributed AI gateway written in Go: a fleet of gateway nodes that route, protect and rate-limit traffic to LLM providers and other backends, configured by a small rule language that compiles to bytecode, with config replicated by a Raft control plane built from scratch. It is a portfolio project, so every major component must come with a measured result.

**Goals**

- Build each core piece by hand: the rule compiler and VM, the distributed rate limiter and Raft. No Redis, etcd or Raft libraries.
- Solve problems only an AI gateway has: cost-based limits on streamed LLM responses, tail latency across model providers, filtering unsafe output mid-stream, and safe rollout of policy changes.
- Produce honest, reproducible metrics for throughput, latency, accuracy, cost and fault recovery.
- Ship in phases, so the project is presentable after every milestone.

**Core CS areas covered**

| Area | Where |
| --- | --- |
| Compilers and programming languages | Rule language: lexer, parser, type checker, static analysis, optimizer, bytecode VM; Aho-Corasick automaton for stream filtering |
| Distributed systems | Consistent-hash rate limiting with cost reservations, Raft control plane, canary rollout with auto-rollback |
| Operating systems and concurrency | Goroutines, connection pools, lock contention on the hot path |
| Networking | HTTP reverse proxy, streaming (SSE), hedged requests, gRPC between nodes, timeouts, health checks |
| Algorithms and data structures | Token bucket, sliding window, hash ring, Raft log |
| Software engineering | Tests, fuzzing, chaos tests, CI, Docker, Terraform, AWS |

**How to use this doc with Claude Code:** export it as Markdown, save it as `SPEC.md` at the repo root, and ask Claude Code to build one milestone at a time (see Milestones and Working rules).

## Architecture

Tollgate splits into a control plane that stores config and a data plane that handles traffic, the same split Istio and Envoy use. Gateways never block on the control plane: they keep serving with their last good config if it goes down.

&#91;embedded content: Tollgate architecture · control plane, data plane, backends\]

Traffic flows left to right through the gateways; the Raft cluster sits off the request path and only pushes config down.

| Component | Binary | Responsibility |
| --- | --- | --- |
| Control plane | `tollgate-control` | 3-node Raft cluster; stores routes and rules; compiles rules; pushes config to gateways over a gRPC stream |
| Gateway (data plane) | `tollgate-gw` | Reverse proxy; runs compiled rules on each request; load balancing, circuit breaking, retries |
| Rate limiter | inside `tollgate-gw` | Each gateway owns a slice of users on a consistent-hash ring; peers check limits over batched gRPC |
| Admin CLI | `tollgatectl` | Submit rules, view cluster state, trigger benchmarks |
| Mock backend | `mockbackend` | Test service with configurable latency and error rate |

**Request flow**

1. Client sends an HTTP request to any gateway node (behind an AWS load balancer).
2. The gateway matches the route and runs the compiled rule bytecode in its VM (auth checks, tier, limits to apply).
3. The gateway asks the owner node of that user's key on the hash ring whether the request is within its limit. If it owns the key itself, this is a local call.
4. If allowed, the load balancer picks a healthy backend instance; the circuit breaker may reject early if that backend is failing.
5. The request is proxied, with timeout and retry policy, and the response returns to the client.
6. Metrics and a trace span are recorded for every step.

**Config flow:** an admin submits rules via `tollgatectl` → the control plane leader type-checks and compiles them → the change is committed through the Raft log → every gateway receives the new version over its watch stream and swaps it in atomically (hot reload).

**AI request flow:** for a model call, step 3 also reserves the estimated cost on the owner node, step 4 may hedge to a second provider, the response streams back through the safety filter, and when the stream ends the gateway settles the real token cost (components 5–7).

**Rollout flow:** a new config version first goes to one canary gateway; the control plane promotes it to all gateways or rolls it back automatically based on the canary's error rate and latency (component 8).

## Tech stack and hard constraints

Everything is Go plus the standard library, with a short list of allowed dependencies; the interesting parts are hand-built.

| Area | Use | Notes |
| --- | --- | --- |
| Language | Go (latest stable) | `go vet`, `staticcheck`, `-race` in CI |
| Proxy | `net/http`, `net/http/httputil` | Custom transport and connection pool settings |
| Node-to-node RPC | gRPC + Protocol Buffers | `google.golang.org/grpc`, `buf` for codegen |
| Metrics | Prometheus client for Go | Exposed on `/metrics` |
| Tracing | OpenTelemetry Go SDK | Export to Jaeger locally |
| Dashboards | Grafana | Provisioned from JSON in the repo |
| Load testing | k6 | Scripts in `bench/k6/` |
| Containers | Docker, docker-compose | Multi-stage builds, distroless images |
| Cloud | AWS ECS on Fargate, ALB | Defined in Terraform |
| CI | GitHub Actions | Test, lint, fuzz smoke run, build images |

**Not allowed** (these are the parts the project exists to build):

- No Redis, Memcached, etcd, Consul or ZooKeeper.
- No Raft or consensus libraries (`hashicorp/raft`, `etcd-io/raft`, etc.).
- No parser generators or expression libraries (ANTLR, goyacc, CEL, expr). The lexer and parser are handwritten.
- No gateway frameworks (Envoy, Kong, Traefik code).

Allowed for storage: the Raft log and snapshots are plain files written by our own code, with `fsync`.

## Repository layout

One Go module, one package per component, binaries under `cmd/`.

```
tollgate/
  cmd/
    tollgate-gw/          gateway binary
    tollgate-control/     control plane binary
    tollgatectl/          admin CLI
    mockbackend/          test backend
    mockllm/              fake LLM provider: streaming SSE, token counts, configurable latency distribution
  internal/
    proxy/                reverse proxy, routing, retries, timeouts, streaming
    lb/                   round robin, least connections, health checks
    breaker/              circuit breaker
    rules/
      lexer/              tokens
      parser/             Pratt parser -> AST
      types/              type checker
      analysis/           shadowed rules, route overlaps, conflicting limits, instruction bounds
      opt/                constant folding, dead rule elimination
      compiler/           AST -> bytecode
      vm/                 stack VM
      interp/             tree-walking interpreter (benchmark baseline)
    ratelimit/
      bucket/             token bucket, sliding window
      ring/               consistent hash ring with virtual nodes
      peer/               batched gRPC limit checks
      cost/               cost reservations, settlement, expiry
    ai/
      providers/          adapters for OpenAI-compatible APIs and the mock LLM
      tokens/             token estimation from request bodies
      hedge/              hedged requests and cancellation
      filter/             Aho-Corasick streaming safety filter
    raft/                 election, replication, log, snapshots
    control/
      store/              config store on top of raft, watch streams
      rollout/            canary deploy, health comparison, auto-rollback
    sim/                  deterministic simulation: fake clock, fake network, seeds
    metrics/              Prometheus registry and metric names
  api/proto/              .proto files
  bench/
    k6/                   load test scripts
    go/                   Go micro-benchmarks
    chaos/                failure-injection scripts
    results/              raw JSON results (committed)
    report.go             generates METRICS.md from results/
  deploy/
    docker-compose.yml
    grafana/              dashboard JSON
    terraform/            AWS ECS, ALB, VPC
  docs/
    SPEC.md               this document
    METRICS.md            generated, never hand-edited
    DESIGN.md             decisions and tradeoffs, written as we go
```

## Component specs

Eight components, each with a defined interface so it can be built and tested alone.

### 1. Gateway proxy (data plane)

- **Routing:** longest-prefix path match plus optional host and method match. Routes come from the active config version.
- **Load balancing:** round robin, least connections and weighted round robin, chosen per route.
- **Health checks:** active (HTTP probe every N seconds, eject after K failures) and passive (eject after consecutive 5xx or connection errors).
- **Circuit breaker:** closed → open after an error-rate threshold in a rolling window → half-open after a cooldown, allowing a few probe requests → closed on success.
- **Retries:** only for idempotent methods, exponential backoff with full jitter, a retry budget so retries cannot exceed 20% of traffic.
- **Timeouts:** per-route connect, header and total timeouts. Client disconnects cancel the upstream request via `context`.
- **Connection pooling:** tuned `http.Transport` (max idle per host, idle timeout, HTTP/2 to backends when supported).
- **Graceful shutdown:** stop accepting, drain in-flight requests up to a deadline, then exit.
- **Hot path rule:** no global locks on the request path; allocations per request are measured and tracked (see Metrics).

### 2. Rule language and compiler

Admins write policies in a small language. Rules compile to bytecode on the control plane, and gateways run them on a stack VM.

```
let free_tier = user.tier == "free"

route "/api/ai/*" {
  require auth
  when free_tier        { limit 10 per minute by user.id }
  when user.tier == "pro" { limit 200 per minute by user.id }
  when header("X-Debug") != "" { deny 403 }
}

route "/api/public/*" {
  limit 30 per minute by client.ip
}
```

Grammar (EBNF, to be finalized in `DESIGN.md`):

```
program    = { letDecl | route } ;
letDecl    = "let" IDENT "=" expr ;
route      = "route" STRING "{" { stmt } "}" ;
stmt       = "require" "auth"
           | "when" expr "{" { stmt } "}"
           | "limit" INT "per" unit "by" expr
           | "deny" INT
           | "set_header" STRING expr ;
unit       = "second" | "minute" | "hour" | "day" ;
expr       = or ;  (* precedence: || < && < == != < < <= > >= < + - < * / < unary ! - *)
primary    = INT | STRING | BOOL | IDENT | field | call | "(" expr ")" ;
field      = IDENT "." IDENT ;
call       = IDENT "(" [ expr { "," expr } ] ")" ;
```

Pipeline:

1. **Lexer:** hand-written, tracks line and column for every token so errors point to the exact spot.
2. **Parser:** recursive descent for statements, Pratt parsing for expressions; reports multiple errors, not just the first.
3. **Type checker:** types are `int`, `string`, `bool`; checks fields (`user.tier`, `client.ip`), built-in call signatures (`header`, `query`, `cidr_match`) and that `limit ... by` keys are strings.
4. **Optimizer:** constant folding, inlining `let` bindings, removing `when false` blocks and unreachable statements after `deny`.
5. **Compiler:** AST → bytecode with a constant pool; opcodes include `PUSH_CONST`, `LOAD_FIELD`, `CALL_BUILTIN`, `EQ`, `LT`, `AND`, `OR`, `NOT`, `JUMP`, `JUMP_IF_FALSE`, `LIMIT`, `DENY`, `SET_HEADER`, `RETURN`.
6. **VM:** stack-based, no allocations per run in the common case (pooled stacks), with an instruction budget per request so a bad rule cannot hang a gateway.
7. **Baseline interpreter:** a tree-walking evaluator over the same AST. It exists only to benchmark the VM against (experiment E2) and as a correctness oracle in differential tests.

Rules are versioned. A gateway swaps to a new version atomically via `atomic.Pointer`; in-flight requests finish on the old version.

### 3. Distributed rate limiter

Limits are enforced cluster-wide without a central store: each gateway owns the counters for part of the key space.

- **Algorithms:** token bucket (default) and sliding-window counter, selectable per rule.
- **Hash ring:** consistent hashing with virtual nodes (start at 128 per node, make configurable). Membership comes from the control plane.
- **Peer checks:** if the key's owner is another node, the request is forwarded as a gRPC `CheckLimits` call. Calls are batched per peer: flush at 500 µs or 100 checks, whichever comes first (tunable).
- **Failure behavior:** if the owner is unreachable within a short timeout, fall back to a local bucket with the limit divided by the node count, and count it as a degraded decision in metrics.
- **Rebalancing:** when membership changes, only keys whose owner changed move; buckets start fresh on the new owner.
- **Gossip alternative:** dropped. A gossip/CRDT comparison was considered and cut in favor of cost reservations (component 5), which give the limiter a harder, more relevant problem.

### 4. Control plane with Raft

A 3-node cluster stores the gateway config in a replicated log, implemented from the Raft paper (Ongaro and Ousterhout, 2014).

- **Leader election:** randomized election timeouts, terms, `RequestVote` RPC.
- **Log replication:** `AppendEntries` RPC with consistency check and log backtracking; commit when a majority has the entry.
- **Persistence:** term, vote and log entries written to disk with `fsync` before replying; restart recovers from disk.
- **Snapshots:** compact the log after N entries; `InstallSnapshot` RPC for lagging followers.
- **State machine:** a key-value config store (routes, compiled rules, membership), each change getting a monotonically increasing version.
- **Client API:** writes go to the leader (followers redirect); reads are served by the leader after confirming leadership (no stale reads).
- **Watch stream:** gateways open a gRPC stream and receive every committed version; on reconnect they resume from their last version.
- Not in scope: membership changes of the Raft cluster itself (fixed at 3 nodes) unless time allows.

### 5. Cost budgets with reserve-and-settle

An LLM request's cost is unknown until its response finishes streaming, so limits on request count cannot stop a user from running up a large bill with a few huge prompts. Tollgate limits by cost, like a credit-card hold.

```
route "/v1/chat/*" {
  require auth
  budget 5.00 usd per day by user.id
  when request.model == "large" { budget 1.00 usd per hour by user.id }
}
```

- **Reserve:** before forwarding, estimate cost from the prompt's token count plus the request's `max_tokens`, priced from a per-model price table in config. Reserve that amount on the node that owns the user on the hash ring. Reject with 429 if the budget cannot cover it.
- **Settle:** when the stream ends, compute the real cost from the provider's reported usage (or count streamed tokens if usage is missing) and settle: refund the unused reservation or charge the overage.
- **Expiry:** every reservation has a deadline. If the gateway crashes or the stream is cut, the owner releases the reservation at the deadline so budgets never leak.
- **Idempotency:** each reservation has an ID, so a retried settle after a network error is applied once.
- **Rule language additions:** `budget <amount> usd per <unit> by <expr>`, fields `request.model` and `request.max_tokens`, built-in `tokens.estimate(body)`.

### 6. Hedged requests across model providers

LLM API latency is very uneven, and a few slow responses dominate the p99. Following Dean and Barroso's "The Tail at Scale" (2013), Tollgate sends a backup request when the first one is late.

```
route "/v1/chat/*" {
  providers ["provider-a", "provider-b"]
  hedge after p95 max 5%
}
```

- Track a rolling latency histogram per provider and model, measured to first token.
- If the primary has not produced its first token by its p95, send the same request to the next provider. Use whichever produces a first token first and cancel the other via `context`.
- **Hedge budget:** hedges may not exceed the configured share of traffic (5% above), so cost cannot silently double.
- Hedging applies only to requests the rule marks safe to duplicate; the cancelled request's partial cost is still settled against the budget.
- Pairs with the circuit breaker: an open breaker removes a provider from the hedge order.

### 7. Streaming safety filter

A platform for children must catch personal information or unsafe phrases in model output while it streams; waiting for the full response defeats streaming, and a phrase can be split across two chunks.

```
route "/v1/chat/*" {
  block_patterns "kid-safety" action redact
}
```

- Pattern lists are named sets in config. The control plane compiles each set into one **Aho-Corasick automaton**, serialized alongside the rule bytecode.
- The gateway scans each SSE chunk with the automaton and **keeps automaton state across chunks**, so a pattern split across a chunk boundary is still caught.
- To redact across a boundary, hold back at most (longest pattern length − 1) characters before releasing a chunk; measure the added delay.
- Actions: `redact` (replace the match), `block` (end the stream with an error event) or `flag` (emit a metric and log only).
- Case-insensitive matching; Unicode normalization (NFKC) before matching so look-alike characters do not bypass it.

### 8. Safe config rollout

Bad config pushed everywhere at once is a common cause of large outages; Cloudflare's July 2019 global outage came from one bad firewall-rule regex. Tollgate checks config at compile time and rolls it out gradually.

- **Static analysis (compile time, rejects or warns):** rules that can never match because an earlier rule shadows them; overlapping routes with conflicting limits; budgets that can never be satisfied; an upper bound on instructions per evaluation, provable because the language has no loops.
- **Canary:** a committed config version first activates on one canary gateway.
- **Compare:** for a fixed window, the control plane compares the canary's error rate and p99 to the other gateways.
- **Promote or roll back:** within thresholds → promote to all gateways; worse → automatically revert the canary and mark the version rejected in the Raft log, with the reason.
- Every decision is recorded in the Raft log, so the rollout history survives a leader failover mid-rollout.

## Metrics

Every number on the resume must come from a script in `bench/` that anyone can rerun; results are saved as JSON and `METRICS.md` is generated from them, never typed by hand. There are two kinds: live metrics the system exposes, and benchmark experiments that produce the resume numbers.

### Live metrics (Prometheus, shown in Grafana)

| Metric | Type | Labels | Why |
| --- | --- | --- | --- |
| `tollgate_requests_total` | counter | route, status, node | Throughput and error rate |
| `tollgate_request_duration_seconds` | histogram | route, node | End-to-end latency, p50/p95/p99 |
| `tollgate_gateway_overhead_seconds` | histogram | node | Time spent inside the gateway, excluding the backend |
| `tollgate_rule_eval_seconds` | histogram | engine (vm, interp) | Rule evaluation cost per request |
| `tollgate_ratelimit_decisions_total` | counter | result (allow, deny, degraded), node | Limit outcomes and fallback rate |
| `tollgate_ratelimit_peer_rpc_seconds` | histogram | peer | Cost of the extra hop to the key owner |
| `tollgate_ratelimit_batch_size` | histogram | peer | How well batching works |
| `tollgate_cost_reserved_usd_total` | counter | model | Money held before responses finish |
| `tollgate_cost_settled_usd_total` | counter | model | Real spend after settlement |
| `tollgate_cost_estimate_error_ratio` | histogram | model | How far estimates miss real cost |
| `tollgate_reservations_expired_total` | counter | node | Reservations released by deadline (crashes, cut streams) |
| `tollgate_time_to_first_token_seconds` | histogram | provider, model | Provider latency that hedging targets |
| `tollgate_hedges_total` | counter | provider, outcome (won, lost, cancelled) | Hedge rate and how often hedges win |
| `tollgate_filter_matches_total` | counter | pattern\_set, action | Safety filter hits |
| `tollgate_filter_holdback_seconds` | histogram | pattern\_set | Delay added by holding back chunk tails |
| `tollgate_rollout_decisions_total` | counter | outcome (promoted, rolled\_back) | Canary results |
| `tollgate_breaker_state` | gauge | backend | 0 closed, 1 half-open, 2 open |
| `tollgate_backend_healthy` | gauge | backend | Health-check result |
| `tollgate_retries_total` | counter | route | Retry budget usage |
| `tollgate_config_version` | gauge | node | Which config each gateway runs |
| `tollgate_config_propagation_seconds` | histogram | node | Commit on leader → active on gateway |
| `tollgate_raft_term` | gauge | node | Elections over time |
| `tollgate_raft_commit_latency_seconds` | histogram | node | Write latency through consensus |
| `tollgate_raft_is_leader` | gauge | node | Who leads |
| Go runtime (built in) | various | node | Goroutines, GC pauses, heap |

One Grafana dashboard with six rows: traffic, rate limiting, cost and budgets, providers and hedging, resilience (breakers, health, retries, safety filter), control plane (Raft, config versions, rollouts).

### Benchmark experiments (the resume numbers)

| ID | Question | Method | Reported numbers |
| --- | --- | --- | --- |
| E1 | How fast is the gateway? | k6 constant-arrival-rate runs at increasing RPS through 1 gateway and through 3; same runs direct to the backend as a baseline | Max sustained RPS with p99 under a set limit; p50/p99 overhead added vs direct; CPU and memory at that load |
| E2 | Does bytecode beat tree-walking? | Go benchmarks (`testing.B`) on 5 rule sets from small to large; differential test that both engines agree | ns per evaluation and allocations per evaluation for each engine; speedup ratio |
| E3 | How accurate is distributed limiting? | Send a known rate for one key spread across 3 nodes; compare allowed count to the configured limit; with and without batching | Over-admission % vs limit; added p99 latency per request; peer RPCs per request |
| E4 | Does consistent hashing minimize movement? | Add and remove a node with 1M keys and N virtual nodes | % of keys moved vs the ideal 1/N; load spread (max/mean keys per node) |
| E5 | How does Raft behave? | Commit-latency runs; kill the leader during writes (chaos script), measure time to a new leader; check no committed entry is lost | Commit p50/p99; leader failover time p50/max; entries lost (must be 0) |
| E6 | How fast does config reach gateways? | Commit 100 rule changes, record time until all gateways report the new version | Propagation p50/p99 |
| E7 | Does the breaker protect latency? | Make one backend fail 50% of requests mid-run | p99 with breaker on vs off; time from failure to breaker open and from recovery to closed |
| E8 | Do cost budgets hold? | Replay a mix of small and very large prompts against a daily budget through the mock LLM; compare a request-count limit, a cost limit with reserve-and-settle, and kill a gateway mid-stream | Overspend % beyond budget for each method; estimate error; leaked reservations after crashes (must be 0) |
| E9 | Does hedging cut tail latency? | Mock LLM with a heavy-tailed latency distribution; run with hedging off and on at several hedge budgets | Time-to-first-token p99 off vs on; extra cost %; share of hedges that won |
| E10 | What does the safety filter cost? | Stream fixed corpora through the filter with pattern sets of 100, 1,000 and 10,000 entries; include patterns split across chunk boundaries | Throughput MB/s; added delay per chunk p99; split-pattern recall (must be 100%) |
| E11 | Does safe rollout catch bad config? | Push a config whose rule denies 30% of traffic, then one that adds latency; also kill the leader mid-rollout | Time from commit to automatic rollback; % of traffic exposed to the bad config; rollout completes correctly after failover |

### Reporting rules

- Each run records: git commit, machine type (laptop model or AWS instance type), Go version, node count, duration, and the raw k6 or `go test` output, in `bench/results/<experiment>-<date>.json`.
- Each load run lasts at least 60 seconds after a warm-up and is repeated 3 times; report the median run.
- `go run ./bench/report.go` rebuilds `METRICS.md` with one table per experiment and the environment it ran in.
- Numbers measured on a laptop and on AWS are reported separately, never mixed.
- Initial goals to aim for (goals, not results): gateway overhead p99 under 2 ms on a single node at moderate load; VM at least 3× faster than the interpreter on large rule sets; over-admission under 5% for the ring; zero committed entries lost in chaos runs; overspend under 2% with reserve-and-settle; hedging cuts time-to-first-token p99 with under 5% extra cost; bad config exposed to under 1/N of traffic before rollback. Adjust them after the first measurements.

## Testing strategy

Each component is tested alone first, then together in docker-compose, then under failure.

| Layer | What | Tools |
| --- | --- | --- |
| Unit | Every package; table-driven tests | `go test -race` |
| Rule language | Golden files for lexer, parser, type-checker and static-analysis errors; differential tests VM vs interpreter on random programs | `testdata/` goldens, a random program generator |
| Fuzzing | Lexer and parser never panic on any input; VM never exceeds its instruction budget; safety filter never misses a pattern split at any chunk boundary | Go native fuzzing (`go test -fuzz`) |
| Deterministic simulation | The whole cluster (Raft, gateways, limiter, rollout) in one process with a fake clock and fake network driven by a seed; drop, delay, reorder and partition messages; check Raft safety, no leaked reservations, rollout correctness; any failure replays exactly from its seed | `internal/sim`, seeded randomness, thousands of seeds in CI nightly |
| Rate limiter and budgets | Bucket math and reservation expiry with a fake clock; ring distribution and movement properties; idempotent settles | Fake clock, property tests |
| Hedging | Cancellation actually stops the losing request; hedge budget never exceeded | `mockllm` with scripted latencies |
| Integration | Full cluster in docker-compose: 3 control nodes, 3 gateways, 3 mock backends, 2 mock LLM providers | `bench/chaos/` scripts, Go test harness |
| Chaos | Kill the Raft leader, kill a gateway mid-stream, partition a node, slow a provider, push bad config, during a load run | Docker kill/pause, `tc` for network delay |
| CI | Lint, unit tests with race detector, 30-second fuzz smoke run, 200-seed simulation smoke run, image build on every push | GitHub Actions |

Coverage target: 80%+ on internal/ai, `internal/rules`, `internal/raft` and `internal/ratelimit`; report coverage in CI but do not chase it on glue code.

## Deployment

Develop and benchmark locally with docker-compose; deploy to AWS only for the final benchmark runs and a live demo, then tear it down to keep costs low.

**Local (`deploy/docker-compose.yml`)**

- 3 × `tollgate-control`, 3 × `tollgate-gw`, 3 × `mockbackend`
- Prometheus, Grafana (dashboard provisioned), Jaeger
- `make up`, `make down`, `make bench-e1` … `make bench-e7`, `make chaos`

Also run 2 × `mockllm` as `provider-a` and `provider-b`. Real model APIs are optional and only for a short live demo; every benchmark runs against `mockllm`, so results are reproducible and cost nothing.

**AWS (`deploy/terraform/`)**

- VPC with public and private subnets; Application Load Balancer in front of the gateways
- ECS on Fargate: services for gateways, control plane (3 tasks with EBS or EFS storage for Raft data) and mock backends
- Service discovery via AWS Cloud Map so nodes can find each other
- Amazon Managed Prometheus and Grafana, or self-hosted on one small task
- A k6 load-generator task in the same region, so network distance does not distort latency
- `terraform destroy` after each benchmark session; record the AWS instance and task sizes in each result file

Cost note: open question — estimate the hourly cost of the full stack with the AWS pricing calculator before the first deploy, and set an AWS budget alert.

## Milestones

Eight milestones; the project is presentable after M2, becomes an AI gateway at M5, and each later one adds a resume bullet. Do not start a milestone until the previous one meets every acceptance check.

1. **M1: Single-node gateway.** Proxy, routing, load balancing, health checks, circuit breaker, retries, timeouts, graceful shutdown; static YAML config for now; mock backend.
   - Accept: unit tests pass with `-race`; docker-compose brings up 1 gateway + 3 backends; killing a backend shifts traffic within the health-check interval.
2. **M2: Metrics and first numbers.** Prometheus metrics, Grafana dashboard, k6 scripts, results JSON and `METRICS.md` generator; CI pipeline.
   - Accept: E1 and E7 run end to end and `METRICS.md` shows their results with environment details.
3. **M3: Rule language.** Lexer, parser, type checker, static analysis, optimizer, bytecode compiler, VM, baseline interpreter; rules replace the YAML policy; hot reload from a file.
   - Accept: golden tests and fuzzing pass; differential tests show VM and interpreter agree; static analysis catches shadowed rules and route conflicts in its golden tests; E2 results in `METRICS.md`.
4. **M4: Distributed rate limiting.** Token bucket and sliding window, hash ring, batched gRPC peer checks, degraded fallback; 3 gateways in docker-compose.
   - Accept: E3 and E4 results in `METRICS.md`; killing a gateway mid-run keeps limits enforced with degraded decisions counted.
5. **M5: AI gateway.** Streaming (SSE) proxying without buffering, `mockllm`, provider adapters, cost budgets with reserve-and-settle, hedged requests, streaming safety filter, and their rule-language additions.
   - Accept: fuzzing shows no missed split patterns; no leaked reservations after gateway kills; E8, E9 and E10 in `METRICS.md`.
6. **M6: Raft control plane.** Election, replication, persistence, snapshots, config store, watch streams; gateways take config from the control plane instead of files; deterministic simulation harness in `internal/sim`.
   - Accept: simulation passes 1,000+ seeds; chaos run kills the leader with zero committed entries lost; E5 and E6 in `METRICS.md`.
7. **M7: Safe rollout.** Canary activation, health comparison, automatic promote or rollback, rollout state in the Raft log.
   - Accept: a deliberately bad config is rolled back automatically, including when the leader is killed mid-rollout; E11 in `METRICS.md`.
8. **M8: AWS and polish.** Terraform deploy, benchmark runs on AWS, README with the architecture diagram and results, `DESIGN.md` tradeoffs.
   - Accept: `terraform apply` brings up the full stack from scratch; AWS results reported separately from laptop results; `terraform destroy` leaves nothing running.

## Working rules for Claude Code

These rules apply to every session on this repo; copy them into `CLAUDE.md` at the repo root.

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

## Resume bullet templates

Fill each bracket only with a number from `METRICS.md`; pick the 3 strongest once the results are in (likely E1, E8 or E9, and E2). Each follows the resume rules: start with a verb, name the feature and stack, one key number.

- Built a distributed AI gateway in Go with streaming proxying, load balancing and circuit breaking, sustaining **\[X\] req/sec** at **\[Y\] ms p99** across 3 nodes on AWS ECS. *(E1)*
- Designed cost-based budgets for streamed LLM responses using reserve-and-settle on a consistent-hash ring, cutting budget overspend to **\[A\]%** versus **\[B\]%** for request-count limits. *(E8)*
- Implemented hedged requests across model providers, reducing time-to-first-token p99 by **\[C\]%** at under **\[D\]%** extra cost. *(E9)*
- Designed a policy language with a handwritten parser, static analysis and optimizer, compiled to bytecode on a custom stack VM that runs **\[Z\]× faster** than a tree-walking interpreter. *(E2)*
- Built a streaming safety filter that compiles pattern sets into an Aho-Corasick automaton, scanning model output at **\[M\] MB/s** and catching patterns split across chunks. *(E10)*
- Implemented Raft consensus from scratch with canary rollouts that automatically roll back bad config in **\[R\] s**, verified with deterministic simulation across **\[S\]** seeds. *(E5, E11)*

Stack line for the project heading: *Go, gRPC, Protocol Buffers, SSE, Prometheus, Grafana, Docker, Terraform, AWS ECS, k6*.
