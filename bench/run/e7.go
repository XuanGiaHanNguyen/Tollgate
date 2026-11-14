package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

const (
	e7BreakerOn = `    breaker: {window: 5s, min_requests: 20, error_rate: 0.3, cooldown: 2s, half_open_probes: 3}
`
	e7BreakerOff = `    breaker: {disabled: true}
`
)

type e7Phase struct {
	Latency   benchfmt.LatencyStats `json:"latency"`
	ErrorRate float64               `json:"error_rate"`
}

type e7Run struct {
	Phases map[string]e7Phase `json:"phases"` // before, failing, recovered
	// Breaker timings from polling /admin/upstreams every 10 ms; -1 if the
	// event never happened. Only meaningful with the breaker on.
	FailureToOpenMs    float64 `json:"failure_to_open_ms"`
	RecoveryToClosedMs float64 `json:"recovery_to_closed_ms"`
	OpensWhileFailing  int     `json:"opens_while_failing"`
	Dropped            float64 `json:"dropped_iterations"`
}

type e7Mode struct {
	Name   string  `json:"name"`
	Runs   []e7Run `json:"runs"`
	Median int     `json:"median_run"` // by failing-phase p99
}

type e7Summary struct {
	Modes []e7Mode `json:"modes"`
}

func runE7() (*benchfmt.Result, error) {
	w := int(warmup.Seconds())
	before, failing := 20, 40
	recovered := 30
	if *quick {
		before, failing, recovered = 3, 7, 5
	}
	failAt, recoverAt := w+before, w+before+failing
	end := recoverAt + recovered

	res := newResult("E7", 1, map[string]any{
		"rate": *e7Rate, "backends": 3, "backend_latency": "2ms + uniform jitter up to 3ms",
		"victim": "backend-1", "injected_error_rate": 0.5,
		"phases_seconds":  map[string]int{"warmup": w, "before": before, "failing": failing, "recovered": recovered},
		"breaker_on":      e7BreakerOn,
		"retries":         "attempts 2 (500s are not retried; only 502/503/504 and connection errors)",
		"passive_health":  "off in both modes, so only the breaker reacts",
		"poll_resolution": "10ms",
	})
	res.Params["measure_seconds"] = float64(before + failing + recovered)

	sum := e7Summary{}
	raw := map[string]any{}
	for _, mode := range []struct{ name, extra string }{{"breaker_on", e7BreakerOn}, {"breaker_off", e7BreakerOff}} {
		m := e7Mode{Name: mode.name}
		for rep := 0; rep < *reps; rep++ {
			c, err := startCluster(clusterSpec{backends: 3, gateways: 1, latency: 2 * time.Millisecond,
				jitter: 3 * time.Millisecond, upstreamExtra: mode.extra})
			if err != nil {
				return nil, err
			}
			run, rawRun, err := e7Once(c, failAt, recoverAt, end)
			c.stop()
			if err != nil {
				return nil, err
			}
			raw[fmt.Sprintf("%s/%d", mode.name, rep)] = rawRun
			m.Runs = append(m.Runs, run)
			f := run.Phases["failing"]
			log.Printf("E7 %s rep=%d failing p99=%.2fms err=%.4f open after %.0fms closed after %.0fms opens=%d",
				mode.name, rep+1, f.Latency.P99, f.ErrorRate, run.FailureToOpenMs, run.RecoveryToClosedMs, run.OpensWhileFailing)
			time.Sleep(2 * time.Second)
		}
		_, m.Median = median(m.Runs, func(r e7Run) float64 { return r.Phases["failing"].Latency.P99 })
		sum.Modes = append(sum.Modes, m)
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(sum)
	res.Raw = mustJSON(raw)
	return res, nil
}

type event struct {
	at   time.Time
	what string // "injected", "recovered", or a breaker state
}

func e7Once(c *cluster, failAt, recoverAt, end int) (e7Run, any, error) {
	victimURL := c.backends[0]
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var events []event
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lastState, lastRate := "closed", 0.0
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			var ups []struct {
				Targets []struct {
					URL     string `json:"url"`
					Breaker string `json:"breaker"`
				} `json:"targets"`
			}
			if err := getJSON(ctx, c.admins[0]+"/admin/upstreams", &ups); err == nil {
				for _, u := range ups {
					for _, t := range u.Targets {
						if t.URL == victimURL && t.Breaker != lastState {
							mu.Lock()
							events = append(events, event{time.Now(), t.Breaker})
							mu.Unlock()
							lastState = t.Breaker
						}
					}
				}
			}
			var cfg struct {
				Settings struct {
					ErrorRate float64 `json:"error_rate"`
				} `json:"settings"`
			}
			if err := getJSON(ctx, victimURL+"/admin/config", &cfg); err == nil && cfg.Settings.ErrorRate != lastRate {
				what := "injected"
				if cfg.Settings.ErrorRate == 0 {
					what = "recovered"
				}
				mu.Lock()
				events = append(events, event{time.Now(), what})
				mu.Unlock()
				lastRate = cfg.Settings.ErrorRate
			}
		}
	}()

	s, raw, err := runK6("bench/k6/e7.js", map[string]string{
		"TARGET":       c.gateways[0],
		"VICTIM_ADMIN": victimURL,
		"RATE":         strconv.Itoa(*e7Rate),
		"WARMUP":       secs(*warmup),
		"FAIL_AT":      strconv.Itoa(failAt),
		"RECOVER_AT":   strconv.Itoa(recoverAt),
		"END":          strconv.Itoa(end),
		"ERROR_RATE":   "0.5",
	})
	cancel()
	wg.Wait()
	if err != nil {
		return e7Run{}, nil, err
	}

	run := e7Run{Phases: map[string]e7Phase{}, FailureToOpenMs: -1, RecoveryToClosedMs: -1,
		Dropped: s.Value("dropped_iterations{scenario:load}", "count")}
	for _, p := range []string{"before", "failing", "recovered"} {
		lat, _ := s.Trend("http_req_duration{phase:" + p + "}")
		run.Phases[p] = e7Phase{Latency: lat, ErrorRate: s.Value("http_req_failed{phase:"+p+"}", "rate")}
	}

	var injected, recovered time.Time
	state := "closed"
	for _, e := range events {
		switch e.what {
		case "injected":
			injected = e.at
		case "recovered":
			recovered = e.at
			if state == "closed" {
				run.RecoveryToClosedMs = 0
			}
		default:
			state = e.what
			if e.what == "open" && !injected.IsZero() && recovered.IsZero() {
				if run.FailureToOpenMs < 0 {
					run.FailureToOpenMs = float64(e.at.Sub(injected).Milliseconds())
				}
				run.OpensWhileFailing++
			}
			if e.what == "closed" && !recovered.IsZero() && run.RecoveryToClosedMs < 0 {
				run.RecoveryToClosedMs = float64(e.at.Sub(recovered).Milliseconds())
			}
		}
	}
	return run, raw, nil
}
