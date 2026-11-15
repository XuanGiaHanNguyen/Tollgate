// Package metrics owns the Prometheus registry and every Tollgate metric
// name. Metrics are added by the milestone that produces them.
package metrics

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Latency buckets from 50µs to ~13s; dense in the sub-millisecond range
// where gateway overhead lives.
var latencyBuckets = prometheus.ExponentialBuckets(50e-6, 2, 19)

// Gateway holds the data-plane metrics for one node.
type Gateway struct {
	Registry *prometheus.Registry
	node     string

	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	overhead      prometheus.Observer
	retries       *prometheus.CounterVec
	breakerState  *prometheus.GaugeVec
	backendHealth *prometheus.GaugeVec
	configVersion prometheus.Gauge
	ruleEval      map[string]prometheus.Observer // by engine
	policy        map[string]prometheus.Counter  // by result
	rlDecisions   map[string]prometheus.Counter  // by result
	peerRPC       *prometheus.HistogramVec
	batchSize     *prometheus.HistogramVec
}

// Policy outcomes for tollgate_policy_decisions_total.
const (
	PolicyAllow   = "allow"
	PolicyDeny    = "deny"
	PolicyLimited = "limited"
	PolicyError   = "error"
)

// NewGateway registers gateway metrics plus Go runtime and process
// collectors on a fresh registry.
func NewGateway(node string) *Gateway {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	g := &Gateway{
		Registry: reg,
		node:     node,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollgate_requests_total",
			Help: "Requests handled by the gateway.",
		}, []string{"route", "status", "node"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tollgate_request_duration_seconds",
			Help:    "End-to-end request latency inside the gateway.",
			Buckets: latencyBuckets,
		}, []string{"route", "node"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollgate_retries_total",
			Help: "Upstream retries issued.",
		}, []string{"route"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tollgate_breaker_state",
			Help: "Circuit breaker state: 0 closed, 1 half-open, 2 open.",
		}, []string{"backend"}),
		backendHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tollgate_backend_healthy",
			Help: "Active health-check result: 1 healthy, 0 unhealthy.",
		}, []string{"backend"}),
	}
	overhead := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tollgate_gateway_overhead_seconds",
		Help:    "Time spent inside the gateway, excluding upstream round trips.",
		Buckets: latencyBuckets,
	}, []string{"node"})
	configVersion := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tollgate_config_version",
		Help: "Config version this gateway is running.",
	}, []string{"node"})
	ruleEval := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tollgate_rule_eval_seconds",
		Help:    "Rule evaluation time per request.",
		Buckets: prometheus.ExponentialBuckets(25e-9, 2, 18), // 25ns .. ~3ms
	}, []string{"engine"})
	policy := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_policy_decisions_total",
		Help: "Rule policy outcomes: allow, deny (deny/require auth), limited (429), error (fail closed).",
	}, []string{"result"})
	rl := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_ratelimit_decisions_total",
		Help: "Rate-limit decisions: allow, deny, or degraded (owner unreachable, local fallback).",
	}, []string{"result", "node"})
	g.peerRPC = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tollgate_ratelimit_peer_rpc_seconds",
		Help:    "CheckLimits RPC latency to the key owner.",
		Buckets: latencyBuckets,
	}, []string{"peer"})
	g.batchSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tollgate_ratelimit_batch_size",
		Help:    "Checks per CheckLimits RPC.",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 100},
	}, []string{"peer"})
	reg.MustRegister(g.requests, g.duration, overhead, g.retries, g.breakerState, g.backendHealth, configVersion, ruleEval, policy,
		rl, g.peerRPC, g.batchSize)
	g.rlDecisions = map[string]prometheus.Counter{}
	for _, r := range []string{"allow", "deny", "degraded"} {
		g.rlDecisions[r] = rl.WithLabelValues(r, node)
	}
	g.ruleEval = map[string]prometheus.Observer{"vm": ruleEval.WithLabelValues("vm"), "interp": ruleEval.WithLabelValues("interp")}
	g.policy = map[string]prometheus.Counter{}
	for _, r := range []string{PolicyAllow, PolicyDeny, PolicyLimited, PolicyError} {
		g.policy[r] = policy.WithLabelValues(r)
	}
	g.overhead = overhead.WithLabelValues(node)
	g.configVersion = configVersion.WithLabelValues(node)
	return g
}

// Handler serves /metrics for this registry.
func (g *Gateway) Handler() http.Handler {
	return promhttp.HandlerFor(g.Registry, promhttp.HandlerOpts{Registry: g.Registry})
}

// Route returns pre-resolved metric handles for one route, so the request
// path does no label hashing or vec locking after warm-up.
func (g *Gateway) Route(name string) *RouteMetrics {
	return &RouteMetrics{
		g:        g,
		name:     name,
		duration: g.duration.WithLabelValues(name, g.node),
		retries:  g.retries.WithLabelValues(name),
	}
}

// ObserveOverhead records time spent in the gateway itself.
func (g *Gateway) ObserveOverhead(seconds float64) { g.overhead.Observe(seconds) }

// SetBreakerState records a breaker transition (0 closed, 1 half-open, 2 open).
func (g *Gateway) SetBreakerState(backend string, state int) {
	g.breakerState.WithLabelValues(backend).Set(float64(state))
}

// SetBackendHealthy records an active health-check result.
func (g *Gateway) SetBackendHealthy(backend string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1
	}
	g.backendHealth.WithLabelValues(backend).Set(v)
}

// ObserveRuleEval records one rule evaluation. The maps are read-only after
// construction, so concurrent lookups are safe.
func (g *Gateway) ObserveRuleEval(engine string, seconds float64) {
	g.ruleEval[engine].Observe(seconds)
}

// Policy counts one policy outcome.
func (g *Gateway) Policy(result string) { g.policy[result].Inc() }

// RateLimitDecision counts one limiter decision (allow, deny, degraded).
func (g *Gateway) RateLimitDecision(result string) { g.rlDecisions[result].Inc() }

// PeerRPC records one CheckLimits RPC (per batch, not per request).
func (g *Gateway) PeerRPC(peer string, seconds float64, batch int, err error) {
	if err != nil {
		return
	}
	g.peerRPC.WithLabelValues(peer).Observe(seconds)
	g.batchSize.WithLabelValues(peer).Observe(float64(batch))
}

// SetConfigVersion records the active config version.
func (g *Gateway) SetConfigVersion(v uint64) { g.configVersion.Set(float64(v)) }

// RouteMetrics are the per-route handles.
type RouteMetrics struct {
	g        *Gateway
	name     string
	duration prometheus.Observer
	retries  prometheus.Counter
	byStatus sync.Map // int -> prometheus.Counter
}

// ObserveRequest records one finished request.
func (r *RouteMetrics) ObserveRequest(status int, seconds float64) {
	r.duration.Observe(seconds)
	c, ok := r.byStatus.Load(status)
	if !ok {
		c, _ = r.byStatus.LoadOrStore(status, r.g.requests.WithLabelValues(r.name, strconv.Itoa(status), r.g.node))
	}
	c.(prometheus.Counter).Inc()
}

// Retry records one retry.
func (r *RouteMetrics) Retry() { r.retries.Inc() }
