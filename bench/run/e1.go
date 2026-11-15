package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

type e1Run struct {
	Latency     benchfmt.LatencyStats `json:"latency"`
	ErrorRate   float64               `json:"error_rate"`
	Requests    float64               `json:"requests"`
	Dropped     float64               `json:"dropped_iterations"`
	AchievedRPS float64               `json:"achieved_rps"`
	// Gateway-side, summed over gateways; zero for the direct baseline.
	CPUCores        float64 `json:"gateway_cpu_cores,omitempty"`
	RSSMB           float64 `json:"gateway_rss_mb,omitempty"`
	OverheadP50Usec float64 `json:"gateway_overhead_p50_us,omitempty"`
	OverheadP99Usec float64 `json:"gateway_overhead_p99_us,omitempty"`
}

type e1Level struct {
	Rate   int     `json:"rate"`
	Runs   []e1Run `json:"runs"`
	Median int     `json:"median_run"` // index into Runs, by p99
	Pass   bool    `json:"pass"`
	Reason string  `json:"reason,omitempty"`
}

type e1Target struct {
	Name            string    `json:"name"`
	Gateways        int       `json:"gateways"`
	Levels          []e1Level `json:"levels"`
	MaxSustainedRPS int       `json:"max_sustained_rps"`
}

type e1Overhead struct {
	Rate     int     `json:"rate"`
	Target   string  `json:"target"`
	AddP50Ms float64 `json:"added_p50_ms"`
	AddP99Ms float64 `json:"added_p99_ms"`
}

type e1Summary struct {
	P99LimitMs float64      `json:"p99_limit_ms"`
	Targets    []e1Target   `json:"targets"`
	Overhead   []e1Overhead `json:"overhead_vs_direct"`
}

func runE1() (*benchfmt.Result, error) {
	var rates []int
	for _, f := range strings.Split(*levels, ",") {
		r, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("bad -levels: %v", err)
		}
		rates = append(rates, r)
	}
	names := strings.Split(*targets, ",")
	res := newResult("E1", 3, map[string]any{
		"levels": rates, "p99_limit_ms": float64(*p99Limit) / 1e6, "targets": names,
		"backends": 3, "backend_latency": "0", "path": "GET /e1",
		"pass_rule": "p99 < limit, error rate < 0.1%, dropped iterations < 1%",
	})
	sum := e1Summary{P99LimitMs: float64(*p99Limit) / 1e6}
	raw := map[string]any{}

	for _, name := range names {
		gws, ok := map[string]int{"direct": 0, "gw1": 1, "gw3": 3}[name]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", name)
		}
		c, err := startCluster(clusterSpec{backends: 3, gateways: gws})
		if err != nil {
			return nil, err
		}
		urls := c.gateways
		if gws == 0 {
			urls = c.backends
		}
		tgt := e1Target{Name: name, Gateways: gws}
		for _, rate := range rates {
			lvl := e1Level{Rate: rate}
			for rep := 0; rep < *reps; rep++ {
				run, rawRun, err := e1Once(c, urls, rate)
				if err != nil {
					c.stop()
					return nil, err
				}
				raw[fmt.Sprintf("%s/%d/%d", name, rate, rep)] = rawRun
				lvl.Runs = append(lvl.Runs, run)
				log.Printf("E1 %s rate=%d rep=%d p50=%.2fms p99=%.2fms err=%.4f dropped=%.0f cpu=%.2f",
					name, rate, rep+1, run.Latency.P50, run.Latency.P99, run.ErrorRate, run.Dropped, run.CPUCores)
				time.Sleep(2 * time.Second)
			}
			med, idx := median(lvl.Runs, func(r e1Run) float64 { return r.Latency.P99 })
			lvl.Median = idx
			expected := float64(rate) * measure.Seconds()
			switch {
			case med.Latency.P99 >= float64(*p99Limit)/1e6:
				lvl.Reason = fmt.Sprintf("p99 %.2f ms >= limit", med.Latency.P99)
			case med.ErrorRate >= 0.001:
				lvl.Reason = fmt.Sprintf("error rate %.4f", med.ErrorRate)
			case med.Dropped >= 0.01*expected:
				lvl.Reason = fmt.Sprintf("load generator dropped %.0f of %.0f iterations", med.Dropped, expected)
			default:
				lvl.Pass = true
				tgt.MaxSustainedRPS = rate
			}
			tgt.Levels = append(tgt.Levels, lvl)
			if !lvl.Pass {
				log.Printf("E1 %s stops at rate=%d: %s", name, rate, lvl.Reason)
				break
			}
		}
		c.stop()
		sum.Targets = append(sum.Targets, tgt)
	}

	// Added latency vs the direct baseline at each rate both measured.
	var direct *e1Target
	for i := range sum.Targets {
		if sum.Targets[i].Name == "direct" {
			direct = &sum.Targets[i]
		}
	}
	if direct != nil {
		for _, t := range sum.Targets {
			if t.Gateways == 0 {
				continue
			}
			for _, l := range t.Levels {
				for _, d := range direct.Levels {
					if d.Rate != l.Rate {
						continue
					}
					g, b := l.Runs[l.Median], d.Runs[d.Median]
					sum.Overhead = append(sum.Overhead, e1Overhead{
						Rate: l.Rate, Target: t.Name,
						AddP50Ms: round(g.Latency.P50-b.Latency.P50, 3),
						AddP99Ms: round(g.Latency.P99-b.Latency.P99, 3),
					})
				}
			}
		}
	}

	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(sum)
	res.Raw = mustJSON(raw)
	return res, nil
}

func e1Once(c *cluster, urls []string, rate int) (e1Run, any, error) {
	type k6Result struct {
		s   *benchfmt.K6Summary
		raw any
		err error
	}
	done := make(chan k6Result, 1)
	go func() {
		s, raw, err := runK6("bench/k6/e1.js", *warmup+*measure, map[string]string{
			"TARGETS":  strings.Join(urls, ","),
			"RATE":     strconv.Itoa(rate),
			"DURATION": secs(*measure) + "s",
			"WARMUP":   secs(*warmup) + "s",
		})
		done <- k6Result{s, raw, err}
	}()

	// Gateway CPU, memory and overhead over the measured window.
	var before []promSample
	var t0 time.Time
	if len(c.admins) > 0 {
		time.Sleep(*warmup)
		t0 = time.Now()
		for _, a := range c.admins {
			s, err := scrape(a)
			if err != nil {
				return e1Run{}, nil, err
			}
			before = append(before, s)
		}
	}
	r := <-done
	if r.err != nil {
		return e1Run{}, nil, r.err
	}
	s := r.s
	lat, _ := s.Trend("http_req_duration{scenario:measure}")
	run := e1Run{
		Latency:     lat,
		ErrorRate:   s.Value("http_req_failed{scenario:measure}", "rate"),
		Requests:    s.Value("http_reqs{scenario:measure}", "count"),
		Dropped:     s.Value("dropped_iterations{scenario:measure}", "count"),
		AchievedRPS: s.Value("http_reqs{scenario:measure}", "count") / measure.Seconds(),
	}
	if len(c.admins) > 0 {
		elapsed := time.Since(t0).Seconds()
		mergedBefore, mergedAfter := promSample{}, promSample{}
		for i, a := range c.admins {
			after, err := scrape(a)
			if err != nil {
				return e1Run{}, nil, err
			}
			run.CPUCores += (after["process_cpu_seconds_total"] - before[i]["process_cpu_seconds_total"]) / elapsed
			run.RSSMB += after["process_resident_memory_bytes"] / (1 << 20)
			for k, v := range before[i] {
				mergedBefore[k] += v
			}
			for k, v := range after {
				mergedAfter[k] += v
			}
		}
		run.CPUCores = round(run.CPUCores, 3)
		run.RSSMB = round(run.RSSMB, 1)
		run.OverheadP50Usec = round(histQuantile(mergedBefore, mergedAfter, "tollgate_gateway_overhead_seconds", 0.50)*1e6, 1)
		run.OverheadP99Usec = round(histQuantile(mergedBefore, mergedAfter, "tollgate_gateway_overhead_seconds", 0.99)*1e6, 1)
	}
	return run, r.raw, nil
}
