// Package proxy is the gateway data plane: routing, reverse proxying,
// retries, timeouts and graceful shutdown.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/breaker"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/lb"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit"
)

// Upstream is a named pool of targets with its balancer and health checker.
type Upstream struct {
	Name     string
	Targets  []*lb.Target
	Balancer lb.Balancer
	health   *lb.HealthChecker
}

// Route is a compiled route: matcher fields plus its proxy.
type Route struct {
	Name     string
	prefix   string
	host     string
	methods  []string
	cfg      config.Route
	upstream *Upstream
	proxy    *httputil.ReverseProxy
	metrics  *metrics.RouteMetrics // nil when metrics are off
	ai       bool                  // type: ai — model API handling
}

// Gateway serves proxied traffic. The router is held in an atomic pointer so
// later milestones can hot-swap config without locks.
type Gateway struct {
	router    atomic.Pointer[Router]
	upstreams map[string]*Upstream
	log       *slog.Logger
	metrics   *metrics.Gateway
	policy    *policy
	ai        *aiState
	stop      chan struct{}
}

// Options are optional hooks for New.
type Options struct {
	Logger  *slog.Logger
	Metrics *metrics.Gateway // optional
	// Limiter enforces rule limits; nil means a single-node local limiter.
	Limiter *ratelimit.Limiter
	// OnBreakerChange and OnHealthChange are metrics/log hooks.
	OnBreakerChange func(target string, from, to breaker.State)
	OnHealthChange  func(target string, healthy bool)
}

// New builds a gateway from config. Call Start to run health checks.
func New(cfg *config.Config, opts Options) (*Gateway, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	g := &Gateway{upstreams: map[string]*Upstream{}, log: log, metrics: opts.Metrics,
		stop: make(chan struct{})}
	lim := opts.Limiter
	if lim == nil {
		var onDecision func(string)
		var hooks ratelimit.BudgetHooks
		if opts.Metrics != nil {
			onDecision = opts.Metrics.RateLimitDecision
			hooks.Expired = func(int64) { opts.Metrics.ReservationExpired() }
		}
		lim = ratelimit.NewLocal(onDecision, hooks)
	}
	g.policy = newPolicy(cfg, lim)
	ai, err := newAIState(cfg)
	if err != nil {
		return nil, err
	}
	g.ai = ai
	if m := opts.Metrics; m != nil {
		// Metrics hooks run alongside any caller-supplied hooks.
		userBreaker, userHealth := opts.OnBreakerChange, opts.OnHealthChange
		opts.OnBreakerChange = func(target string, from, to breaker.State) {
			m.SetBreakerState(target, int(to))
			if userBreaker != nil {
				userBreaker(target, from, to)
			}
		}
		opts.OnHealthChange = func(target string, healthy bool) {
			m.SetBackendHealthy(target, healthy)
			if userHealth != nil {
				userHealth(target, healthy)
			}
		}
	}

	for _, uc := range cfg.Upstreams {
		up, err := buildUpstream(uc, opts, log)
		if err != nil {
			return nil, err
		}
		g.upstreams[uc.Name] = up
		if opts.Metrics != nil {
			for _, t := range up.Targets {
				opts.Metrics.SetBackendHealthy(t.String(), true)
				if t.Breaker != nil {
					opts.Metrics.SetBreakerState(t.String(), int(breaker.Closed))
				}
			}
		}
	}

	budget := NewRetryBudget(cfg.RetryBudget.Ratio, cfg.RetryBudget.Initial)
	bufPool := &bufferPool{}
	routes := make([]*Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		up := g.upstreams[rc.Upstream]
		rt := &Route{
			Name: rc.Name, prefix: rc.Prefix, host: rc.Host, methods: rc.Methods,
			cfg: rc, upstream: up, ai: rc.Type == "ai",
		}
		if opts.Metrics != nil {
			rt.metrics = opts.Metrics.Route(rc.Name)
		}
		transport := &retryTransport{
			route:    rc.Name,
			base:     newHTTPTransport(cfg.Transport, rc.Timeouts),
			upstream: up,
			retry:    rc.Retry,
			budget:   budget,
			metrics:  rt.metrics,
		}
		rt.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetXForwarded()
				pr.Out.Host = pr.In.Host
				// Scheme and host are filled per attempt by retryTransport.
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = "upstream"
			},
			Transport:    transport,
			BufferPool:   bufPool,
			ErrorHandler: g.errorHandler,
		}
		routes = append(routes, rt)
	}
	g.router.Store(NewRouter(routes))
	return g, nil
}

func buildUpstream(uc config.Upstream, opts Options, log *slog.Logger) (*Upstream, error) {
	up := &Upstream{Name: uc.Name}
	for _, tc := range uc.Targets {
		u, err := url.Parse(tc.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", uc.Name, err)
		}
		var br *breaker.Breaker
		if !uc.Breaker.Disabled {
			name := u.String()
			br = breaker.New(breaker.Config{
				Window:         uc.Breaker.Window,
				MinRequests:    uc.Breaker.MinRequests,
				ErrorRate:      uc.Breaker.ErrorRate,
				Cooldown:       uc.Breaker.Cooldown,
				HalfOpenProbes: uc.Breaker.HalfOpenProbes,
			}, breaker.WithOnChange(func(from, to breaker.State) {
				log.Info("breaker state change", "target", name, "from", from, "to", to)
				if opts.OnBreakerChange != nil {
					opts.OnBreakerChange(name, from, to)
				}
			}))
		}
		up.Targets = append(up.Targets, lb.NewTarget(u, tc.Weight, br, lb.PassiveConfig{
			ConsecutiveFailures: uc.Passive.ConsecutiveFailures,
			EjectDuration:       uc.Passive.EjectDuration,
		}))
	}
	bal, err := lb.NewBalancer(uc.Balancer, up.Targets)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", uc.Name, err)
	}
	up.Balancer = bal
	hc := uc.HealthCheck
	up.health = lb.NewHealthChecker(lb.HealthConfig{
		Path: hc.Path, Interval: hc.Interval, Timeout: hc.Timeout,
		UnhealthyThreshold: hc.UnhealthyThreshold, HealthyThreshold: hc.HealthyThreshold,
	}, up.Targets, func(t *lb.Target, healthy bool) {
		log.Info("health change", "upstream", uc.Name, "target", t.String(), "healthy", healthy)
		if opts.OnHealthChange != nil {
			opts.OnHealthChange(t.String(), healthy)
		}
	})
	return up, nil
}

func newHTTPTransport(tc config.Transport, to config.Timeouts) *http.Transport {
	dialer := &net.Dialer{Timeout: to.Connect, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     *tc.ForceHTTP2,
		MaxIdleConns:          0, // no global cap; per-host cap below
		MaxIdleConnsPerHost:   tc.MaxIdleConnsPerHost,
		IdleConnTimeout:       tc.IdleConnTimeout,
		ResponseHeaderTimeout: to.Header,
		TLSHandshakeTimeout:   to.Connect,
		ExpectContinueTimeout: time.Second,
	}
}

// Start launches health checkers.
func (g *Gateway) Start() {
	for _, up := range g.upstreams {
		up.health.Start()
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
				g.policy.limiter.Sweep()
			}
		}
	}()
}

// Stop halts health checkers.
func (g *Gateway) Stop() {
	close(g.stop)
	for _, up := range g.upstreams {
		up.health.Stop()
	}
}

// Upstream returns an upstream by name (for admin/metrics).
func (g *Gateway) Upstream(name string) *Upstream { return g.upstreams[name] }

// ServeHTTP routes, applies rule policy, and proxies one request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := g.router.Load().Match(r)
	if route == nil {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	if route.ai {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		g.serveAI(sw, r, route)
		if route.metrics != nil {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			route.metrics.ObserveRequest(status, time.Since(start).Seconds())
		}
		return
	}
	if route.metrics == nil {
		if status := g.applyPolicy(r); status != 0 {
			writePolicyStatus(w, status)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), route.cfg.Timeouts.Total)
		defer cancel()
		route.proxy.ServeHTTP(w, r.WithContext(ctx))
		return
	}

	start := time.Now()
	if status := g.applyPolicy(r); status != 0 {
		writePolicyStatus(w, status)
		total := time.Since(start)
		route.metrics.ObserveRequest(status, total.Seconds())
		g.metrics.ObserveOverhead(total.Seconds())
		return
	}
	st := statsPool.Get().(*reqStats)
	*st = reqStats{}
	sw := &statusWriter{ResponseWriter: w}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), statsKey{}, st), route.cfg.Timeouts.Total)
	route.proxy.ServeHTTP(sw, r.WithContext(ctx))
	cancel()

	total := time.Since(start)
	status := sw.status
	if status == 0 {
		status = http.StatusOK
	}
	route.metrics.ObserveRequest(status, total.Seconds())
	if overhead := total - st.upstream - st.streaming; overhead >= 0 {
		g.metrics.ObserveOverhead(overhead.Seconds())
	}
	statsPool.Put(st)
}

func (g *Gateway) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	var ne net.Error
	switch {
	case errors.Is(err, ErrNoHealthyBackend), errors.Is(err, ErrCircuitOpen):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		status = 499 // client closed request (nginx convention); client won't see it
	}
	if status != 499 {
		g.log.Debug("proxy error", "path", r.URL.Path, "err", err, "status", status)
	}
	w.WriteHeader(status)
}

// bufferPool implements httputil.BufferPool to avoid a 32 KiB allocation per
// proxied response.
type bufferPool struct{ p sync.Pool }

func (b *bufferPool) Get() []byte {
	if v := b.p.Get(); v != nil {
		return *(v.(*[]byte))
	}
	return make([]byte, 32<<10)
}

func (b *bufferPool) Put(buf []byte) { b.p.Put(&buf) }

// TargetStatus is the admin view of one target.
type TargetStatus struct {
	URL      string `json:"url"`
	Healthy  bool   `json:"healthy"`
	Ejected  bool   `json:"ejected"`
	Breaker  string `json:"breaker"`
	Inflight int64  `json:"inflight"`
}

// UpstreamStatus is the admin view of one upstream.
type UpstreamStatus struct {
	Name    string         `json:"name"`
	Targets []TargetStatus `json:"targets"`
}

// Status reports health, ejection and breaker state for every target.
func (g *Gateway) Status() []UpstreamStatus {
	out := make([]UpstreamStatus, 0, len(g.upstreams))
	for _, up := range g.upstreams {
		us := UpstreamStatus{Name: up.Name}
		for _, t := range up.Targets {
			br := "disabled"
			if t.Breaker != nil {
				br = t.Breaker.State().String()
			}
			us.Targets = append(us.Targets, TargetStatus{
				URL: t.String(), Healthy: t.Healthy(), Ejected: t.Ejected(), Breaker: br, Inflight: t.Inflight(),
			})
		}
		out = append(out, us)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
