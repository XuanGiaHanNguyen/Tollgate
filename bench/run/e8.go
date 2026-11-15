package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"strings"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/ring"
)

const (
	e8BudgetUSD = 100.0
	e8Requests  = 400
	e8Rate      = 8.0
	e8TTL       = 5 * time.Second
)

type e8Run struct {
	Accepted      int     `json:"accepted"`
	Rejected      int     `json:"rejected_429"`
	Failed        int     `json:"failed"` // connection errors (e.g. killed gateway)
	SpendUSD      float64 `json:"true_spend_usd"`
	OverspendPct  float64 `json:"overspend_pct"`
	EstErrMean    float64 `json:"estimate_error_mean"` // (estimate-actual)/actual, mean over settled requests
	Expired       float64 `json:"reservations_expired"`
	LeakedHolds   int64   `json:"leaked_holds_after_ttl"`
	KilledAtReq   int     `json:"killed_at_request,omitempty"`
	CancelledByLL int64   `json:"provider_cancelled_streams"`
	Degraded      float64 `json:"degraded_limit_decisions"`
}

type e8Mode struct {
	Name   string  `json:"name"`
	Rules  string  `json:"rules"`
	Runs   []e8Run `json:"runs"`
	Median int     `json:"median_run"` // by overspend
}

// e8Mix returns the request bodies for one seeded run: 80% small prompts
// (50 words, max_tokens 100), 20% very large (3000 words, max_tokens 2000).
func e8Mix(seed uint64) [][]byte {
	r := rand.New(rand.NewPCG(seed, 8))
	words := []string{"budget", "token", "stream", "gateway", "model", "cost", "limit", "user", "request", "answer"}
	prompt := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteByte(' ')
		}
		return b.String()
	}
	out := make([][]byte, e8Requests)
	for i := range out {
		n, max := 50, 100
		if r.IntN(5) == 0 {
			n, max = 3000, 2000
		}
		out[i] = []byte(fmt.Sprintf(`{"model":"m","max_tokens":%d,"stream":true,"messages":[{"role":"user","content":%q}]}`, max, prompt(n)))
	}
	return out
}

func runE8() (*benchfmt.Result, error) {
	// Expected true cost per request of the mix at $1/1K in, $3/1K out with
	// output uniform in [0.2, 1.0] x max_tokens (mean 0.6):
	// small 54 in + 60 out = $0.234; large 3004 in + 1200 out = $6.604.
	meanCost := 0.8*0.234 + 0.2*6.604
	countLimit := int(math.Round(e8BudgetUSD / meanCost))

	// The user's budget and limit keys are owned by gw-3; traffic goes
	// through gw-1 and gw-2, so every hold is remote and gw-1 can be killed
	// mid-stream while gw-3 keeps the holds.
	r3, _ := ring.New([]string{"gw-1", "gw-2", "gw-3"}, 128)
	user := ""
	for i := 0; ; i++ {
		u := fmt.Sprintf("e8-user-%d", i)
		if r3.Owner("b0|"+u) == "gw-3" && r3.Owner("0|"+u) == "gw-3" {
			user = u
			break
		}
	}
	auth := fmt.Sprintf("auth:\n  api_keys: [{key: e8-key, user_id: %s, tier: free}]\n", user)
	setHdr := "  set_header \"X-User-Id\" user.id\n"
	modes := []e8Mode{
		{Name: "request_count_limit", Rules: fmt.Sprintf("route \"/v1/chat/*\" {\n  require auth\n  limit %d per day by user.id\n%s}\n", countLimit, setHdr)},
		{Name: "cost_budget", Rules: fmt.Sprintf("route \"/v1/chat/*\" {\n  require auth\n  budget %.2f usd per day by user.id\n%s}\n", e8BudgetUSD, setHdr)},
		{Name: "cost_budget_kill_gateway", Rules: fmt.Sprintf("route \"/v1/chat/*\" {\n  require auth\n  budget %.2f usd per day by user.id\n%s}\n", e8BudgetUSD, setHdr)},
	}
	res := newResult("E8", 3, map[string]any{
		"budget_usd": e8BudgetUSD, "requests": e8Requests, "rate": e8Rate, "user_key_owner": "gw-3 (traffic via gw-1, gw-2)",
		"mix":             "80% small (50-word prompt, max_tokens 100), 20% large (3000-word prompt, max_tokens 2000); seeded per rep, same across modes",
		"provider":        "1 x mockllm, TTFT 50ms, 1000 tok/s, output uniform 0.2-1.0 x max_tokens; prompt tokens counted by words",
		"pricing":         "$1/1K input, $3/1K output; true spend from the provider's per-user billing",
		"count_limit":     fmt.Sprintf("%d per day = budget / expected cost per request ($%.3f)", countLimit, meanCost),
		"reservation_ttl": e8TTL.String(),
		"kill":            "gw-1 SIGKILLed after 10% of requests were sent, while the budget still had room and large streams were in flight",
		"leak_check":      "active holds on gw-3 after every reservation deadline has passed",
		"duration_note":   "a budget-accounting run, not a throughput run: 400 requests at 8 req/s",
	})
	raw := map[string]any{}
	for mi := range modes {
		m := &modes[mi]
		for rep := 0; rep < *reps; rep++ {
			c, err := startCluster(clusterSpec{gateways: 3, backends: 1, cluster: true, batchMax: 100, batchFlush: 500 * time.Microsecond,
				llms:         []llmSpec{{args: []string{"-ttft-median", "50ms", "-ttft-sigma", "0.2", "-tps", "1000", "-output-frac-min", "0.2", "-output-frac-max", "1.0", "-seed", fmt.Sprint(rep + 1)}}},
				rules:        m.Rules,
				aiYAML:       fmt.Sprintf(aiPricingYAML, e8TTL),
				gatewayExtra: auth})
			if err != nil {
				return nil, err
			}
			run := e8Run{}
			bodies := e8Mix(uint64(rep + 1))
			kill := m.Name == "cost_budget_kill_gateway"
			urls := c.gateways[:2]
			if kill {
				run.KilledAtReq = e8Requests / 10 // early: budget still open, large streams in flight
				go func() {
					time.Sleep(time.Duration(float64(run.KilledAtReq) / e8Rate * float64(time.Second)))
					_ = c.gwProcs[0].Process.Signal(syscall.SIGKILL)
				}()
			}
			results := aiLoad(urls, "e8-key", e8Rate, e8Requests, func(i int) []byte { return bodies[i] })
			for _, r := range results {
				switch {
				case r.err != nil || r.status == 0:
					run.Failed++
				case r.status == 429:
					run.Rejected++
				case r.status == 200:
					run.Accepted++
				default:
					run.Failed++
				}
			}
			// Wait past every reservation deadline (+ expiry tick), then
			// check what the owner still holds.
			time.Sleep(e8TTL + 2*time.Second)
			var st struct {
				Active  int64 `json:"Active"`
				Expired int64 `json:"Expired"`
			}
			if err := getJSON(context.Background(), c.admins[2]+"/admin/budgets", &st); err != nil {
				c.stop()
				return nil, err
			}
			run.LeakedHolds, run.Expired = st.Active, float64(st.Expired)
			u, err := readMockUsage(c.llmURLs[0])
			if err != nil {
				c.stop()
				return nil, err
			}
			bu := u.ByUser[user]
			run.CancelledByLL = bu.Cancelled
			spend := float64(microCost(bu.PromptTokens, bu.CompletionTokens)) / 1e6
			run.SpendUSD = round(spend, 4)
			run.OverspendPct = round(100*(spend-e8BudgetUSD)/e8BudgetUSD, 3)
			var sum, cnt float64
			for _, a := range c.admins {
				s, err := scrape(a)
				if err != nil {
					continue
				}
				sum += sumPrefix(s, "tollgate_cost_estimate_error_ratio_sum")
				cnt += sumPrefix(s, "tollgate_cost_estimate_error_ratio_count")
				for k, v := range s {
					if strings.HasPrefix(k, "tollgate_ratelimit_decisions_total") && strings.Contains(k, `result="degraded"`) {
						run.Degraded += v
					}
				}
			}
			if cnt > 0 {
				run.EstErrMean = round(sum/cnt, 4)
			}
			c.stop()
			raw[fmt.Sprintf("%s/%d", m.Name, rep)] = run
			m.Runs = append(m.Runs, run)
			log.Printf("E8 %s rep=%d accepted=%d 429=%d failed=%d spend=$%.2f overspend=%.2f%% est_err=%.3f expired=%.0f leaked=%d degraded=%.0f",
				m.Name, rep+1, run.Accepted, run.Rejected, run.Failed, run.SpendUSD, run.OverspendPct, run.EstErrMean, run.Expired, run.LeakedHolds, run.Degraded)
			time.Sleep(time.Second)
		}
		_, m.Median = median(m.Runs, func(r e8Run) float64 { return r.OverspendPct })
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"modes": modes})
	res.Raw = mustJSON(raw)
	return res, nil
}
