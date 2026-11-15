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
