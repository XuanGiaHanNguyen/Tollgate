package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/cost"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
)

// fakeLLM streams pieces after ttft, then usage and [DONE].
type fakeLLM struct {
	srv       *httptest.Server
	ttft      atomic.Int64 // nanoseconds
	gap       time.Duration
	pieces    []string
	calls     atomic.Int64
	cancelled atomic.Int64
	omitUsage bool
}

func newFakeLLM(t *testing.T, pieces ...string) *fakeLLM {
	f := &fakeLLM{pieces: pieces, gap: time.Millisecond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(time.Duration(f.ttft.Load())):
		case <-r.Context().Done():
			f.cancelled.Add(1)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, p := range f.pieces {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": p}}}})
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-time.After(f.gap):
			case <-r.Context().Done():
				f.cancelled.Add(1)
				return
			}
		}
		if !f.omitUsage {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":%d}}\n\n", len(f.pieces))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const aiRules = `
route "/v1/chat/*" {
  require auth
  providers ["provider-a", "provider-b"]
  hedge after p50 max 50%
  block_patterns "kid" action redact
  budget 1.00 usd per day by user.id
}
route "/v1/blocked/*" {
  require auth
  providers ["provider-a"]
  block_patterns "kid" action block
}
`

func aiGateway(t *testing.T, a, b *fakeLLM) (*Gateway, *httptest.Server, *metrics.Gateway) {
	t.Helper()
	y := fmt.Sprintf(`
upstreams:
  - {name: provider-a, targets: [{url: %s}], breaker: {disabled: true}}
  - {name: provider-b, targets: [{url: %s}], breaker: {disabled: true}}
routes:
  - {prefix: /v1/, upstream: provider-a, type: ai, timeouts: {total: 10s}}
auth:
  api_keys: [{key: k, user_id: alice, tier: free}]
ai:
  reservation_ttl: 1s
  hedge_min_samples: 3
  pricing:
    "*": {input_per_1k_usd: 1.0, output_per_1k_usd: 2.0}
  pattern_sets:
    kid: ["home address", "secret code"]
`, a.srv.URL, b.srv.URL)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.NewGateway("n1")
	g, err := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Compile(aiRules, rules.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CheckRules(rs); err != nil {
		t.Fatal(err)
	}
	g.SetRules(rs)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv, m
}

func chat(t *testing.T, url string, stream bool, maxTokens int) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"m","max_tokens":%d,"stream":%v,"messages":[{"role":"user","content":"hello"}]}`, maxTokens, stream)
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !stream || resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		var c struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
			Error any `json:"error"`
		}
		if json.Unmarshal([]byte(line), &c) == nil {
			if c.Error != nil {
				text.WriteString("[ERROR]")
			}
			if len(c.Choices) > 0 {
				text.WriteString(c.Choices[0].Delta.Content)
			}
		}
	}
	return resp.StatusCode, text.String()
}

func balance(g *Gateway) int64 {
	b, _ := g.policy.limiter.Budgets().Balance(cost.Key{BudgetID: 0, Value: "alice"})
	return b
}

func TestAIStreamFilterAndSettle(t *testing.T) {
	a := newFakeLLM(t, "Hi", ", my ho", "me addr", "ess is", " 42")
	b := newFakeLLM(t, "unused")
	g, srv, m := aiGateway(t, a, b)

	code, text := chat(t, srv.URL+"/v1/chat/completions", true, 100)
	if code != 200 || text != "Hi, my ************ is 42" {
		t.Fatalf("code=%d text=%q", code, text)
	}
	// Real cost = 100 prompt * $1/1K + 5 completion * $2/1K = 0.1 + 0.01 = $0.11
	if bal := balance(g); bal != 1_000_000-110_000 {
		t.Fatalf("balance after settle = %d", bal)
	}
	out := scrape(t, m)
	for _, w := range []string{`tollgate_filter_matches_total{action="redact",pattern_set="kid"} 1`,
		`tollgate_cost_settled_usd_total{model="m"} 0.11`, `tollgate_time_to_first_token_seconds_count{model="m",provider="provider-a"} 1`} {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q", w)
		}
	}
	// Non-streaming client gets one JSON body, filtered too.
	code, body := chat(t, srv.URL+"/v1/chat/completions", false, 100)
	if code != 200 || !strings.Contains(body, `"content":"Hi, my ************ is 42"`) {
		t.Fatalf("non-stream: %d %s", code, body)
	}
}

func TestAIBudgetExhausted(t *testing.T) {
	a := newFakeLLM(t, "x")
	g, srv, _ := aiGateway(t, a, newFakeLLM(t, "y"))
	// Estimate: 9 prompt tokens + 600 max_tokens -> ~$1.21 > $1.00 budget.
	if code, _ := chat(t, srv.URL+"/v1/chat/completions", true, 600); code != 429 {
		t.Fatalf("code = %d, want 429", code)
	}
	if a.calls.Load() != 0 || balance(g) != 1_000_000 {
		t.Fatal("provider called or budget touched despite 429")
	}
}

func TestAIHedgeWinsAndCancelsPrimary(t *testing.T) {
	a := newFakeLLM(t, "from-a")
	b := newFakeLLM(t, "from-b")
	_, srv, m := aiGateway(t, a, b)
	for i := 0; i < 4; i++ { // warm the TTFT window: provider-a is fast
		chat(t, srv.URL+"/v1/chat/completions", true, 10)
	}
	a.ttft.Store(int64(2 * time.Second)) // provider-a turns slow
	start := time.Now()
	code, text := chat(t, srv.URL+"/v1/chat/completions", true, 10)
	if code != 200 || text != "from-b" {
		t.Fatalf("code=%d text=%q", code, text)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("hedge did not cut latency: %v", el)
	}
	deadline := time.Now().Add(time.Second)
	for a.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.cancelled.Load() == 0 {
		t.Fatal("losing primary was not cancelled")
	}
	out := scrape(t, m)
	for _, w := range []string{`tollgate_hedges_total{outcome="won",provider="provider-b"} 1`,
		`tollgate_hedges_total{outcome="cancelled",provider="provider-a"} 1`} {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q", w)
		}
	}
}

func TestAIBlockEndsStream(t *testing.T) {
	a := newFakeLLM(t, "safe text ", "secret ", "code", " more")
	_, srv, _ := aiGateway(t, a, newFakeLLM(t, "y"))
	_, text := chat(t, srv.URL+"/v1/blocked/x", true, 10)
	if strings.Contains(text, "secret") || strings.Contains(text, "more") || !strings.Contains(text, "[ERROR]") {
		t.Fatalf("text = %q", text)
	}
}

// A client that disconnects mid-stream: the upstream is cancelled and the
// partial cost is settled; nothing stays held.
func TestAIClientDisconnectSettles(t *testing.T) {
	pieces := make([]string, 200)
	for i := range pieces {
		pieces[i] = " tok"
	}
	a := newFakeLLM(t, pieces...)
	a.gap = 5 * time.Millisecond
	a.omitUsage = true
	g, srv, _ := aiGateway(t, a, newFakeLLM(t, "y"))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","max_tokens":200,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	resp.Body.Read(buf) // some tokens arrive
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for g.policy.limiter.Budgets().Stats().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st := g.policy.limiter.Budgets().Stats()
	if st.Active != 0 || st.Settled != 1 || st.Expired != 0 {
		t.Fatalf("hold not settled on disconnect: %+v", st)
	}
	if a.cancelled.Load() == 0 {
		t.Fatal("upstream not cancelled")
	}
	// Partial cost was charged (some tokens counted), not the full estimate.
	if bal := balance(g); bal >= 1_000_000 || bal <= 1_000_000-430_000 {
		t.Fatalf("balance %d: expected a partial charge", bal)
	}
}
