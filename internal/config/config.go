// Package config loads the gateway's static YAML config (M1). In later
// milestones the same shape arrives from the control plane.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole gateway config.
type Config struct {
	Listen          string        `yaml:"listen"`
	AdminListen     string        `yaml:"admin_listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	RetryBudget     RetryBudget   `yaml:"retry_budget"`
	Transport       Transport     `yaml:"transport"`
	Upstreams       []Upstream    `yaml:"upstreams"`
	Routes          []Route       `yaml:"routes"`
	Rules           Rules         `yaml:"rules"`
	Auth            Auth          `yaml:"auth"`
	Cluster         *Cluster      `yaml:"cluster"` // nil: single-node limiting
	AI              AI            `yaml:"ai"`
}

// AI configures model routes (M5).
type AI struct {
	ReservationTTL time.Duration       `yaml:"reservation_ttl"` // hold deadline for budget reservations
	Pricing        map[string]Price    `yaml:"pricing"`         // per model; "*" is the default
	PatternSets    map[string][]string `yaml:"pattern_sets"`    // named safety-filter phrase lists
	HedgeMinSample int                 `yaml:"hedge_min_samples"`
}

// Price per 1,000 tokens in USD.
type Price struct {
	InputPer1K  float64 `yaml:"input_per_1k_usd"`
	OutputPer1K float64 `yaml:"output_per_1k_usd"`
}

// Cluster configures distributed rate limiting (M4). Membership is static
// until the control plane provides it (M6); every gateway lists all members.
type Cluster struct {
	Self        string          `yaml:"self"`   // this gateway's member name
	Listen      string          `yaml:"listen"` // gRPC address for peer checks
	VNodes      int             `yaml:"vnodes"`
	PeerTimeout time.Duration   `yaml:"peer_timeout"`
	Batch       Batch           `yaml:"batch"`
	Members     []ClusterMember `yaml:"members"`
}

// Batch tunes peer-check batching. max: 1 disables it.
type Batch struct {
	Max   int           `yaml:"max"`
	Flush time.Duration `yaml:"flush"`
}

// ClusterMember is one gateway.
type ClusterMember struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
}

// Rules points at the policy file written in the rule language (M3).
type Rules struct {
	File           string        `yaml:"file"`            // "" = no policy, every routed request is allowed
	ReloadInterval time.Duration `yaml:"reload_interval"` // how often the file is checked for changes
	Budget         int           `yaml:"budget"`          // VM instructions per evaluation
	Engine         string        `yaml:"engine"`          // "vm" (default) or "interp"
}

// Auth holds static API keys (M3). A request is authenticated when it sends
// "Authorization: Bearer <key>" with a listed key.
type Auth struct {
	APIKeys []APIKey `yaml:"api_keys"`
}

// APIKey maps a key to a user identity visible to rules.
type APIKey struct {
	Key    string `yaml:"key"`
	UserID string `yaml:"user_id"`
	Tier   string `yaml:"tier"`
}

// RetryBudget caps retries as a share of traffic.
type RetryBudget struct {
	Ratio   float64 `yaml:"ratio"`   // retries allowed per request (0.2 = 20%)
	Initial int     `yaml:"initial"` // retries available before any traffic
}

// Transport tunes the upstream connection pool.
type Transport struct {
	MaxIdleConnsPerHost int           `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout     time.Duration `yaml:"idle_conn_timeout"`
	ForceHTTP2          *bool         `yaml:"force_http2"`
}

// Upstream is a named pool of backend targets.
type Upstream struct {
	Name        string      `yaml:"name"`
	Balancer    string      `yaml:"balancer"`
	Targets     []Target    `yaml:"targets"`
	HealthCheck HealthCheck `yaml:"health_check"`
	Passive     Passive     `yaml:"passive"`
	Breaker     Breaker     `yaml:"breaker"`
}

// Target is one backend URL with an optional weight.
type Target struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// HealthCheck configures active probing.
type HealthCheck struct {
	Path               string        `yaml:"path"`
	Interval           time.Duration `yaml:"interval"`
	Timeout            time.Duration `yaml:"timeout"`
	UnhealthyThreshold int           `yaml:"unhealthy_threshold"`
	HealthyThreshold   int           `yaml:"healthy_threshold"`
}

// Passive configures ejection based on live traffic.
type Passive struct {
	ConsecutiveFailures int64         `yaml:"consecutive_failures"`
	EjectDuration       time.Duration `yaml:"eject_duration"`
}

// Breaker configures the per-target circuit breaker.
type Breaker struct {
	Disabled       bool          `yaml:"disabled"`
	Window         time.Duration `yaml:"window"`
	MinRequests    int64         `yaml:"min_requests"`
	ErrorRate      float64       `yaml:"error_rate"`
	Cooldown       time.Duration `yaml:"cooldown"`
	HalfOpenProbes int64         `yaml:"half_open_probes"`
}

// Route maps matching requests to an upstream.
type Route struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"` // "" (HTTP proxy) or "ai" (model API: budgets, hedging, safety filter)
	Prefix   string   `yaml:"prefix"`
	Host     string   `yaml:"host"`
	Methods  []string `yaml:"methods"`
	Upstream string   `yaml:"upstream"`
	Timeouts Timeouts `yaml:"timeouts"`
	Retry    Retry    `yaml:"retry"`
}

// Timeouts per route.
type Timeouts struct {
	Connect time.Duration `yaml:"connect"`
	Header  time.Duration `yaml:"header"`
	Total   time.Duration `yaml:"total"`
}

// Retry policy per route.
type Retry struct {
	Attempts    int           `yaml:"attempts"` // total tries including the first; 1 = no retry
	BaseBackoff time.Duration `yaml:"base_backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes YAML, fills defaults and validates.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AdminListen == "" {
		c.AdminListen = ":9090"
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 15 * time.Second
	}
	if c.RetryBudget.Ratio == 0 {
		c.RetryBudget.Ratio = 0.2
	}
	if c.RetryBudget.Initial == 0 {
		c.RetryBudget.Initial = 10
	}
	if c.Transport.MaxIdleConnsPerHost == 0 {
		c.Transport.MaxIdleConnsPerHost = 256
	}
	if c.Transport.IdleConnTimeout == 0 {
		c.Transport.IdleConnTimeout = 90 * time.Second
	}
	if c.Transport.ForceHTTP2 == nil {
		t := true
		c.Transport.ForceHTTP2 = &t
	}
	if c.AI.ReservationTTL == 0 {
		c.AI.ReservationTTL = 2 * time.Minute
	}
	if c.AI.HedgeMinSample == 0 {
		c.AI.HedgeMinSample = 20
	}
	if c.Rules.ReloadInterval == 0 {
		c.Rules.ReloadInterval = time.Second
	}
	if c.Rules.Budget == 0 {
		c.Rules.Budget = 10_000
	}
	if c.Rules.Engine == "" {
		c.Rules.Engine = "vm"
	}
	if cl := c.Cluster; cl != nil {
		if cl.VNodes == 0 {
			cl.VNodes = 128
		}
		if cl.PeerTimeout == 0 {
			cl.PeerTimeout = 20 * time.Millisecond
		}
		if cl.Batch.Max == 0 {
			cl.Batch.Max = 100
		}
		if cl.Batch.Flush == 0 {
			cl.Batch.Flush = 500 * time.Microsecond
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Name == "" {
			r.Name = r.Prefix
		}
		if r.Timeouts.Connect == 0 {
			r.Timeouts.Connect = 2 * time.Second
		}
		if r.Timeouts.Header == 0 {
			r.Timeouts.Header = 10 * time.Second
		}
		if r.Timeouts.Total == 0 {
			r.Timeouts.Total = 30 * time.Second
		}
		if r.Retry.Attempts == 0 {
			r.Retry.Attempts = 1
		}
		if r.Retry.BaseBackoff == 0 {
			r.Retry.BaseBackoff = 10 * time.Millisecond
		}
		if r.Retry.MaxBackoff == 0 {
			r.Retry.MaxBackoff = 200 * time.Millisecond
		}
		for j, m := range r.Methods {
			r.Methods[j] = strings.ToUpper(m)
		}
	}
}

// Validate reports every problem found, not just the first.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.RetryBudget.Ratio < 0 || c.RetryBudget.Ratio > 1 {
		add("retry_budget.ratio must be in [0,1], got %v", c.RetryBudget.Ratio)
	}
	upstreams := map[string]bool{}
	for i, u := range c.Upstreams {
		where := fmt.Sprintf("upstreams[%d]", i)
		if u.Name == "" {
			add("%s: name is required", where)
		} else if upstreams[u.Name] {
			add("%s: duplicate upstream %q", where, u.Name)
		}
		upstreams[u.Name] = true
		switch u.Balancer {
		case "", "round_robin", "least_conn", "weighted_round_robin":
		default:
			add("%s: unknown balancer %q", where, u.Balancer)
		}
		if len(u.Targets) == 0 {
			add("%s: at least one target is required", where)
		}
		for j, t := range u.Targets {
			pu, err := url.Parse(t.URL)
			if err != nil || pu.Scheme == "" || pu.Host == "" {
				add("%s.targets[%d]: invalid url %q", where, j, t.URL)
			}
			if t.Weight < 0 {
				add("%s.targets[%d]: weight must be >= 0", where, j)
			}
		}
		if u.Breaker.ErrorRate < 0 || u.Breaker.ErrorRate > 1 {
			add("%s.breaker.error_rate must be in [0,1]", where)
		}
	}
	if len(c.Routes) == 0 {
		add("at least one route is required")
	}
	if c.Rules.Engine != "vm" && c.Rules.Engine != "interp" {
		add("rules.engine must be vm or interp, got %q", c.Rules.Engine)
	}
	if c.Rules.Budget < 0 {
		add("rules.budget must be positive")
	}
	if cl := c.Cluster; cl != nil {
		if cl.Listen == "" {
			add("cluster.listen is required")
		}
		names := map[string]bool{}
		for i, m := range cl.Members {
			if m.Name == "" || m.Addr == "" {
				add("cluster.members[%d]: name and addr are required", i)
			}
			if names[m.Name] {
				add("cluster.members[%d]: duplicate member %q", i, m.Name)
			}
			names[m.Name] = true
		}
		if !names[cl.Self] {
			add("cluster.self %q is not in cluster.members", cl.Self)
		}
		if cl.Batch.Max < 1 || cl.Batch.Max > 1000 {
			add("cluster.batch.max must be in [1,1000]")
		}
	}
	for model, p := range c.AI.Pricing {
		if p.InputPer1K < 0 || p.OutputPer1K < 0 {
			add("ai.pricing[%s]: prices must be >= 0", model)
		}
	}
	for name, set := range c.AI.PatternSets {
		if len(set) == 0 {
			add("ai.pattern_sets[%s]: empty set", name)
		}
	}
	keys := map[string]bool{}
	for i, k := range c.Auth.APIKeys {
		if k.Key == "" || k.UserID == "" {
			add("auth.api_keys[%d]: key and user_id are required", i)
		}
		if keys[k.Key] {
			add("auth.api_keys[%d]: duplicate key", i)
		}
		keys[k.Key] = true
	}
	type key struct{ prefix, host, methods string }
	seen := map[key]bool{}
	for i, r := range c.Routes {
		where := fmt.Sprintf("routes[%d] (%s)", i, r.Name)
		if !strings.HasPrefix(r.Prefix, "/") {
			add("%s: prefix must start with /", where)
		}
		if !upstreams[r.Upstream] {
			add("%s: unknown upstream %q", where, r.Upstream)
		}
		if r.Retry.Attempts < 1 || r.Retry.Attempts > 10 {
			add("%s: retry.attempts must be in [1,10]", where)
		}
		if r.Type != "" && r.Type != "ai" {
			add("%s: type must be empty or \"ai\", got %q", where, r.Type)
		}
		k := key{r.Prefix, r.Host, strings.Join(r.Methods, ",")}
		if seen[k] {
			add("%s: duplicate route for prefix %q host %q methods %v", where, r.Prefix, r.Host, r.Methods)
		}
		seen[k] = true
	}
	return errors.Join(errs...)
}
