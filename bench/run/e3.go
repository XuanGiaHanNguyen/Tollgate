package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/ring"
)

const (
	e3Limit = 1000 // per second
	e3Rate  = 6000 // offered req/s: 6x the limit, high enough that peer checks overlap and batching matters
	e3Rules = `route "/*" {
  require auth
  limit 1000 per second by user.id
}
`
)

type e3Phase struct {
	Requests      float64               `json:"requests"`
	Allowed       float64               `json:"allowed"`
	Limited       float64               `json:"limited"`
	Other         float64               `json:"other_status"`
	Seconds       float64               `json:"seconds"`
	Ideal         float64               `json:"ideal_allowed"`
	OverAdmission float64               `json:"over_admission_pct"` // (allowed-ideal)/ideal*100
	Latency       benchfmt.LatencyStats `json:"latency"`
}

type e3Run struct {
	Phases      map[string]e3Phase `json:"phases"`
	PeerRPCs    float64            `json:"peer_rpcs"`   // CheckLimits calls, all gateways
	PeerChecks  float64            `json:"peer_checks"` // checks carried by those calls
	RPCsPerReq  float64            `json:"peer_rpcs_per_request"`
	MeanBatch   float64            `json:"mean_batch_size"`
	Degraded    float64            `json:"degraded_decisions"`
	Dropped     float64            `json:"dropped_iterations"`
	KilledAtSec float64            `json:"killed_at_s,omitempty"`
}

type e3Mode struct {
	Name     string  `json:"name"`
	Gateways int     `json:"gateways"`
	Targets  string  `json:"targets"`
	Runs     []e3Run `json:"runs"`
	Median   int     `json:"median_run"`
}

func runE3() (*benchfmt.Result, error) {
	w, d := int(warmup.Seconds()), int(measure.Seconds())
	killAfter := 20 // seconds into the measured window
	if killAfter > d/3 {
		killAfter = d / 3
	}
	// A user whose limit key (limit ID 0) is owned by gw-3, so the kill run
	// can take the owner down while traffic keeps flowing through gw-1/gw-2.
	r3, _ := ring.New([]string{"gw-1", "gw-2", "gw-3"}, 128)
	user := ""
	for i := 0; ; i++ {
		u := fmt.Sprintf("e3-user-%d", i)
		if r3.Owner("0|"+u) == "gw-3" {
			user = u
			break
		}
	}
	auth := fmt.Sprintf("auth:\n  api_keys: [{key: e3-key, user_id: %s, tier: free}]\n", user)

	res := newResult("E3", 3, map[string]any{
		"limit": fmt.Sprintf("%d per second by user.id (token bucket)", e3Limit), "offered_rate": e3Rate,
		"key_owner": "gw-3", "ideal": "limit × measured seconds (the warm-up drains the initial burst)",
		"batching": "batched: max 100 / flush 500us; unbatched: max 1", "peer_timeout": "20ms",
		"kill_run": fmt.Sprintf("traffic via gw-1, gw-2 only; gw-3 (the owner) SIGKILLed %ds into the measured window; fallback allows limit/3 per gateway", killAfter),
	})

	modes := []struct {
		name    string
		spec    clusterSpec
		targets func(c *cluster) []string
		kill    bool
	}{
		{"single_gateway", clusterSpec{gateways: 1}, func(c *cluster) []string { return c.gateways }, false},
		{"cluster_batched", clusterSpec{gateways: 3, cluster: true, batchMax: 100, batchFlush: 500 * time.Microsecond}, func(c *cluster) []string { return c.gateways }, false},
		{"cluster_unbatched", clusterSpec{gateways: 3, cluster: true, batchMax: 1, batchFlush: 500 * time.Microsecond}, func(c *cluster) []string { return c.gateways }, false},
		{"cluster_kill_owner", clusterSpec{gateways: 3, cluster: true, batchMax: 100, batchFlush: 500 * time.Microsecond}, func(c *cluster) []string { return c.gateways[:2] }, true},
	}
	sum := map[string]any{}
	var out []e3Mode
	raw := map[string]any{}
	for _, mo := range modes {
		m := e3Mode{Name: mo.name, Gateways: mo.spec.gateways}
		for rep := 0; rep < *reps; rep++ {
			spec := mo.spec
			spec.backends, spec.rules, spec.gatewayExtra = 3, e3Rules, auth
			c, err := startCluster(spec)
			if err != nil {
				return nil, err
			}
			targets := mo.targets(c)
			m.Targets = strings.Join(targets, ",")
			run, rawRun, err := e3Once(c, targets, w, d, mo.kill, killAfter)
			c.stop()
			if err != nil {
				return nil, err
			}
			raw[fmt.Sprintf("%s/%d", mo.name, rep)] = rawRun
			m.Runs = append(m.Runs, run)
			for name, p := range run.Phases {
				log.Printf("E3 %s rep=%d phase=%s allowed=%.0f ideal=%.0f over=%.2f%% p99=%.2fms rpcs/req=%.3f batch=%.1f degraded=%.0f",
					mo.name, rep+1, name, p.Allowed, p.Ideal, p.OverAdmission, p.Latency.P99, run.RPCsPerReq, run.MeanBatch, run.Degraded)
			}
			time.Sleep(2 * time.Second)
		}
		key := "measure"
		if mo.kill {
			key = "after"
		}
		_, m.Median = median(m.Runs, func(r e3Run) float64 { return r.Phases[key].OverAdmission })
		out = append(out, m)
	}
	sum["modes"] = out
	res.Params["measure_seconds"] = float64(d)
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(sum)
	res.Raw = mustJSON(raw)
	return res, nil
}

func e3Once(c *cluster, targets []string, warm, dur int, kill bool, killAfter int) (e3Run, any, error) {
	env := map[string]string{
		"TARGETS": strings.Join(targets, ","), "RATE": strconv.Itoa(e3Rate),
		"WARMUP": strconv.Itoa(warm), "DURATION": strconv.Itoa(dur), "API_KEY": "e3-key",
	}
	killAt := 0
	if kill {
		killAt = warm + killAfter
		env["KILL_AT"] = strconv.Itoa(killAt)
	}

	type k6res struct {
		s   *benchfmt.K6Summary
		raw any
		err error
	}
	done := make(chan k6res, 1)
	start := time.Now()
	go func() {
		s, raw, err := runK6("bench/k6/e3.js", time.Duration(warm+dur)*time.Second, env)
		done <- k6res{s, raw, err}
	}()

	// k6 takes a moment to start; anchor phase timing on the first request
	// reaching a gateway, so the kill lines up with k6's phase boundary.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s, err := scrape(c.admins[0]); err == nil && sumPrefix(s, "tollgate_requests_total") > 0 {
			start = time.Now()
			break
		}
	}
	time.Sleep(time.Until(start.Add(time.Duration(warm) * time.Second)))
	before := make([]promSample, len(c.admins))
	for i, a := range c.admins {
		before[i], _ = scrape(a)
	}
	var killedAt float64
	if kill {
		time.Sleep(time.Until(start.Add(time.Duration(killAt) * time.Second)))
		killedAt = time.Since(start).Seconds()
		_ = c.gwProcs[2].Process.Signal(syscall.SIGKILL)
	}
	r := <-done
	if r.err != nil {
		return e3Run{}, nil, r.err
	}
	run := e3Run{Phases: map[string]e3Phase{}, Dropped: r.s.Value("dropped_iterations", "count"), KilledAtSec: round(killedAt, 3)}

	phases := map[string]float64{"measure": float64(dur)}
	if kill {
		phases = map[string]float64{"before": float64(killAfter), "after": float64(dur - killAfter)}
	}
	for name, secs := range phases {
		p := e3Phase{
			Requests: r.s.Value("http_reqs{phase:"+name+"}", "count"),
			Allowed:  r.s.Value("http_reqs{phase:"+name+",status:200}", "count"),
			Limited:  r.s.Value("http_reqs{phase:"+name+",status:429}", "count"),
			Seconds:  secs,
			Ideal:    e3Limit * secs,
		}
		p.Other = p.Requests - p.Allowed - p.Limited
		p.OverAdmission = round(100*(p.Allowed-p.Ideal)/p.Ideal, 3)
		p.Latency, _ = r.s.Trend("http_req_duration{phase:" + name + "}")
		run.Phases[name] = p
	}

	// Peer RPC counts over the measured window, from the gateways still up.
	var reqs float64
	for _, p := range run.Phases {
		reqs += p.Requests
	}
	for i, a := range c.admins {
		after, err := scrape(a)
		if err != nil {
			continue // killed gateway
		}
		for k, v := range after {
			switch {
			case strings.HasPrefix(k, "tollgate_ratelimit_batch_size_count"):
				run.PeerRPCs += v - before[i][k]
			case strings.HasPrefix(k, "tollgate_ratelimit_batch_size_sum"):
				run.PeerChecks += v - before[i][k]
			case strings.HasPrefix(k, "tollgate_ratelimit_decisions_total") && strings.Contains(k, `result="degraded"`):
				run.Degraded += v - before[i][k]
			}
		}
	}
	if reqs > 0 {
		run.RPCsPerReq = round(run.PeerRPCs/reqs, 4)
	}
	if run.PeerRPCs > 0 {
		run.MeanBatch = round(run.PeerChecks/run.PeerRPCs, 2)
	}
	return run, r.raw, nil
}

func sumPrefix(s promSample, prefix string) float64 {
	var t float64
	for k, v := range s {
		if strings.HasPrefix(k, prefix) {
			t += v
		}
	}
	return t
}
