# Design log

Decisions and tradeoffs, written as we go. Newest entries at the bottom of each section.

## M1: Single-node gateway — plan

### Packages
- `internal/config` — static YAML config (`gopkg.in/yaml.v3`; the only new dependency in M1, needed because the spec asks for YAML). Validated at load; durations use Go syntax (`2s`, `500ms`).
- `internal/lb` — `Target` (URL, weight, `healthy`, `ejectedUntil`, `inflight`, its breaker), `Pool` (targets + balancer + health checker), balancers.
- `internal/breaker` — per-target circuit breaker.
- `internal/proxy` — router, per-route `httputil.ReverseProxy`, a retrying `RoundTripper`, retry budget, server with graceful shutdown.
- `cmd/tollgate-gw`, `cmd/mockbackend`.

### Hot path: no global locks
- **Router** is immutable after build: routes sorted by prefix length (desc), linear scan. Swapped later via `atomic.Pointer` (M3).
- **Round robin:** one `atomic.Uint64` counter per pool, skipping unavailable targets.
- **Weighted round robin:** smooth WRR (nginx) needs a mutex if run per request. Instead the smooth-WRR sequence is precomputed once into a schedule of target indices (length = sum of weights) and walked with an atomic counter. Same distribution and interleaving, lock-free.
- **Least connections:** scan for min `inflight` (atomic) starting at a rotating offset so ties spread out. O(n) per pick, fine for small pools. Considered power-of-two-choices; deferred until pools are large.
- **Breaker:** rolling window of time buckets with atomic counters (bucket reset by CAS on the bucket's epoch). A racing reset can lose a handful of counts at a bucket boundary — acceptable for a threshold decision, and it avoids a per-backend mutex on every request.

### Health
- **Active:** a goroutine per target probes `GET <health path>` every interval; `unhealthy_threshold` consecutive failures mark it down, `healthy_threshold` successes bring it back.
- **Passive:** `consecutive_failures` 5xx/connection errors in a row eject a target for `eject_duration` (stored as an atomic unix-nano deadline).
- A target is *available* iff healthy, not ejected, and its breaker admits the request.

### Circuit breaker
closed → open when, in the rolling window, requests ≥ `min_requests` and error rate ≥ `error_rate` → half-open after `cooldown`, admitting at most `half_open_probes` concurrent probes → closed after that many successes; any probe failure reopens.

### Retries
- Only idempotent methods (GET, HEAD, OPTIONS, PUT, DELETE, TRACE). Requests with a body are retried only if the body is ≤ 1 MiB (buffered so it can be replayed).
- Retry on connection errors and 502/503/504. Each retry prefers a target not yet tried.
- Backoff: exponential with full jitter, `sleep = rand(0, min(max, base·2^attempt))`, cut short by request cancellation.
- **Budget:** Finagle-style token budget. Every request deposits 0.2 tokens, a retry withdraws 1; balance is capped. Long-run retries ≤ 20% of requests (plus a small initial allowance). Atomic int64 in milli-tokens.

### Timeouts and pooling
- Per route: `connect` (dialer timeout), `header` (`ResponseHeaderTimeout`), `total` (`context.WithTimeout` across all attempts). Each route gets its own `http.Transport` so these can differ; `MaxIdleConnsPerHost`, `IdleConnTimeout` and `ForceAttemptHTTP2` are set from config.
- Client disconnect cancels upstream because the outgoing request carries the inbound request's context.

### Why `httputil.ReverseProxy` + custom `RoundTripper`
ReverseProxy already handles hop-by-hop headers, `X-Forwarded-For`, trailers and flushing (needed for SSE in M5). Target selection, breaker accounting and retries live in the RoundTripper, so the proxy stays stock. Least-conn `inflight` is decremented when the response body is closed, not when headers arrive.

### Graceful shutdown
`SIGINT`/`SIGTERM` → `http.Server.Shutdown` with `shutdown_timeout`: stops accepting, waits for in-flight requests, then health checkers stop.

### Open items
- Docker is not installed on the dev machine yet; the compose file is written but the "kill a backend" acceptance check is also covered by an in-process Go test and a script that runs the real binaries.

## M1: decisions log

- **Upstream Host header.** The proxy forwards the client's original `Host` (Envoy's default), not the target's host. Revisit if a backend needs its own host.
- **Route prefix matching is segment-aware.** `/api` matches `/api` and `/api/x`, not `/apix`; a prefix ending in `/` matches anything under it. Ties at equal prefix length go to routes that constrain host, then method.
- **Breaker accounting on client disconnect.** If the client goes away and the attempt fails, the result is not counted against the backend (`Breaker.Ignore`, no passive failure), so client churn cannot open a breaker.
- **Retry budget cap.** Balance is capped at `max(initial, 100)` retries so a quiet period cannot bank a large retry storm.
- **Retries hide dead backends from clients.** In the kill-backend check the client saw 0 errors, because a refused connection is retried on another target. The chaos check therefore also reports when the gateway itself marks the victim down, via `/admin/upstreams`.
- **First kill-backend test was wrong.** It killed the backend at the same moment as the first health probe, so it "passed" in ~5 ms without measuring the interval. Fixed by killing mid-interval after the first probes settle.

## M1: measured (local, Apple M2, Go 1.25.1)

Not benchmark results for `METRICS.md` (those come from `bench/` in M2), just checks run while building:

- Unit tests pass with `-race`. Coverage: breaker 83.0%, config 88.9%, lb 90.2%, proxy 85.5%.
- Micro-benchmarks (`go test -bench`, parallel): breaker Allow+Record 51 ns/op, balancer pick 38–52 ns/op, all 0 allocs. Full `ServeHTTP` through a loopback backend: ~14.7 µs/op, 114 allocs/op — mostly `net/http` client/server; to be profiled in M2.
- Kill-backend (local processes, `make chaos-kill-backend-local`, 1 s probe interval, 3 runs): gateway marked the killed backend down 67–69 ms after the kill (passive ejection fires before the next active probe); 0 client-visible errors because retries rerouted.
- docker-compose: **not verified** — Docker is not installed on the dev machine.

## M2: Metrics and first numbers — plan

### Live metrics (`internal/metrics`)
- One `prometheus.Registry` per process (not the global default), exposed on the admin listener at `/metrics`, plus Go runtime and process collectors.
- M2 registers the metrics that M1 code can produce: `tollgate_requests_total{route,status,node}`, `tollgate_request_duration_seconds{route,node}`, `tollgate_gateway_overhead_seconds{node}`, `tollgate_breaker_state{backend}`, `tollgate_backend_healthy{backend}`, `tollgate_retries_total{route}`, `tollgate_config_version{node}`. The rest of the spec's table is added by the milestone that creates the behavior, so no metric is exported that is always zero.
- **Hot path:** `*Vec.WithLabelValues` hashes labels and takes a read lock on every call. Handles are resolved once per route at build time (duration, retries) and per (route, status) through a per-route `sync.Map` that is read-mostly after warm-up. No lock is taken per request after the first request with a given status.
- **Gateway overhead** = wall time in `ServeHTTP` − time spent inside upstream `RoundTrip` calls (connect + send + wait for headers, summed over attempts). Retry backoff counts as overhead because the gateway chose to wait. Body streaming after headers is excluded because it is paced by the backend. Considered: taking overhead only from k6 (gateway vs direct): kept too, as a cross-check, because it includes the extra network hop.
- Status capture uses a `ResponseWriter` wrapper with `Unwrap()`, so `http.ResponseController` (used by ReverseProxy to flush SSE in M5) still reaches the real writer.

### Benchmarks
- `bench/k6/` holds the scripts; `bench/run` (Go) orchestrates a run: starts processes (laptop mode) or targets compose, drives k6, injects failures, samples gateway CPU/RSS, and writes `bench/results/<exp>-<date>.json` with the environment (git commit, machine, CPU, Go and k6 versions, node count, duration) and the raw k6 summaries.
- `bench/report.go` regenerates `docs/METRICS.md` from the newest result per experiment, laptop and AWS in separate sections.
- **E1:** k6 `constant-arrival-rate` on a rising RPS ladder for three targets: direct to backend, 1 gateway, 3 gateways (k6 spreads requests across the gateway URLs, standing in for the ALB). A level passes if p99 < limit and errors < 0.1% and k6 kept up with the arrival rate (dropped iterations < 1%); the ladder stops at the first failing level. 3 repetitions per level; the median run is reported.
- **E7:** 3 backends, one switched to 50% errors mid-run and back. Breaker on vs off, otherwise identical config (passive ejection off in both, so the breaker is the only thing reacting). Records p99 and error rate per phase and the times failure→open and recovery→closed, from polling `/admin/upstreams` every 10 ms.
- Laptop caveat: k6, gateways and backends share the same 8 cores, so absolute RPS is a floor set by the laptop, not the gateway alone. The result files record this.

### CI
GitHub Actions: gofmt check, `go vet`, `staticcheck`, `go test -race` with coverage report, build all binaries, `docker build` for each image.

## M2: decisions log

- **Go 1.26.** `prometheus/client_golang` v1.25 requires Go 1.26, so the module and Dockerfile moved from 1.25 to 1.26 (the spec asks for latest stable).
- **Metrics cost.** Pre-resolved handles keep label hashing off the hot path; with metrics on, `ServeHTTP` goes from 114 to 116–117 allocs/op and no measurable ns/op change (`BenchmarkGatewayServeHTTP*`).
- **E7 breaker settings.** With the default `error_rate: 0.5`, a backend failing exactly 50% of requests hovers around the threshold and may never trip. E7 uses `window: 5s, error_rate: 0.3, min_requests: 20, cooldown: 2s, half_open_probes: 3`; these are recorded in the result file. Expected time to open ≈ 0.3/0.5 × 5 s = 3 s, because the window still holds pre-failure successes.
- **E7 failures are fast 500s.** The mock answers injected errors immediately, and 500 is not retried (only 502/503/504 and connection errors are). So the breaker mostly changes the **error rate** clients see; any p99 change is secondary. Both are reported.
- **E7 failure injection runs inside k6** (one-shot scenarios at fixed offsets), so the phase boundaries in k6's tagged metrics match the moment the backend changes. The runner timestamps the change by polling the backend, and timestamps breaker transitions by polling `/admin/upstreams` (10 ms resolution).
- **Laptop noise.** The dev laptop runs other apps (load average 3–6 before benchmarks). Repeated identical runs at 1000 req/s through one gateway gave client p99 of 2.8, 13 and 37 ms, while the gateway's own overhead stayed ≤ 1.6 ms for every request and k6 dropped iterations, so the tail comes from scheduling on a shared machine, not the gateway. The E1 pass limit (p99 < 10 ms) was chosen in the plan before any data and was not changed. Instead, a 500 req/s level was added and every result now records the load average at start. AWS runs (M8) are the clean numbers.
- **CI.** gofmt, vet, staticcheck v0.8.2, race tests with a coverage summary, binary build, and `docker build` per image. The first run on GitHub passed, which is the first check that the Dockerfile builds.
- **Machine sleep invalidated the first full run.** With the lid closed on battery the laptop slept between runs, which k6 counted as thousands of dropped iterations. Nothing was recorded. The runner now discards any k6 run that takes 20 s longer than planned and records AC vs battery. The final E1 started on battery; E7 ran on AC.

## M3: Rule language — plan

### Scope split: YAML vs rules
YAML keeps infrastructure: listeners, upstreams, routes to upstreams, timeouts, retries. The rule file holds **policy**: auth, limits, deny, headers. A rule `route "<pattern>"` block selects which policy runs for a request path; it does not pick an upstream. (Upstreams move to the control plane in M6.)

### Finalized grammar (EBNF)
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
expr       = binary | unary | primary ;
binary     = expr op expr ;         (* Pratt; precedence low→high: || , && , == != , < <= > >= , + - , * / *)
unary      = ( "!" | "-" ) expr ;
primary    = INT | STRING | "true" | "false" | IDENT | field | call | "(" expr ")" ;
field      = IDENT "." IDENT ;
call       = IDENT "(" [ expr { "," expr } ] ")" ;
```
- Comments: `#` to end of line. Strings: double-quoted with `\" \\ \n \t` escapes.
- Route pattern: `"/x/*"` matches `/x` and anything under it (segment-aware, like M1 prefixes); without `*` it matches exactly. The most specific route (longest literal prefix, exact beats wildcard) runs; only one route runs per request.

### Semantics
- Statements run in order. `deny N` ends evaluation with status N (400–599). `limit` statements accumulate, and every applicable limit must admit the request. `set_header` accumulates request headers added before proxying. `require auth` denies 401 if the request is not authenticated.
- **Auth (M3 choice):** static API keys in the YAML (`auth.api_keys: [{key, user_id, tier}]`), sent as `Authorization: Bearer <key>`. This fills `user.id` and `user.tier`; unauthenticated requests see empty strings. Considered trusting `X-User-*` headers from an upstream identity proxy (simpler, but unsafe without one) and JWT (more work, no extra learning for this project). Swappable later.
- **Fields:** `user.id`, `user.tier`, `client.ip`, `request.method`, `request.path`, `request.host` (all string). **Builtins:** `header(string) string`, `query(string) string`, `cidr_match(ip string, cidr string) bool`.
- **Types:** `int`, `string`, `bool`. `== !=` need equal types; `< <= > >=` and `+ - * /` need ints; `&& || !` need bools; `when` needs bool; `limit … by` key must be string; `set_header` value must be string.
- **Runtime errors** (division by zero, VM budget exceeded): fail closed with 500 and a counted metric. Constant division by zero is a compile error.

### Pipeline (one package each under `internal/rules/`)
1. `lexer`: hand-written, every token carries line:col.
2. `parser`: recursive descent for statements, Pratt for expressions; on error, skips to the next statement or `}` and keeps going, so one pass reports several errors.
3. `types`: resolves `let` (must be defined before use, no redefinition), fields and builtins; reports all errors with positions.
4. `analysis`: (a) **shadowed rules**: a `when` block after an unconditional `deny`, or after `when C { deny … }` where its own condition implies `C` (syntactic implication: equal, or a conjunction containing `C`), can never have effect; (b) **route conflicts**: duplicate route patterns (error); overlapping patterns whose `limit`s on the same key and unit disagree (warning); (c) **instruction bound**: the language has no loops or backward jumps, so the longest path through a route's bytecode is at most its length; reject routes whose bound exceeds the VM budget; (d) unused `let` (warning).
5. `opt`: inline `let`s, fold constants (arithmetic, comparisons, `&&`/`||` with a constant side, `!`), drop `when false` blocks, turn `when true { … }` into its body, remove statements after an unconditional `deny`.
6. `compiler`: AST → `[]Instr{Op, Arg}` per route, with a constant pool. Opcodes: `PUSH_CONST LOAD_FIELD CALL_BUILTIN EQ NE LT LE GT GE ADD SUB MUL DIV NEG NOT JUMP JUMP_IF_FALSE AND/OR` (short-circuit via `JUMP_IF_FALSE`/`JUMP_IF_TRUE` + `POP`), `REQUIRE_AUTH LIMIT DENY SET_HEADER RETURN`.
7. `vm`: stack machine with a fixed-size value stack in a pooled frame; values are a small tagged struct (no interfaces, no boxing). Header names are canonicalized at compile time, so `header()` is a direct map lookup with no allocation. Per-request instruction budget.
8. `interp`: tree-walking evaluator over the same (optimized) AST, used as the E2 baseline and as the oracle in differential tests.

### Testing
Golden files under each package's `testdata/` (input `.tg`, expected tokens / AST dump / errors). Fuzz targets: lexer and parser never panic; VM never exceeds its budget. Differential test: a random generator of well-typed programs and requests; interpreter on the unoptimized AST, interpreter on the optimized AST, and the VM must give identical decisions.

### Integration
- `rules_file` in the gateway YAML. Compiled rules sit in an `atomic.Pointer` next to the router; a file watcher (mtime poll, 1 s) recompiles and swaps. On a compile error the old version keeps serving and the error is logged. `tollgate_config_version` increments on each swap. In-flight requests finish on the version they started with.
- `limit` is enforced in M3 by a local in-memory token bucket per (route, limit, key). M4 replaces it with the distributed limiter. `tollgate_rule_eval_seconds{engine}` is recorded.

### E2
`bench/go/rules_bench_test.go`: 5 rule sets from small to large (generated deterministically), VM vs interpreter, `-benchmem`. `go run ./bench/run e2` runs it 3× (`-count=3`), takes the median, and writes the result file.

## M2: measured results (laptop; full tables in METRICS.md)

- **E1:** 1 gateway and 3 gateways each sustained 15,000 req/s with p99 < 10 ms; direct passed the top level tested (20,000). Added median latency ≈ 0.05 ms from 2,500 to 15,000 req/s. In-gateway overhead ≈ 25 µs p50 / ≤ 50 µs p99 (bucket resolution). 1 gateway used 1.23 cores at 15k. **Goal check:** "overhead p99 under 2 ms at moderate load" holds for the in-gateway overhead (≤ 50 µs); end-to-end added p99 is dominated by laptop noise (it ranges from −1.7 to +1.0 ms below 15k).
- **3 gateways do not beat 1 on the laptop.** At 20,000 req/s, 3 gateways collapsed (p99 976 ms, 7.5% errors, 483k dropped iterations), because k6, 3 gateways and 3 backends compete for 8 cores (load average 32.9 right after). This is a property of the test machine, not a gateway limit; multi-node scaling has to be measured on AWS (M8).
- **E7:** with one backend failing 50%, the breaker cut the error rate clients saw from 16.7% to 1.4% (median runs). It opened 2.86–3.08 s after the failure (expected ≈ 3 s from window 5 s × 0.3/0.5) and closed 1.0–1.6 s after recovery (cooldown 2 s, so it closes on the next half-open probe round). It re-opened ~15 times during the 40 s failing phase, since half-open probes keep hitting 50% errors. Failing-phase p99 was 6.2 ms on vs 6.4 ms off: as predicted, fast 500s don't hurt latency, so the breaker's benefit here is errors, not p99.


## M3: decisions log

- **Policy vs routing.** The YAML still maps paths to upstreams; the rule file only decides policy. A request with no matching rule route is allowed (no policy), which keeps M1 behavior when `rules.file` is unset.
- **All ints are compile-time constants.** Fields and built-ins return only strings and bools, so every `int` expression folds in the optimizer. Constant division by zero is a compile error and no runtime arithmetic error is possible. The VM and interpreter still check, for malformed bytecode or unoptimized ASTs.
- **Short-circuit `&&`/`||`** compile to `AND`/`OR` jump instructions that keep the deciding value on the stack, so the VM never evaluates the right side unnecessarily. The opcode list in the spec names `AND`/`OR`; they are implemented as short-circuit jumps rather than strict binary ops.
- **Instruction bound.** Every jump is forward, so instructions executed ≤ bytecode length. The analyzer rejects a route whose bytecode is longer than the VM budget, and the VM also counts steps at runtime (belt and braces, covered by `FuzzVMBudget`).
- **Shadowing is syntactic.** `when B` after `when A { deny }` is reported when B implies A by structural equality after inlining lets, or when B is a conjunction containing A (or A a disjunction containing B). This is sound but incomplete: it never reports a false shadow, but misses semantic ones like `x == "a"` vs `x != "b"`.
- **Overlapping routes.** The most specific route runs alone, so a narrower route silently replaces a broader one's policy. The analyzer warns when that drops a `require auth` or changes a limit on the same key and unit.
- **Limits and reloads.** Bucket identity is (limit ID, key). IDs are assigned in source order, so after a reload a bucket may continue under an edited limit. Its capacity and rate are updated on the next request. Acceptable for M3; M4 moves buckets to the ring.
- **Limit consumption.** Every applicable limit is checked in order; if a later one refuses, earlier buckets keep the token they gave. This slightly over-counts against those keys and is the common behavior of layered limiters.
- **Auth key handling.** The gateway strips `Authorization: Bearer <gateway key>` after authenticating, so gateway API keys never reach backends. Rules can forward identity explicitly (`set_header "X-User-Id" user.id`).
- **client.ip** is the socket peer address. `X-Forwarded-For` is ignored until there is a trusted-proxy list (M8, behind the ALB).
- **Hot reload** polls the file every `reload_interval` (stdlib only, survives rename-on-save). A broken file is logged and the previous version keeps serving; a broken file at startup stops the gateway.
- **Bucket memory.** A minute ticker drops buckets that have refilled to capacity, since they are indistinguishable from new ones. That handles `per day` limits correctly, where a fixed idle timeout would not.

## M3: measured results (E2, laptop; full table in METRICS.md)

- **Goal missed: the VM is not faster than a fair interpreter.** Final E2 (`e2-20261115T033800Z.json`, clean tree at `8eb7652`), xs to xl: VM/interpreter speedup 0.90×, 1.01×, 1.00×, 1.01×, 0.97×. An earlier run of the same code before it was committed (`e2-20261115T033218Z.json`, marked dirty) gave 0.86–1.03×. Both engines allocate nothing per evaluation. The goal was ≥ 3× on large rule sets.
- **The first E2 run is superseded, and why.** `e2-20261115T032952Z.json` showed 1.5–2.05×. Profiling showed ~40% of interpreter time was runtime name resolution: `LookupField(obj, name)` built `"user"+"."+"tier"` and hit a map on every field access, plus a builtin map lookup per call. That made the interpreter a strawman. Field and builtin IDs are now resolved once by the type checker and stored on the AST, and both engines use them, so the comparison is fair. The ratio dropped to ~1×. Both files are kept; METRICS.md shows the newest.
- **Shared builtin cost reduced for both engines.** The client IP is now parsed once per request instead of on every `cidr_match` call (it was the single largest shared cost). The prefix argument is still parsed per call.
- **Where the VM loses** (pprof, xl rule set): of 2.18 s in `vm.Run`, ~420 ms is the dispatch loop and switch, 190 ms the per-instruction `d.Err` check, 140 ms copying 32-byte `Value`s from the constant pool, ~200 ms defensive stack-bound checks. The interpreter does the same work in fewer, larger steps: `user.tier == "free"` is one node visit with direct field and constant access, versus 3 dispatched VM instructions.
- **Ways to make the VM win (not done; needs a decision):** (1) drop the per-instruction budget check when `len(code) ≤ budget`, which the analyzer already proves; (2) superinstructions for the dominant shapes (`EQ_FIELD_CONST`, `NE_HEADER_CONST`) and fused compare-and-jump; (3) errors returned only from the opcodes that can fail; (4) stack checks moved to load time (compiler-verified `MaxStack`); (5) pre-parse constant CIDR prefixes at compile time. (1), (3) and (4) are about removing defensive overhead; (2) is the classic VM speedup.

## M3: VM optimization round (decided with the user after the ~1× result)

- **What changed.** (1) Jump-mode code for conditions: `when a && b` compiles to conditional jumps and pushes no booleans. (2) Superinstructions, each fused with a jump in jump mode: `FIELD_EQ/NE_CONST`, `HEADER_EQ/NE_CONST`, `CIDR_FIELD`. (3) A bytecode verifier (`vm.Validate`) proves operand ranges, forward jumps and the exact stack depth at every pc, so `Run` drops per-instruction stack and error checks; a `recover()` turns any panic on unverified code into `ErrBadBytecode`. (4) Shared by both engines, so the baseline stays fair: constant CIDR prefixes are parsed at compile time, and the query string is split once per request into a fixed array (no allocation).
- **Result** (`e2-20261115T045308Z.json`, clean tree at `18b89aa`): speedup 0.99× (xs), 1.85× (s), 2.39× (m), 2.50× (l), **2.56× (xl)**; both engines 0 allocs/eval. The spec example route went from 35 to 19 instructions.
- **Goal (≥ 3× on large rule sets) still missed: 2.56×.** What remains in the VM profile is mostly work both engines must do: string compares on fields and headers, header map lookups, prefix containment. A bigger gap would need the interpreter to be worse, not the VM better. The xs set (2 statements, 16 instructions) stays at ~1× because fixed per-call cost dominates.
- **History kept:** `e2-…032952Z` (1.5–2.05×, strawman interpreter), `e2-…033218Z` (dirty tree) and `e2-…033800Z` (0.90–1.01×, fair baseline before VM work) remain in `bench/results/`; METRICS.md shows the newest.

## M4: Distributed rate limiting — plan

### Packages
- `internal/ratelimit/bucket`: token bucket (M3) plus a **sliding-window counter**: two fixed windows; the estimate is `prev × (1 − elapsed/per) + curr`, and a request is admitted while the estimate is below the limit. Same sharded store, with an algorithm per call.
- `internal/ratelimit/ring`: consistent hash ring. FNV-1a 64 of `"<node>#<i>"` per virtual node (default 128), passed through a splitmix64 finalizer for an even spread. Points are sorted; `Owner(key)` is a binary search. The ring is immutable and swapped through an `atomic.Pointer` on membership change. It must be deterministic across processes, so it cannot use `maphash`, which is seeded per process.
- `internal/ratelimit/peer`: the gRPC `CheckLimits` client and server. One **batcher** goroutine per peer collects checks and flushes at 100 checks or 500 µs, whichever comes first (both tunable; `max_batch: 1` disables batching). Each caller waits on its own pooled result channel, with a short timeout.
- `internal/ratelimit` (`Limiter`): `Allow(ctx, LimitReq) → {allowed, degraded}`. Owner = `ring.Owner("<limitID>|<key>")`. If that's this node, check the local store. Otherwise check the peer, and if the peer fails or times out, fall back to a **separate** local store with the limit divided by the node count (at least 1), counted as degraded.
- `api/proto/tollgate/v1/ratelimit.proto`, generated with `buf` into `api/gen/`, and the generated code committed so builds need no protoc.

### Wire format
`CheckRequest{repeated Check}`, where `Check{limit_id, key, count, per_ns, algorithm}`. The owner applies the parameters it receives, so a gateway that has already reloaded rules is not blocked by a peer still on the old version. `CheckResponse{repeated bool allowed}`, in the same order.

### Rule language
`limit 10 per minute by user.id using sliding_window`, where `using` is optional and token bucket is the default. It flows through AST → compiler `LimitInfo` → `env.LimitReq.Algo`, in both engines.

### Membership (static until M6)
YAML `cluster: {self, listen, vnodes, peers: [{name, addr}], batch: {max, flush}, peer_timeout}`. With no `cluster` block the limiter is purely local (M3 behavior).

### Failure behavior and rebalancing
- An unreachable owner falls back as above. There's no failure detector yet: each call pays at most `peer_timeout` (default 20 ms), so a dead peer costs latency until M6 membership removes it. Considered a circuit breaker per peer (reusing `internal/breaker`) to skip dead peers immediately, and adopted it: an open breaker goes straight to fallback.
- When membership changes, only keys whose owner changed move, and their buckets start fresh on the new owner. Old state ages out through `Sweep`.

### Metrics
`tollgate_ratelimit_decisions_total{result=allow|deny|degraded,node}`, `tollgate_ratelimit_peer_rpc_seconds{peer}`, `tollgate_ratelimit_batch_size{peer}`.

### Experiments
- **E3** (laptop, local processes): one key, 3 gateways, k6 sends a known rate round-robin across them. Over-admission = (allowed − ideal) / ideal, where ideal = burst + limit × duration. Runs with batching and without (`max_batch: 1`). Also reports added p99 vs a single gateway enforcing locally, and peer RPCs per request (from metrics). A **kill run** sends traffic only to gw-1/gw-2 for a key owned by gw-3, kills gw-3 mid-run, and reports degraded decisions and over-admission.
- **E4** (pure Go): 1M keys; add a 4th node to 3 and remove one from 4; vnodes ∈ {1, 16, 64, 128, 256}. Reports % of keys moved vs the ideal 1/N and the max/mean keys per node.

## M4: decisions log

- **Ring hash.** FNV-1a 64 over `"<member>#<i>"` with a splitmix64 finalizer. FNV alone clusters similar short strings; the finalizer is what makes 128 vnodes spread within ±8%. `maphash` was rejected because it is seeded per process, and every gateway must agree on owners.
- **Wire carries limit parameters.** `Check` includes count, period and algorithm, so the owner enforces what the caller compiled; gateways mid-reload don't disagree about a key's limit.
- **Breaker per peer.** A dead owner first costs one `peer_timeout` (20 ms) per decision; after 5 failures the peer's breaker opens and decisions fall back immediately (tested: < 10 ms with the breaker open). It half-opens after 1 s to notice recovery.
- **Fallback share = limit / N** with N = current ring size, at least 1. During an outage the cluster admits less than the limit (two survivors × 1/3), never more, except for each fallback bucket's initial burst.
- **E3 offered rate.** At 300 req/s against 100/s every batch held exactly one check (remote checks ~10 ms apart vs a 500 µs flush window), so "with vs without batching" measured nothing. E3 uses 6,000 req/s against 1,000/s (same 6× overload ratio as planned; the absolute rate was raised before any recorded run).
- **Kill timing.** The runner first measured the kill time from k6 launch, which put the kill ~0.5 s before k6's phase boundary and showed a false 17% over-admission "before" the kill in a smoke run. Phase timing is now anchored on the first request reaching a gateway.
- **Toolchain.** Homebrew installed Go 1.27.4 as a dependency during this milestone and it now precedes `/usr/local/go` (1.26.1) on PATH. E3 and E4 binaries were built with 1.27.4 (recorded in their result files); E1, E2 and E7 used 1.26. CI pins 1.26 via `go.mod`.

## M4: measured results (laptop; full tables in METRICS.md)

- **E3** (6,000 req/s offered against 1,000/s, 60 s × 3 runs, median by over-admission): 1 gateway −0.00% (exact); **3 gateways batched +0.07%** (runs: +0.03, +0.42, +0.07); **3 gateways unbatched +0.31%** (+0.21, +0.46, +0.31). Goal "under 5%" met. Over-admission comes from degraded decisions, when a peer check exceeds 20 ms under CPU saturation and the fallback bucket's initial burst admits a few extra. Batching halves peer RPCs per request (0.666 → 0.346, mean batch 1.93) and, in the full runs, also had fewer degraded decisions (44 vs 227 in the medians), because fewer RPCs means less CPU contention. A shorter 20 s probe run showed the opposite, so the comparison of degraded counts is noisy at this scale.
- **Added p99** is within laptop noise: −0.92 ms (batched) and +2.50 ms (unbatched) vs the single gateway in the median runs. The isolated latency probe (no k6, no gateways) is the cleaner number: a loopback peer check costs p50 0.13 ms / p99 0.25 ms unbatched and p50 0.64 ms / p99 0.86 ms batched at 2,000 checks/s, so batching trades ~0.5 ms of latency for fewer RPCs. That trade pays off when RPCs are expensive (across machines), not on loopback.
- **Owner killed mid-run** (acceptance): before the kill +0.01–0.40%; after, admitted 27,306 of an ideal 40,000 (−31.7%, identical in all 3 runs), i.e. two survivors × limit/3 plus their fallback bursts. All ~240k post-kill decisions were counted as degraded. Limits stay enforced in the safe direction.
- **E4** (1M keys): no key ever moved anywhere except to or from the changed node, at any vnode count. Keys moved when adding a 4th node: 12.7% (1 vnode), 31.0% (16), 18.0% (64), **23.5% (128)**, 23.8% (256), against an ideal 25%. Max/mean keys per node on 3 nodes: 1.54, 1.18, 1.09, **1.08**, 1.04. Removing the node is exactly symmetric by construction (the 4→3 ring is the 3-node ring), so its numbers equal the add case.
- **Hot-reload fail-open bug (found by CI in M4, fixed in M3 and M4).** The watcher could read the rules file between an editor's truncate and write, get an empty file, and load it as a valid zero-route program, dropping all policy until the next poll. Reloads now require identical content on two consecutive polls (one extra interval of delay) and refuse an empty file (`ErrEmptyRules`).

## M5: AI gateway — plan

### Request path for an AI route
1. Read the body (≤ 1 MiB) and parse the OpenAI-style request (`model`, `max_tokens`, `stream`, `messages`).
2. Evaluate rules with the new fields (`request.model`, `request.max_tokens`, `body`). The decision now also carries **budgets**, and the matched route carries **providers**, **hedge** settings and **block_patterns**.
3. Apply `limit`s as before; then **reserve** each budget's estimated cost on the owner node. A failure releases the ones already taken and answers 429.
4. Send to the first healthy provider; **hedge** to the next one if no first token by that provider's p95, within the hedge budget. The first to produce a token wins; the loser is cancelled through its `context`.
5. Stream SSE to the client **without buffering**, passing content through the **safety filter**, which keeps automaton state across chunks.
6. When the stream ends (or the client leaves), **settle** each reservation with the real cost from the provider's `usage`, or from counted tokens if usage is missing. The cancelled hedge's prompt cost is settled too.

### Packages
- `internal/ai/tokens`: estimate tokens from text (≈ 4 bytes per token, at least 1 per word); price table → micro-USD (`int64`, no floats for money).
- `internal/ai/filter`: Aho-Corasick over NFKC-normalized, lower-cased runes. `Stream` keeps the automaton state across chunks and holds back the last (longest pattern − 1) runes before releasing them, so a match that spans a chunk boundary can still be redacted. Actions: `redact` (each matched rune → `*`), `block` (stop the stream), `flag` (metric only). NFKC uses `golang.org/x/text/unicode/norm` (already in the module graph via Prometheus; quasi-standard library). **Tradeoff:** filtered output is NFKC-normalized text, because redaction positions are defined on the normalized stream.
- `internal/ratelimit/cost`: budget state per (budget ID, key) on the owner: a balance in micro-USD refilling `amount` per period (the same model as the token bucket), with reservations `{id, amount, deadline}`. `Reserve` takes the estimate if the balance covers it. `Settle(id, actual)` refunds `reserved − actual`; overage can drive the balance negative, which blocks later requests until it refills. Settles are **idempotent**: a settled ID is remembered past its deadline. **Expiry** releases (refunds) a reservation at its deadline, so a crashed gateway or cut stream never leaks budget. RPCs `Reserve`/`Settle` are added to the same gRPC service and routed through the ring; an unreachable owner falls back to a local budget at amount/N (degraded), as for limits.
- `internal/ai/providers`: OpenAI-compatible adapter: build the upstream request (`stream: true`, `stream_options.include_usage`), parse SSE `data:` events into content deltas and usage, and re-emit them.
- `internal/ai/hedge`: rolling time-to-first-token histogram per provider and model (fixed buckets, atomic counters, recent window); p95 lookup; hedge budget as a token budget like the retry budget (share of requests). The breaker on a provider removes it from the hedge order.
- `cmd/mockllm`: OpenAI-compatible streaming server. TTFT is log-normal with a configurable heavy tail (probability and multiplier); tokens per second; output length a configurable fraction of `max_tokens`; prompt tokens counted by words (deliberately different from the gateway's estimator, so estimate error is real); optional missing `usage`; optional injected phrases split across chunks; usage totals on `/admin/usage`.

### Rule language additions
```
budget 5.00 usd per day by user.id
providers ["provider-a", "provider-b"]
hedge after p95 max 5%
block_patterns "kid-safety" action redact
```
- `budget` may appear inside `when`; `providers`, `hedge` and `block_patterns` are route settings and must be top-level (type error otherwise).
- New decimal literal for USD amounts (lexer `FLOAT`), valid only in `budget`.
- `request.model` (string), `request.max_tokens` (**int**: the first runtime int), `body` (string), `tokens.estimate(string) int`.
- **Consequence:** runtime ints mean runtime arithmetic errors are now possible (`x / request.max_tokens`). Both engines already fail closed on them. The optimizer may still drop a pure subexpression whose value doesn't matter (`e && false`), which can elide an error the unoptimized program would hit; this is documented semantics (errors in dead pure subexpressions are not guaranteed). The differential generator divides only by non-zero literals.

### Config
`pricing: {<model>: {input_per_1k_usd, output_per_1k_usd}}`, `pattern_sets: {<name>: [phrases…]}`, route `type: ai` with providers mapped to upstream names; `reservation_ttl` (default 2 min).

### Experiments
- **E8:** a Go driver replays a seeded mix of small and very large prompts for one user against a budget, in three setups: request-count limit sized to the mix's average cost, cost budget with reserve-and-settle, and cost budget with the streaming gateway killed mid-run. Reports overspend % (true spend from `mockllm`'s usage totals vs the budget), estimate error, and reservations still outstanding after every deadline (must be 0).
- **E9:** `mockllm` with heavy-tailed TTFT; hedging off vs on at 1%, 5% and 10% budgets. Reports TTFT p99, extra cost %, and the share of hedges that won.
- **E10:** Go benchmark streaming fixed corpora in random-size chunks through pattern sets of 100, 1,000 and 10,000 entries, with patterns planted across chunk boundaries. Reports MB/s, p99 per-chunk processing time, holdback, and split-pattern recall (must be 100%).

## M5: decisions log

- **One automaton per pattern set, over normalized text.** Patterns and stream are NFKC-normalized and lower-cased rune by rune (1:1, so positions line up). Holdback is in runes: the last (longest pattern − 1) runes wait for the next chunk. Redaction replaces matched runes with `*`; `block` drops the held text and ends the stream with an error event; `flag` only counts. Filtered output is NFKC-normalized (tradeoff accepted: redaction positions are defined on the normalized stream).
- **Fuzzing found no missed split patterns** (~980k execs at first, then in CI); a timing test guards against pathological inputs (combining-mark runs, long partial matches, 1-byte chunks stay linear).
- **Money is int64 micro-USD.** Budget balances refill continuously like a token bucket and may go negative after an overage, which blocks new holds until it refills.
- **Expiry refunds the hold; a late settle still charges.** If a stream outlives its TTL the hold is released at the deadline, and the settle that eventually arrives charges the real cost in full. If the gateway died, no settle ever arrives, so the partial cost the provider billed for the cut stream is never charged: E8 measures this as a small overspend in the kill run. The alternative, charging the whole hold on expiry, would overcharge users for gateway crashes. Kept the spec's "release at the deadline" semantics.
- **Estimates are conservative.** The hold covers the prompt estimate (≈4 bytes/token) plus the full `max_tokens`, so holds exceed real cost (E8 measures ~+80–90%). That makes overspend from concurrency impossible except through expiries and degraded decisions, at the price of rejecting some requests the budget could have afforded.
- **Hedging records the loser's lower bound.** When the backup wins, the primary's elapsed time is fed to its TTFT window, so its p95 does not drift down from only ever recording winners.
- **A cancelled hedge pays its prompt.** The settle adds the prompt cost of every cancelled attempt, because providers bill prompts they received.
- **Stream through, don't buffer.** Every provider event is flushed to the client as soon as it is read (filtered content after the holdback). Non-streaming clients get one JSON body assembled at the end, since the gateway always streams upstream.
- **Runtime ints arrived** (`request.max_tokens`, `tokens.estimate`). Both engines already fail closed on runtime arithmetic errors; the optimizer may still drop a pure subexpression whose value is irrelevant (`e && false`). The differential generator now covers runtime ints and budgets.
- **Test-found issues fixed on the way:** the local deploy config referenced providers that did not exist (caught by the new deploy-config test); E8's first kill happened after the budget was already exhausted, so nothing was in flight. The kill now happens at 10% of the schedule.

## M5: measured results (laptop; full tables in METRICS.md)

- **E8** ($100/day budget, 400 requests, 80/20 small/huge mix, median of 3): request-count limit **+14.8%** overspend (runs +14.8, +15.5, −7.6: the count limit's outcome depends on how many huge prompts land inside it); reserve-and-settle **−0.17%** (−0.17, −0.07, −0.31), i.e. never over budget, while admitting more requests (87–99 vs 66–67); reserve-and-settle with the streaming gateway SIGKILLed mid-run **+4.65%** (+4.51, +4.65, +10.60). **Leaked holds after every deadline: 0 in all 9 runs.** The kill-run overspend is the unsettled partial cost of the 1–2 cut streams whose holds expired (one large request ≈ $4–6), exactly the tradeoff above. Mean estimate error +77% to +93% (holds reserve the full `max_tokens`). Goal "overspend under 2% with reserve-and-settle" holds without crashes; with a gateway crash it is 4.5–10.6%.
- **The count-limit runs that admitted 67 instead of 66** each show one degraded limit decision (a peer check that missed its 20 ms deadline under load and fell back locally), confirming the earlier guess.
- **E9** (40 req/s, TTFT median 100 ms with 5% of requests ×10, 2,400 requests per run, median of 3): TTFT p99 off **1,291 ms**; hedge max 1% 1,205 ms (−6.6%, +0.12% cost); **max 5% 660 ms (−48.9%) for +0.77% cost**; max 10% 535 ms (−58.6%) for +0.88% cost. p50 unchanged (~103 ms). The backup won 80–98% of hedges. Goal "cut p99 with under 5% extra cost" met.
- **Why 10% barely beats 5%:** hedges fire only after the primary's p95, so at most ~5% of requests are ever late enough to hedge; the measured hedge rate at max 10% was 5.5–5.7%. A larger budget only helps if the trigger quantile drops too. Run-to-run variance is large at p99 (5%: 383–711 ms) because a 2,400-request run has only 24 samples above p99.
- **E10:** see the earlier entry; 27–35 MB/s, ~2 µs p99 per chunk, 100% split recall.

## M6: Raft control plane — plan

### Shape: a pure Raft core, two drivers
`internal/raft` is a **deterministic state machine**: no goroutines, no clock, no I/O. It exposes `Tick()`, `Step(Message)`, `Propose(data)`, `ReadIndex(ctx)` and `Ready()`/`Advance()` (the etcd-style protocol). A `Ready` carries what must happen next, in order: the hard state and new entries to **persist (fsync) before** any message in the same `Ready` is sent, a snapshot to install, messages to send, committed entries to apply, and confirmed reads. Two drivers run the same core:
- `raft.Server`: real time (ticker), file storage with fsync, gRPC transport.
- `internal/sim`: fake clock, fake network and in-memory storage, all driven by one seed. Any failure replays exactly from its seed.

### Raft (Ongaro & Ousterhout 2014), fixed 3-node membership
- **Election:** randomized timeout in [E, 2E) ticks, `RequestVote` with the log up-to-date check, a no-op entry on winning.
- **Replication:** `AppendEntries` with the prev-index/term consistency check; on reject the follower returns its last index as a hint and the leader backs `next` off to `min(next − 1, hint + 1)`. Commit only entries of the current term once a majority match (§5.4.2). Empty `AppendEntries` serve as heartbeats.
- **Persistence:** term, vote and entries are fsynced before replying; restart replays them.
- **Snapshots:** the application snapshots after N applied entries; the log is compacted to the snapshot index; `InstallSnapshot` for followers whose `next` is below the first retained index.
- **Linearizable reads:** `ReadIndex`: the leader records its commit index, confirms leadership with one heartbeat round acked by a majority, and serves the read once that index is applied. Followers redirect.

### Storage (`internal/raft/wal`)
An append-only log file of CRC-checked records (hard state; entry). On replay a later entry at index *i* truncates everything from *i* on (that's how conflicting suffixes are overwritten). Snapshots are written to a temp file, fsynced, renamed and the directory fsynced; the WAL is then rewritten without the compacted prefix the same way. A torn tail record (crash mid-write) is detected by the CRC and dropped.

### Control plane (`internal/control/store`, `cmd/tollgate-control`)
- The state machine is a versioned KV store: version = Raft index of the last applied change. It keeps a bounded change history for watchers.
- The gRPC `ControlService`: `Put`/`Delete` (leader only; followers answer with the leader's address), `Get` (linearizable via ReadIndex), `Status`, and `Watch(from_version)` (a stream of every change after `from_version`; a full snapshot first if history was compacted past it).
- **Rules:** the leader compiles (type-checks) rule source before proposing and rejects invalid rules. Gateways receive the validated **source** and compile it locally. **Deviation from the spec**, which says compiled rules are shipped: the compiler is deterministic and the gateway already has it, and shipping source avoids a bytecode serialization format and versioning problem. Recorded here; revisit if needed.
- **Keys:** `rules` (rule source), `members` (gateway cluster membership for the hash ring, M4).

### Gateways
`control: {endpoints: [...]}` in the gateway YAML replaces `rules.file`. Each gateway watches from its last version, reconnects (and resumes) on failure, keeps serving its last good config if the control plane is down, and reports `tollgate_config_version` and `tollgate_config_propagation_seconds` (leader commit timestamp in the event → active on the gateway).

### `tollgatectl`
`rules push <file>`, `rules get`, `status`, `members set`.

### Deterministic simulation (`internal/sim`)
3 Raft cores + config stores on a fake network that drops, delays, duplicates and reorders messages, partitions nodes, and crashes and restarts them from their persisted state, all from one seed. Invariants checked continuously:
- (1) at most one leader per term;
- (2) log matching;
- (3) state machine safety: no two nodes apply different entries at the same index;
- (4) every committed proposal is applied on every node after healing;
- (5) liveness: after healing, a leader emerges and pending proposals commit within a bound.

Acceptance: 1,000+ seeds; CI runs a 200-seed smoke.

### Experiments
- **E5:** commit latency (Put through the leader, fsync on), leader failover time under writes (SIGKILL the leader; time until a new leader accepts a write), committed entries lost (must be 0: every acknowledged write must be readable after failover).
- **E6:** commit 100 rule changes and measure commit-to-active time on all 3 gateways (p50/p99).

## M6: decisions log

- **Pure core, two drivers.** All Raft logic lives in a deterministic state machine (`internal/raft`); the server (`internal/control`) and the simulator (`internal/sim`) only move messages, persist and apply. The simulator therefore tests the exact code that runs in production, and every failure replays from its seed (`go test ./internal/sim -run TestTrace` with `TRACE_SEED=N` prints a full event log).
- **The simulator's first bug was its own.** Seed 2 failed immediately: nodes were started while the member list was still being built, so node 1 thought it was a one-node cluster and committed alone. The event-trace replay found it in one run.
- **Mutation check.** `bench/sim/mutants.py` plants five classic Raft bugs (vote without the up-to-date check, in vote and pre-vote; committing old-term entries by counting replicas, Raft's Figure 8; no step-down on a higher term; voting twice in a term) and requires the simulator to catch each.
  - The first harness used fixed fault rates and missed Figure 8 within 300 seeds; fault intensity now varies per seed, and Figure 8 is caught at seed 1061.
  - A first version of the mutation script reported a mutant that failed to compile as "not caught", which briefly made it look like PreVote hid a bug. The script now fails on build errors.
  - Figure 8 needs more than 1000 seeds, so CI runs a 200-seed smoke test and a nightly job runs 5000 seeds per suite plus the mutation check.
- **PreVote (Raft thesis §9.6), found by a live test.** Restarting a killed leader made it raise the term and depose the healthy leader (a write outage on every restart). PreVote asks "would you vote for me?" without changing anyone's term; peers that heard from a leader within an election timeout refuse. Verified live: a restarted node now rejoins as a follower at the same term.
- **PreVote reduces what the simulator explores.** With PreVote on every seed, the simulator stopped catching Figure 8 in 3000 seeds (calmer elections mean fewer stale-log candidacies). The simulator now alternates: even seeds run the production config (PreVote on), odd seeds turn it off.
- **gRPC reconnect backoff** is capped at 1 s for peer links; the default grows to minutes, so a restarted node missed heartbeats and timed out.
- **Proposals resolve on apply, by term.** A client is acknowledged only when its entry is applied with the term it was proposed in; if another leader's entry takes that index, the client gets `ErrDropped` (retry), never a false success. Clients retry through elections and follow `not_leader` redirects. A stopped node answers `Unavailable` so clients move on.
- **Reads** are linearizable through ReadIndex (leader confirms with a heartbeat quorum, then serves once that index is applied); a fresh leader answers `ErrNotReady` until its no-op commits and the server retries.
- **Rules ship as source, compiled on both ends.** The leader compiles before proposing and refuses invalid rules (`InvalidArgument` with the compiler's diagnostics); gateways compile the same source. Deviation from the spec's "compiled rules are distributed", chosen to avoid a bytecode wire format and its versioning.
- **Gateways never block on the control plane once they have rules.** They cache the last accepted rules on disk, keep serving them when every control node is down, and restart from the cache (verified live). With no cache and no control plane a gateway waits rather than serving with no policy.
- **Propagation timing** counts only changes committed after the gateway's watch started; catch-up of older versions at startup would otherwise inflate it by the gateway's start delay.
- **Not done:** gateway membership (`members`) from the control plane. The hash ring still comes from each gateway's static `cluster` block; dynamic membership needs peer dialing on change and is deferred.

## M6: measured results (laptop; full tables in METRICS.md)

- **E5** (3 nodes, 50 ms tick, fsync = F_FULLFSYNC, median of 3 runs): commit latency **p50 13.5 ms / p99 25.2 ms** (leader and follower each fsync before the write is acknowledged); leader failover over 30 SIGKILLs: new leader elected **p50 544 ms / max 947 ms**, first write accepted **p50 643 ms / max 972 ms**; **0 of 17,842 acknowledged writes lost**. Election time is set by the 500–1000 ms randomized timeout; a shorter tick would cut it at the cost of more heartbeats.
- **The first E5 run is superseded** (`e5-20261116T001204Z.json`, kept): its "first write" times were quantized at ~640/1140/1640 ms by the client's 20–500 ms retry backoff and included two 0.1 ms "failovers" (writes the old leader committed just before dying). The rerun retries every 5 ms and counts only writes started after the kill.
- **E6** (3 control nodes → 3 gateways, 100 rule changes, median of 3): commit → active on all gateways **p50 4.8 ms / p99 16.1 ms**; no change timed out. Each gateway compiles the rules itself, which is inside this time.
- **Simulation acceptance:** **5,000 seeds × 2 suites** (KV and the real config store) pass: ~149k elections, ~451k crashes, ~68k partitions, ~3.47M committed entries, ~35k snapshot installs, ~13.5M dropped messages. The mutation check catches all five planted bugs (Figure 8 first at seed 1061).
