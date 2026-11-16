package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/rollout"
)

const (
	e11Rate   = 300.0 // req/s over 3 gateways
	e11Base   = "route \"/*\" {\n  when query(\"b\") == \"1\" { set_header \"X-Bucket\" \"one\" }\n}\n"
	e11Deny   = "route \"/*\" {\n  when query(\"b\") == \"1\" { deny 403 }\n}\n" // b=1 on 30% of requests
	e11Slow   = "route \"/*\" {\n  set_header \"X-Mock-Delay\" \"50ms\"\n}\n"
	e11Good   = "route \"/*\" {\n  when query(\"b\") == \"1\" { set_header \"X-Bucket\" \"uno\" }\n}\n"
	e11Window = 10 * time.Second
)

type e11Case struct {
	Scenario       string  `json:"scenario"`
	Outcome        string  `json:"outcome"`
	Reason         string  `json:"reason"`
	DecisionMs     float64 `json:"commit_to_decision_ms"`
	ExposedPct     float64 `json:"exposed_pct"` // share of requests during the rollout served by the candidate
	ExposedReqs    int64   `json:"exposed_requests"`
	BadResponses   int64   `json:"bad_responses"` // 403s (or >150ms) the candidate actually served
	RolloutReqs    int64   `json:"rollout_requests"`
	LeaderKilled   bool    `json:"leader_killed,omitempty"`
	DecidedAfter   bool    `json:"decided_after_kill,omitempty"` // the new leader made the decision
	CanaryReverted bool    `json:"canary_reverted_or_promoted"`
}

type tagged struct {
	at     time.Time
	gw     int
	status int
	dur    time.Duration
}

func runE11() (*benchfmt.Result, error) {
	res := newResult("E11", 3, map[string]any{
		"control_nodes": 3, "gateways": 3, "rate": e11Rate, "window": e11Window.String(),
		"policy":    "roll back if canary error rate > baseline + 5 pts or p99 > 1.5x baseline + 5ms; promote after a healthy window; min 100 requests per side",
		"scenarios": "deny_30pct: candidate denies the 30% of requests with b=1; slow_50ms: candidate adds 50ms (X-Mock-Delay); good: harmless change; deny_30pct_leader_killed: control leader SIGKILLed 300ms into the canary, before any verdict is possible",
		"exposure":  "requests sent to the canary between rollout commit and decision, over all requests in that interval (round-robin over 3 gateways)",
	})
	res.Env.Topology = "local processes: 3 tollgate-control + 3 tollgate-gw + 3 mockbackend"
	delete(res.Params, "warmup_seconds")
	delete(res.Params, "measure_seconds")
	var all []e11Case
	for rep := 0; rep < *reps; rep++ {
		gwArg := []string{}
		for i := 1; i <= 3; i++ {
			gwArg = append(gwArg, fmt.Sprintf("gw-%d=http://127.0.0.1:%d", i, 19090+i))
		}
		c, err := startCluster(clusterSpec{control: 3, gateways: 3, backends: 3, rules: e11Base,
			controlArgs: []string{"-gateways", strings.Join(gwArg, ","), "-rollout-window", e11Window.String(), "-rollout-max-wait", "60s"}})
		if err != nil {
			return nil, err
		}
		cases, err := e11Once(c)
		c.stop()
		if err != nil {
			return nil, err
		}
		for _, cs := range cases {
			log.Printf("E11 rep=%d %s -> %s in %.0fms (after kill: %v), exposed %.1f%% (%d reqs, %d bad), canary ok after: %v; reason: %s",
				rep+1, cs.Scenario, cs.Outcome, cs.DecisionMs, cs.DecidedAfter, cs.ExposedPct, cs.ExposedReqs, cs.BadResponses, cs.CanaryReverted, cs.Reason)
		}
		all = append(all, cases...)
		time.Sleep(time.Second)
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"cases": all})
	res.Raw = mustJSON(all)
	return res, nil
}

func e11Once(c *cluster) ([]e11Case, error) {
	cl := client.New(c.ctlEndpoints())
	defer cl.Close()
	cl.SetMaxBackoff(20 * time.Millisecond)

	// Steady traffic, round-robin over gateways, 30% with b=1.
	var mu sync.Mutex
	var reqs []tagged
	stop := make(chan struct{})
	var wg sync.WaitGroup
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	var seq atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		gap := time.Second / time.Duration(e11Rate)
		t := time.NewTicker(gap)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			i := seq.Add(1)
			wg.Add(1)
			go func(i int64) {
				defer wg.Done()
				gw := int(i % 3)
				q := "b=0"
				if i%10 < 3 {
					q = "b=1"
				}
				start := time.Now()
				resp, err := hc.Get(c.gateways[gw] + "/e11?" + q)
				st := 0
				if err == nil {
					st = resp.StatusCode
					resp.Body.Close()
				}
				mu.Lock()
				reqs = append(reqs, tagged{start, gw, st, time.Since(start)})
				mu.Unlock()
			}(i)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	time.Sleep(3 * time.Second) // warm up: TTFB baselines and full traffic

	type scen struct {
		name, src string
		kill      bool
	}
	var out []e11Case
	for _, s := range []scen{{"deny_30pct", e11Deny, false}, {"slow_50ms", e11Slow, false}, {"good", e11Good, false}, {"deny_30pct_leader_killed", e11Deny, true}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := cl.Put(ctx, rollout.KeyRollout, rollout.Record{Candidate: []byte(s.src), Canary: "gw-1"}.Encode())
		cancel()
		if err != nil {
			return out, fmt.Errorf("%s: start rollout: %w", s.name, err)
		}
		started := time.Now()
		var killedAt time.Time
		if s.kill {
			// Kill well before any verdict is possible (it needs ~1s of
			// traffic), so the decision must come from the new leader.
			time.Sleep(300 * time.Millisecond)
			ld, err := c.ctlLeader(5 * time.Second)
			if err != nil {
				return out, err
			}
			_ = ld.proc.Process.Signal(syscall.SIGKILL)
			ld.proc.Wait()
			killedAt = time.Now()
		}
		// Wait for the decision.
		var rec rollout.Record
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			r, err := cl.Get(ctx, rollout.KeyRollout)
			cancel()
			if err == nil && r.Found {
				rec, _ = rollout.Decode(r.Value)
				if rec.State != rollout.StateCanary {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		decided := time.Now()
		cs := e11Case{Scenario: s.name, Outcome: rec.State, Reason: rec.Reason, LeaderKilled: s.kill}
		if rec.DecidedTS > 0 {
			cs.DecisionMs = round(float64(rec.DecidedTS-rec.StartedTS)/1e6, 0)
			if s.kill {
				cs.DecidedAfter = rec.DecidedTS > killedAt.UnixNano()
			}
		}
		mu.Lock()
		for _, r := range reqs {
			if r.at.Before(started) || r.at.After(decided) {
				continue
			}
			cs.RolloutReqs++
			if r.gw == 0 { // gw-1 is the canary
				cs.ExposedReqs++
				if (s.src == e11Deny && r.status == 403) || (s.src == e11Slow && r.dur > 50*time.Millisecond) {
					cs.BadResponses++
				}
			}
		}
		mu.Unlock()
		if cs.RolloutReqs > 0 {
			cs.ExposedPct = round(100*float64(cs.ExposedReqs)/float64(cs.RolloutReqs), 2)
		}
		// The canary must be back on the active rules (or everyone on the
		// promoted ones): probe it.
		time.Sleep(300 * time.Millisecond)
		resp, err := hc.Get(c.gateways[0] + "/e11?b=1")
		if err == nil {
			cs.CanaryReverted = resp.StatusCode == 200
			resp.Body.Close()
		}
		out = append(out, cs)
		if s.kill {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return out, nil
}
