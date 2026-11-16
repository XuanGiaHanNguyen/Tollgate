package rollout

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/store"
)

// Store is what the controller needs from the control-plane node.
type Store interface {
	IsLeader() bool
	LocalGet(key string) (store.Item, bool)
	Propose(ctx context.Context, c store.Command) (uint64, error)
}

// Controller decides canary rollouts. Run it on every control node; it
// acts only while its node leads.
type Controller struct {
	Store    Store
	Gateways map[string]string // gateway name -> admin base URL (serving /metrics)
	Policy   Policy
	Interval time.Duration
	Log      *slog.Logger
	// OnDecision is called after a decision commits (outcome: promoted, rolled_back).
	OnDecision func(outcome string)
	Client     *http.Client

	curID  string
	since  time.Time
	before map[string]sample
}

type sample map[string]float64 // series -> value

// NewID returns a random rollout ID.
func NewID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Run loops until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	if c.Interval <= 0 {
		c.Interval = 500 * time.Millisecond
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.Step(ctx)
	}
}

func (c *Controller) reset() { c.curID, c.before = "", nil }

// Step runs one control iteration (exported for tests).
func (c *Controller) Step(ctx context.Context) {
	if !c.Store.IsLeader() {
		c.reset() // a new leader starts its own comparison window
		return
	}
	item, ok := c.Store.LocalGet(KeyRollout)
	if !ok {
		c.reset()
		return
	}
	rec, err := Decode(item.Value)
	if err != nil || rec.State != StateCanary {
		c.reset()
		return
	}
	if rec.ID != c.curID {
		c.curID, c.since, c.before = rec.ID, time.Now(), c.scrapeAll(ctx)
		c.Log.Info("rollout: comparing canary with the other gateways", "id", rec.ID, "canary", rec.Canary)
		return
	}
	after := c.scrapeAll(ctx)
	canary, base := c.compare(rec.Canary, c.before, after)
	v, reason := Decide(c.Policy, canary, base, time.Since(c.since), time.Since(time.Unix(0, rec.StartedTS)))
	c.decide(ctx, rec, item.Version, canary, base, v, reason)
}

func (c *Controller) decide(ctx context.Context, rec Record, version uint64, canary, base Stats, v Verdict, reason string) {
	if v == Wait {
		return
	}
	final := rec
	final.DecidedTS = time.Now().UnixNano()
	final.Reason = reason
	final.Report = &Report{Canary: canary, Baseline: base, Verdict: v.String()}
	final.State = StateRolledBack
	ops := []store.Command{}
	if v == Promote {
		final.State = StatePromoted
		ops = append(ops, store.Command{Op: "put", Key: KeyRules, Value: rec.Candidate})
	}
	enc := final.Encode()
	ops = append(ops, store.Command{Op: "put", Key: KeyRollout, Value: enc},
		store.Command{Op: "put", Key: KeyHistory + rec.ID, Value: enc})
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Store.Propose(pctx, store.Command{Op: "txn", If: &store.Cond{Key: KeyRollout, Version: version}, Ops: ops})
	if errors.Is(err, store.ErrCondFailed) {
		c.reset() // decided elsewhere
		return
	}
	if err != nil {
		c.Log.Warn("rollout: decision not committed; will retry", "id", rec.ID, "err", err.Error())
		return
	}
	c.Log.Info("rollout decided", "id", rec.ID, "state", final.State, "reason", reason)
	if c.OnDecision != nil {
		c.OnDecision(final.State)
	}
	c.reset()
}

func (c *Controller) scrapeAll(ctx context.Context) map[string]sample {
	out := map[string]sample{}
	for name, url := range c.Gateways {
		if s, err := c.scrape(ctx, url); err == nil {
			out[name] = s
		}
	}
	return out
}

func (c *Controller) scrape(ctx context.Context, base string) (sample, error) {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/metrics", nil)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseMetrics(bufio.NewScanner(resp.Body))
}

// parseMetrics keeps the series the comparison needs.
func parseMetrics(sc *bufio.Scanner) (sample, error) {
	s := sample{}
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "tollgate_requests_total{") && !strings.HasPrefix(line, "tollgate_request_duration_seconds_bucket{") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			s[line[:i]] = v
		}
	}
	return s, sc.Err()
}

func label(series, name string) string {
	i := strings.Index(series, name+`="`)
	if i < 0 {
		return ""
	}
	rest := series[i+len(name)+2:]
	return rest[:strings.IndexByte(rest, '"')]
}

// compare builds canary and baseline stats from scrape deltas. Gateways
// missing from either scrape (down) are left out.
func (c *Controller) compare(canaryName string, before, after map[string]sample) (Stats, Stats) {
	var canary, base Stats
	cb, bb := map[float64]float64{}, map[float64]float64{}
	for name, a := range after {
		b, ok := before[name]
		if !ok {
			continue
		}
		st, buckets := &base, bb
		if name == canaryName {
			st, buckets = &canary, cb
		}
		for k, v := range a {
			d := v - b[k]
			switch {
			case strings.HasPrefix(k, "tollgate_requests_total{"):
				st.Requests += d
				if code, _ := strconv.Atoi(label(k, "status")); code >= 400 {
					st.Errors += d
				}
			case strings.HasPrefix(k, "tollgate_request_duration_seconds_bucket{"):
				le := label(k, "le")
				f, err := strconv.ParseFloat(le, 64)
				if le == "+Inf" {
					f, err = math.Inf(1), nil
				}
				if err == nil {
					buckets[f] += d
				}
			}
		}
	}
	canary.P99, base.P99 = quantile(cb, 0.99), quantile(bb, 0.99)
	canary.Finish()
	base.Finish()
	return canary, base
}

// quantile interpolates within cumulative histogram buckets.
func quantile(b map[float64]float64, q float64) float64 {
	les := make([]float64, 0, len(b))
	for le := range b {
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 || b[les[len(les)-1]] <= 0 {
		return 0
	}
	rank := q * b[les[len(les)-1]]
	prevLe, prevC := 0.0, 0.0
	for _, le := range les {
		cnt := b[le]
		if cnt >= rank {
			if math.IsInf(le, 1) {
				return prevLe
			}
			if cnt == prevC {
				return le
			}
			return prevLe + (le-prevLe)*(rank-prevC)/(cnt-prevC)
		}
		prevLe, prevC = le, cnt
	}
	return prevLe
}
