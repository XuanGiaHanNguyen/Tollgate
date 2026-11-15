package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

type e9Run struct {
	Requests     int     `json:"requests"`
	OK           int     `json:"ok"`
	TTFTP50Ms    float64 `json:"ttft_p50_ms"`
	TTFTP95Ms    float64 `json:"ttft_p95_ms"`
	TTFTP99Ms    float64 `json:"ttft_p99_ms"`
	CostPerReqUS float64 `json:"cost_per_request_micro_usd"`
	Hedges       float64 `json:"hedges"`
	HedgesWon    float64 `json:"hedges_won"`
	HedgeRatePct float64 `json:"hedge_rate_pct"`
	BilledCalls  int64   `json:"provider_calls"` // calls the providers saw (incl. cancelled hedges)
}

type e9Mode struct {
	Name     string  `json:"name"`
	MaxPct   float64 `json:"hedge_max_pct"` // 0 = hedging off
	Runs     []e9Run `json:"runs"`
	Median   int     `json:"median_run"` // by TTFT p99
	ExtraPct float64 `json:"extra_cost_pct_vs_off"`
	P99Cut   float64 `json:"ttft_p99_change_pct_vs_off"`
}

func e9Rules(maxPct float64) string {
	hedge := ""
	if maxPct > 0 {
		hedge = fmt.Sprintf("  hedge after p95 max %g%%\n", maxPct)
	}
	return `route "/v1/chat/*" {
  require auth
  providers ["provider-a", "provider-b"]
` + hedge + `}
`
}

func runE9() (*benchfmt.Result, error) {
	const rate = 40.0
	w, d := warmup.Seconds(), measure.Seconds()
	llmArgs := func(seed int) []string {
		return []string{"-ttft-median", "100ms", "-ttft-sigma", "0.3", "-tail-prob", "0.05", "-tail-mult", "10",
			"-tps", "500", "-output-frac-min", "1", "-output-frac-max", "1", "-seed", fmt.Sprint(seed)}
	}
	res := newResult("E9", 1, map[string]any{
		"rate": rate, "providers": "2 x mockllm, TTFT log-normal median 100ms sigma 0.3, 5% of requests x10 (heavy tail), 500 tok/s",
		"request":  "max_tokens 20, ~10-word prompt, streaming",
		"hedge":    "after the primary's p95 TTFT (rolling 256-sample window, >= 20 samples)",
		"cost":     "true cost from both providers' billing at $1/1K input, $3/1K output; a cancelled hedge is billed its prompt",
		"ttft":     "measured by the client to the first content event",
		"warmup_s": w,
	})
	modes := []e9Mode{{Name: "off"}, {Name: "hedge_1pct", MaxPct: 1}, {Name: "hedge_5pct", MaxPct: 5}, {Name: "hedge_10pct", MaxPct: 10}}
	body := []byte(`{"model":"m","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"please summarize the key ideas of this short note"}]}`)
	raw := map[string]any{}
	for mi := range modes {
		m := &modes[mi]
		for rep := 0; rep < *reps; rep++ {
			c, err := startCluster(clusterSpec{gateways: 1, backends: 1,
				llms:         []llmSpec{{args: llmArgs(rep*2 + 1)}, {args: llmArgs(rep*2 + 2)}},
				rules:        e9Rules(m.MaxPct),
				aiYAML:       fmt.Sprintf(aiPricingYAML, "2m"),
				gatewayExtra: "auth:\n  api_keys: [{key: e9-key, user_id: e9, tier: free}]\n"})
			if err != nil {
				return nil, err
			}
			n := int(rate * (w + d))
			nWarm := int(rate * w)
			// Bill only the measured window: snapshot provider usage and
			// gateway metrics at the end of the warm-up.
			var before []mockUsage
			var mBefore promSample
			snap := make(chan struct{})
			go func() {
				time.Sleep(time.Duration(w * float64(time.Second)))
				for _, u := range c.llmURLs {
					mu, _ := readMockUsage(u)
					before = append(before, mu)
				}
				mBefore, _ = scrape(c.admins[0])
				close(snap)
			}()
			results := aiLoad(c.gateways, "e9-key", rate, n, func(int) []byte { return body })
			<-snap
			time.Sleep(500 * time.Millisecond)
			run := e9Run{}
			var ttfts []time.Duration
			for _, r := range results[nWarm:] {
				run.Requests++
				if r.err == nil && r.status == 200 && r.ttft > 0 {
					run.OK++
					ttfts = append(ttfts, r.ttft)
				}
			}
			ms := func(q float64) float64 { return round(float64(quantileDur(ttfts, q))/1e6, 2) }
			run.TTFTP50Ms, run.TTFTP95Ms, run.TTFTP99Ms = ms(0.5), ms(0.95), ms(0.99)
			var cost int64
			for i, u := range c.llmURLs {
				after, err := readMockUsage(u)
				if err != nil {
					c.stop()
					return nil, err
				}
				cost += microCost(after.Total.PromptTokens-before[i].Total.PromptTokens, after.Total.CompletionTokens-before[i].Total.CompletionTokens)
				run.BilledCalls += after.Total.Requests - before[i].Total.Requests
			}
			if run.OK > 0 {
				run.CostPerReqUS = round(float64(cost)/float64(run.OK), 2)
			}
			mAfter, _ := scrape(c.admins[0])
			for k, v := range mAfter {
				if !strings.HasPrefix(k, "tollgate_hedges_total") || !strings.Contains(k, `provider="provider-b"`) {
					continue
				}
				delta := v - mBefore[k]
				if strings.Contains(k, `outcome="won"`) {
					run.HedgesWon += delta
					run.Hedges += delta
				} else if strings.Contains(k, `outcome="lost"`) {
					run.Hedges += delta
				}
			}
			if run.Requests > 0 {
				run.HedgeRatePct = round(100*run.Hedges/float64(run.Requests), 3)
			}
			c.stop()
			raw[fmt.Sprintf("%s/%d", m.Name, rep)] = run
			m.Runs = append(m.Runs, run)
			log.Printf("E9 %s rep=%d ok=%d/%d ttft p50=%.1fms p99=%.1fms cost/req=%.0fµ$ hedges=%.0f won=%.0f rate=%.2f%%",
				m.Name, rep+1, run.OK, run.Requests, run.TTFTP50Ms, run.TTFTP99Ms, run.CostPerReqUS, run.Hedges, run.HedgesWon, run.HedgeRatePct)
			time.Sleep(time.Second)
		}
		_, m.Median = median(m.Runs, func(r e9Run) float64 { return r.TTFTP99Ms })
	}
	off := modes[0].Runs[modes[0].Median]
	for mi := range modes {
		mr := modes[mi].Runs[modes[mi].Median]
		modes[mi].ExtraPct = round(100*(mr.CostPerReqUS/off.CostPerReqUS-1), 2)
		modes[mi].P99Cut = round(100*(mr.TTFTP99Ms/off.TTFTP99Ms-1), 2)
	}
	res.Params["measure_seconds"] = d
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"modes": modes})
	res.Raw = mustJSON(raw)
	return res, nil
}
