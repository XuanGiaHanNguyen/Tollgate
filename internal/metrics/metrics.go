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

	costReserved *prometheus.CounterVec
	costSettled  *prometheus.CounterVec
	costErr      *prometheus.HistogramVec
	resvExpired  prometheus.Counter
	ttft         *prometheus.HistogramVec
	hedges       *prometheus.CounterVec
	filterHits   *prometheus.CounterVec
	holdback     *prometheus.HistogramVec
	propagation  prometheus.Observer
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
	g.costReserved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cost_reserved_usd_total", Help: "Money held before responses finish.",
	}, []string{"model"})
	g.costSettled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cost_settled_usd_total", Help: "Real spend after settlement.",
	}, []string{"model"})
	g.costErr = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_cost_estimate_error_ratio", Help: "(estimate - actual) / actual per request.",
		Buckets: []float64{-1, -0.5, -0.25, -0.1, 0, 0.1, 0.25, 0.5, 1, 2, 4, 8},
	}, []string{"model"})
	expired := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_reservations_expired_total", Help: "Reservations released at their deadline (crashes, cut streams).",
	}, []string{"node"})
	g.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_time_to_first_token_seconds", Help: "Provider time to first token.", Buckets: latencyBuckets,
	}, []string{"provider", "model"})
	g.hedges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_hedges_total", Help: "Hedged requests by provider and outcome (won, lost, cancelled).",
	}, []string{"provider", "outcome"})
	g.filterHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_filter_matches_total", Help: "Safety filter matches.",
	}, []string{"pattern_set", "action"})
	g.holdback = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_filter_holdback_seconds", Help: "Delay added by holding back chunk tails.", Buckets: latencyBuckets,
	}, []string{"pattern_set"})
	reg.MustRegister(g.requests, g.duration, overhead, g.retries, g.breakerState, g.backendHealth, configVersion, ruleEval, policy,
		rl, g.peerRPC, g.batchSize, g.costReserved, g.costSettled, g.costErr, expired, g.ttft, g.hedges, g.filterHits, g.holdback)
	g.resvExpired = expired.WithLabelValues(node)
	prop := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tollgate_config_propagation_seconds", Help: "Commit on the control-plane leader to active on this gateway.",
		Buckets: latencyBuckets,
	}, []string{"node"})
	reg.MustRegister(prop)
	g.propagation = prop.WithLabelValues(node)
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

// AI metrics. Label lookups happen once per model call, not per byte.

// CostReserved records a budget hold (micro-USD).
func (g *Gateway) CostReserved(model string, micro int64) {
	g.costReserved.WithLabelValues(model).Add(float64(micro) / 1e6)
}

// CostSettled records real spend (micro-USD) and the estimate error.
func (g *Gateway) CostSettled(model string, estimate, actual int64) {
	g.costSettled.WithLabelValues(model).Add(float64(actual) / 1e6)
	if actual > 0 {
		g.costErr.WithLabelValues(model).Observe(float64(estimate-actual) / float64(actual))
	}
}

// ConfigPropagation records commit-to-active time for one config version.
func (g *Gateway) ConfigPropagation(seconds float64) { g.propagation.Observe(seconds) }

// ReservationExpired counts a hold released at its deadline.
func (g *Gateway) ReservationExpired() { g.resvExpired.Inc() }

// TTFT records a provider's time to first token.
func (g *Gateway) TTFT(provider, model string, seconds float64) {
	g.ttft.WithLabelValues(provider, model).Observe(seconds)
}

// Hedge records one hedged request's outcome for a provider.
func (g *Gateway) Hedge(provider, outcome string) { g.hedges.WithLabelValues(provider, outcome).Inc() }

// FilterMatches counts safety filter hits.
func (g *Gateway) FilterMatches(set, action string, n int) {
	g.filterHits.WithLabelValues(set, action).Add(float64(n))
}

// FilterHoldback records how long held-back text waited.
func (g *Gateway) FilterHoldback(set string, seconds float64) {
	g.holdback.WithLabelValues(set).Observe(seconds)
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

// Control holds control-plane node metrics.
type Control struct {
	Registry      *prometheus.Registry
	term          prometheus.Gauge
	isLeader      prometheus.Gauge
	commitLatency prometheus.Observer
	rollouts      *prometheus.CounterVec
}

// NewControl registers control-plane metrics for one node.
func NewControl(node string) *Control {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	term := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tollgate_raft_term", Help: "Current Raft term."}, []string{"node"})
	leader := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tollgate_raft_is_leader", Help: "1 if this node leads."}, []string{"node"})
	lat := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "tollgate_raft_commit_latency_seconds",
		Help: "Write latency through consensus (propose to applied on the leader).", Buckets: latencyBuckets}, []string{"node"})
	ro := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tollgate_rollout_decisions_total",
		Help: "Canary rollout decisions made by this node while leader."}, []string{"outcome"})
	reg.MustRegister(term, leader, lat, ro)
	return &Control{Registry: reg, term: term.WithLabelValues(node), isLeader: leader.WithLabelValues(node),
		commitLatency: lat.WithLabelValues(node), rollouts: ro}
}

// Handler serves /metrics.
func (c *Control) Handler() http.Handler {
	return promhttp.HandlerFor(c.Registry, promhttp.HandlerOpts{Registry: c.Registry})
}

// Term records the term and leadership.
func (c *Control) Term(term uint64, leader bool) {
	c.term.Set(float64(term))
	if leader {
		c.isLeader.Set(1)
	} else {
		c.isLeader.Set(0)
	}
}

// RolloutDecision counts one decision (promoted, rolled_back).
func (c *Control) RolloutDecision(outcome string) { c.rollouts.WithLabelValues(outcome).Inc() }

// CommitLatency records one write's latency.
func (c *Control) CommitLatency(seconds float64) { c.commitLatency.Observe(seconds) }
