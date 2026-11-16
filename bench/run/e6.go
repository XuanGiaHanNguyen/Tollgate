package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
)

type e6Run struct {
	Changes  int       `json:"changes"`
	PerGWMs  []float64 `json:"per_gateway_ms"`  // commit ack -> active, each gateway, each change
	AllGWMs  []float64 `json:"all_gateways_ms"` // commit ack -> active on every gateway
	P50Ms    float64   `json:"all_p50_ms"`
	P99Ms    float64   `json:"all_p99_ms"`
	GWP50Ms  float64   `json:"per_gateway_p50_ms"`
	GWP99Ms  float64   `json:"per_gateway_p99_ms"`
	TimedOut int       `json:"timed_out"`
}

func runE6() (*benchfmt.Result, error) {
	changes := 100
	if *quick {
		changes = 20
	}
	res := newResult("E6", 3, map[string]any{
		"control_nodes": 3, "gateways": 3, "changes": changes,
		"measure": "from the leader acknowledging the commit (Put returns) until each gateway's /admin/rules reports that version (polled every 1ms)",
		"change":  "a different rule file each time (deny status varies), compiled by the leader before commit and by each gateway after",
	})
	res.Env.Topology = "local processes: 3 tollgate-control + 3 tollgate-gw (watch streams)"
	delete(res.Params, "warmup_seconds")
	delete(res.Params, "measure_seconds")
	var runs []e6Run
	for rep := 0; rep < *reps; rep++ {
		c, err := startCluster(clusterSpec{control: 3, gateways: 3, backends: 1, rules: `route "/*" { deny 401 }` + "\n"})
		if err != nil {
			return nil, err
		}
		run, err := e6Once(c, changes)
		c.stop()
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
		log.Printf("E6 rep=%d changes=%d all-gateways p50=%.2fms p99=%.2fms per-gateway p50=%.2fms p99=%.2fms timeouts=%d",
			rep+1, run.Changes, run.P50Ms, run.P99Ms, run.GWP50Ms, run.GWP99Ms, run.TimedOut)
		time.Sleep(time.Second)
	}
	_, medianIdx := median(runs, func(r e6Run) float64 { return r.P99Ms })
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"runs": runs, "median_run": medianIdx})
	res.Raw = mustJSON(runs)
	return res, nil
}

func e6Once(c *cluster, changes int) (e6Run, error) {
	cl := client.New(c.ctlEndpoints())
	defer cl.Close()
	run := e6Run{Changes: changes}
	for i := 0; i < changes; i++ {
		src := fmt.Sprintf("route \"/*\" { deny %d }\n", 400+i%100)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ver, err := cl.Put(ctx, "rules", []byte(src))
		cancel()
		if err != nil {
			return run, err
		}
		acked := time.Now()
		var wg sync.WaitGroup
		times := make([]float64, len(c.admins))
		for gi, a := range c.admins {
			wg.Add(1)
			go func(gi int, a string) {
				defer wg.Done()
				deadline := acked.Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var st struct {
						Version uint64 `json:"version"`
					}
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					err := getJSON(ctx, a+"/admin/rules", &st)
					cancel()
					if err == nil && st.Version >= ver {
						times[gi] = float64(time.Since(acked).Microseconds()) / 1000
						return
					}
					time.Sleep(time.Millisecond)
				}
				times[gi] = -1
			}(gi, a)
		}
		wg.Wait()
		worst := 0.0
		for _, t := range times {
			if t < 0 {
				run.TimedOut++
				continue
			}
			run.PerGWMs = append(run.PerGWMs, round(t, 3))
			if t > worst {
				worst = t
			}
		}
		run.AllGWMs = append(run.AllGWMs, round(worst, 3))
		time.Sleep(20 * time.Millisecond)
	}
	run.P50Ms, run.P99Ms = round(pct(run.AllGWMs, 0.5), 3), round(pct(run.AllGWMs, 0.99), 3)
	run.GWP50Ms, run.GWP99Ms = round(pct(run.PerGWMs, 0.5), 3), round(pct(run.PerGWMs, 0.99), 3)
	return run, nil
}
