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
