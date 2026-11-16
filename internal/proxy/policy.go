package proxy

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// policy evaluates compiled rules for each request. The rule set lives in an
// atomic pointer: a reload swaps it without locks, and a request keeps the
// version it loaded even if a swap happens mid-request.
type policy struct {
	rules   atomic.Pointer[rules.RuleSet]
	version atomic.Uint64
	engine  rules.Engine
	limiter *ratelimit.Limiter
	keys    map[string]config.APIKey // read-only after construction
}

func newPolicy(cfg *config.Config, lim *ratelimit.Limiter) *policy {
	p := &policy{limiter: lim, keys: map[string]config.APIKey{}}
	if cfg.Rules.Engine == "interp" {
		p.engine = rules.Interp
	}
	for _, k := range cfg.Auth.APIKeys {
		p.keys[k.Key] = k
	}
	return p
}

// SetRules installs a new rule set and returns its version.
func (g *Gateway) SetRules(rs *rules.RuleSet) uint64 {
	g.policy.rules.Store(rs)
	v := g.policy.version.Add(1)
	if g.metrics != nil {
		g.metrics.SetConfigVersion(v)
	}
	return v
}

// SetRulesAt installs rules at a control-plane version. commitTS (unix
// nanos, 0 if unknown) times propagation from the leader's commit.
func (g *Gateway) SetRulesAt(rs *rules.RuleSet, version uint64, commitTS int64) {
	g.policy.rules.Store(rs)
	g.policy.version.Store(version)
	if g.metrics != nil {
		g.metrics.SetConfigVersion(version)
		if commitTS > 0 {
			g.metrics.ConfigPropagation(time.Since(time.Unix(0, commitTS)).Seconds())
		}
	}
}

// RulesVersion reports the active rule version (0 = none loaded).
func (g *Gateway) RulesVersion() uint64 { return g.policy.version.Load() }

// applyPolicy runs the rules for r. It returns 0 to let the request through,
// or the status to answer with. It may add request headers (set_header) and
// removes the gateway API key so it never reaches backends.
func (g *Gateway) applyPolicy(r *http.Request) int {
	status, _, _ := g.evalPolicy(r, nil)
	return status
}

// policyResult is what an AI route needs beyond the status.
type policyResult struct {
	route *rules.Route // nil when no rule route matched
	d     env.Decision
	user  string
}

// evalPolicy is applyPolicy with optional AI fields (model, max_tokens,
// body) and the matched rule route and decision returned for budgets and
// AI settings.
func (g *Gateway) evalPolicy(r *http.Request, ai *env.Request) (int, *policyResult, error) {
	rs := g.policy.rules.Load()
	if rs == nil {
		return 0, &policyResult{}, nil
	}
	route := rs.Match(r.URL.Path)
	if route == nil {
		return 0, &policyResult{}, nil
	}

	req := env.Request{
		Method:   r.Method,
		Path:     r.URL.Path,
		Host:     hostOnly(r.Host),
		Header:   r.Header,
		RawQuery: r.URL.RawQuery,
		ClientIP: clientIP(r.RemoteAddr),
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if k, ok := g.policy.keys[h[len("Bearer "):]]; ok {
			req.Authenticated, req.UserID, req.UserTier = true, k.UserID, k.Tier
			r.Header.Del("Authorization")
		}
	}
	if ai != nil {
		req.Model, req.MaxTokens, req.Body = ai.Model, ai.MaxTokens, ai.Body
	}
	res := &policyResult{route: route, user: req.UserID}
	d := &res.d
	var start time.Time
	if g.metrics != nil {
		start = time.Now()
	}
	rs.Eval(route, g.policy.engine, &req, d)
	if g.metrics != nil {
		g.metrics.ObserveRuleEval(g.policy.engine.String(), time.Since(start).Seconds())
	}

	switch {
	case d.Err != nil:
		g.log.Error("rule evaluation failed; failing closed", "route", route.Pattern, "err", d.Err)
		g.countPolicy(metrics.PolicyError)
		return http.StatusInternalServerError, res, d.Err
	case d.Deny != 0:
		g.countPolicy(metrics.PolicyDeny)
		return d.Deny, res, nil
	}
	// Every applicable limit must admit the request. Earlier buckets keep
	// the token they gave even if a later one refuses. The limiter checks
	// locally or on the key's owner node.
	for i := 0; i < d.NLimits; i++ {
		if ok, _ := g.policy.limiter.Allow(r.Context(), d.Limits[i]); !ok {
			g.countPolicy(metrics.PolicyLimited)
			return http.StatusTooManyRequests, res, nil
		}
	}
	for i := 0; i < d.NHeaders; i++ {
		r.Header.Set(d.Headers[i].Name, d.Headers[i].Value)
	}
	g.countPolicy(metrics.PolicyAllow)
	return 0, res, nil
}

func (g *Gateway) countPolicy(result string) {
	if g.metrics != nil {
		g.metrics.Policy(result)
	}
}

// clientIP uses the socket peer address. X-Forwarded-For is not trusted
// until a trusted-proxy list exists (M8, behind the ALB).
func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

func writePolicyStatus(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(strconv.Itoa(status) + " " + http.StatusText(status) + "\n"))
}
