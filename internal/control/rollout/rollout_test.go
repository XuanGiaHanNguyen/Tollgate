package rollout

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/store"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

func st(req, errs, p99ms float64) Stats {
	s := Stats{Requests: req, Errors: errs, P99: p99ms / 1000}
	s.Finish()
	return s
}

func TestDecide(t *testing.T) {
	p := DefaultPolicy()
	good := st(500, 5, 10)
	tests := []struct {
		name               string
		canary, base       Stats
		compared, sinceAll time.Duration
		want               Verdict
	}{
		{"too little traffic, early", st(10, 0, 10), good, time.Second, time.Second, Wait},
		{"too little traffic past max wait", st(10, 0, 10), good, time.Second, p.MaxWait, RollBack},
		{"error rate breach rolls back early", st(500, 150, 10), good, time.Second, time.Second, RollBack},
		{"error rate within delta", st(500, 25, 10), good, time.Second, time.Second, Wait},
		{"latency breach", st(500, 5, 60), good, time.Second, time.Second, RollBack},
		{"latency within ratio+slack", st(500, 5, 19), good, time.Second, time.Second, Wait},
		{"healthy for a full window promotes", st(500, 6, 11), good, p.Window, p.Window, Promote},
		{"failover: new leader needs its own full window", st(500, 6, 11), good, time.Second, 5 * p.Window, Wait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := Decide(p, tc.canary, tc.base, tc.compared, tc.sinceAll); got != tc.want {
				t.Fatalf("got %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestQuantileAndParse(t *testing.T) {
	text := `# HELP x
tollgate_requests_total{node="g",route="r",status="200"} 90
tollgate_requests_total{node="g",route="r",status="403"} 10
tollgate_request_duration_seconds_bucket{node="g",route="r",le="0.01"} 50
tollgate_request_duration_seconds_bucket{node="g",route="r",le="0.02"} 100
tollgate_request_duration_seconds_bucket{node="g",route="r",le="+Inf"} 100
other_metric 5
`
	s, err := parseMetrics(bufio.NewScanner(strings.NewReader(text)))
	if err != nil || len(s) != 5 {
		t.Fatalf("parsed %d series: %v", len(s), err)
	}
	c := &Controller{}
	canary, _ := c.compare("g", map[string]sample{"g": {}}, map[string]sample{"g": s})
	if canary.Requests != 100 || canary.ErrorRate != 0.1 || canary.P99 < 0.0195 || canary.P99 > 0.02 {
		t.Fatalf("stats %+v", canary)
	}
}

// ---- controller against a fake store and fake gateways ----

type fakeStore struct {
	mu     sync.Mutex
	fsm    *store.FSM
	idx    uint64
	leader bool
}

func (f *fakeStore) IsLeader() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.leader }
func (f *fakeStore) LocalGet(k string) (store.Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fsm.Get(k)
}
func (f *fakeStore) Propose(_ context.Context, c store.Command) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idx++
	_, err := f.fsm.Apply(raft.Entry{Index: f.idx, Term: 1, Data: c.Encode()})
	return f.idx, err
}

// gwSim serves /metrics with counters the test advances.
type gwSim struct {
	mu        sync.Mutex
	ok, deny  float64
	fast, all float64 // requests <= 10ms, total
	srv       *httptest.Server
}

func newGW(t *testing.T) *gwSim {
	g := &gwSim{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		fmt.Fprintf(w, "tollgate_requests_total{node=\"x\",route=\"r\",status=\"200\"} %g\n", g.ok)
		fmt.Fprintf(w, "tollgate_requests_total{node=\"x\",route=\"r\",status=\"403\"} %g\n", g.deny)
		fmt.Fprintf(w, "tollgate_request_duration_seconds_bucket{node=\"x\",route=\"r\",le=\"0.01\"} %g\n", g.fast)
		fmt.Fprintf(w, "tollgate_request_duration_seconds_bucket{node=\"x\",route=\"r\",le=\"0.1\"} %g\n", g.all)
		fmt.Fprintf(w, "tollgate_request_duration_seconds_bucket{node=\"x\",route=\"r\",le=\"+Inf\"} %g\n", g.all)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gwSim) traffic(n, denyFrac, slowFrac float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deny += n * denyFrac
	g.ok += n * (1 - denyFrac)
	g.fast += n * (1 - slowFrac)
	g.all += n
}

func setup(t *testing.T) (*Controller, *fakeStore, map[string]*gwSim) {
	fs := &fakeStore{fsm: store.New(), leader: true}
	gws := map[string]*gwSim{"gw-1": newGW(t), "gw-2": newGW(t), "gw-3": newGW(t)}
	urls := map[string]string{}
	for n, g := range gws {
		urls[n] = g.srv.URL
	}
	p := DefaultPolicy()
	p.Window, p.MaxWait = 200*time.Millisecond, 2*time.Second
	c := &Controller{Store: fs, Gateways: urls, Policy: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fs.Propose(context.Background(), store.Command{Op: "put", Key: KeyRules, Value: []byte("old")})
	rec := Record{ID: "r1", State: StateCanary, Candidate: []byte("new"), Canary: "gw-1", StartedTS: time.Now().UnixNano()}
	fs.Propose(context.Background(), store.Command{Op: "put", Key: KeyRollout, Value: rec.Encode()})
	return c, fs, gws
}

func drive(c *Controller, gws map[string]*gwSim, canaryDeny, canarySlow float64, steps int) {
	for i := 0; i < steps; i++ {
		for n, g := range gws {
			if n == "gw-1" {
				g.traffic(100, canaryDeny, canarySlow)
			} else {
				g.traffic(100, 0.01, 0)
			}
		}
		c.Step(context.Background())
		time.Sleep(60 * time.Millisecond)
	}
}

func state(t *testing.T, fs *fakeStore) (Record, string) {
	it, _ := fs.LocalGet(KeyRollout)
	r, err := Decode(it.Value)
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := fs.LocalGet(KeyRules)
	return r, string(rules.Value)
}

func TestControllerRollsBackBadErrorRate(t *testing.T) {
	c, fs, gws := setup(t)
	drive(c, gws, 0.30, 0, 6)
	r, rules := state(t, fs)
	if r.State != StateRolledBack || rules != "old" || !strings.Contains(r.Reason, "error rate") {
		t.Fatalf("state=%s rules=%s reason=%q", r.State, rules, r.Reason)
	}
	if h, ok := fs.LocalGet(KeyHistory + "r1"); !ok || !strings.Contains(string(h.Value), StateRolledBack) {
		t.Fatal("decision not recorded in history")
	}
}

func TestControllerRollsBackLatency(t *testing.T) {
	c, fs, gws := setup(t)
	drive(c, gws, 0, 0.5, 6)
	r, rules := state(t, fs)
	if r.State != StateRolledBack || rules != "old" || !strings.Contains(r.Reason, "p99") {
		t.Fatalf("state=%s rules=%s reason=%q", r.State, rules, r.Reason)
	}
}

func TestControllerPromotesHealthyCanary(t *testing.T) {
	c, fs, gws := setup(t)
	drive(c, gws, 0.01, 0, 8)
	r, rules := state(t, fs)
	if r.State != StatePromoted || rules != "new" {
		t.Fatalf("state=%s rules=%s reason=%q", r.State, rules, r.Reason)
	}
}

func TestFollowerNeverDecides(t *testing.T) {
	c, fs, gws := setup(t)
	fs.leader = false
	drive(c, gws, 0.9, 0, 6)
	if r, _ := state(t, fs); r.State != StateCanary {
		t.Fatalf("a follower decided: %s", r.State)
	}
}

// A stale leader whose decision races a newer one is rejected by the guard.
func TestGuardRejectsStaleDecision(t *testing.T) {
	c, fs, gws := setup(t)
	drive(c, gws, 0, 0, 1) // controller has read rollout at its current version
	it, _ := fs.LocalGet(KeyRollout)
	// someone else decides first
	fs.Propose(context.Background(), store.Command{Op: "put", Key: KeyRollout, Value: Record{ID: "r1", State: StatePromoted}.Encode()})
	_, err := fs.Propose(context.Background(), store.Command{Op: "txn", If: &store.Cond{Key: KeyRollout, Version: it.Version},
		Ops: []store.Command{{Op: "put", Key: KeyRules, Value: []byte("stale")}}})
	if err != store.ErrCondFailed {
		t.Fatalf("stale decision not rejected: %v", err)
	}
	_ = c
}
