package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/ai/filter"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ai/hedge"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ai/providers"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ai/tokens"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/lb"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// defaultMaxTokens is assumed for the estimate when a request sets none.
const defaultMaxTokens = 1024

// aiState is shared by every AI route.
type aiState struct {
	prices  map[string]tokens.Price
	defPx   tokens.Price
	sets    map[string]*filter.Set
	ttl     time.Duration
	tracker *hedge.Tracker
	clients map[string]*http.Client // per upstream
	budgets sync.Map                // hedge budget per rule route: string -> *hedge.Budget
}

func newAIState(cfg *config.Config) (*aiState, error) {
	a := &aiState{prices: map[string]tokens.Price{}, sets: map[string]*filter.Set{}, ttl: cfg.AI.ReservationTTL,
		tracker: hedge.NewTracker(256, cfg.AI.HedgeMinSample), clients: map[string]*http.Client{}}
	for model, p := range cfg.AI.Pricing {
		px := tokens.Price{InputPer1K: tokens.USDToMicro(p.InputPer1K), OutputPer1K: tokens.USDToMicro(p.OutputPer1K)}
		if model == "*" {
			a.defPx = px
		} else {
			a.prices[model] = px
		}
	}
	for name, phrases := range cfg.AI.PatternSets {
		s, err := filter.Compile(name, phrases)
		if err != nil {
			return nil, err
		}
		a.sets[name] = s
	}
	for _, u := range cfg.Upstreams {
		// Long header timeout: a model's time to first token is the header wait.
		a.clients[u.Name] = &http.Client{Transport: newHTTPTransport(cfg.Transport, config.Timeouts{Connect: 2 * time.Second, Header: 60 * time.Second})}
	}
	return a, nil
}

func (a *aiState) price(model string) tokens.Price {
	if p, ok := a.prices[model]; ok {
		return p
	}
	return a.defPx
}

func (a *aiState) hedgeBudget(route string, pct float64) *hedge.Budget {
	key := fmt.Sprintf("%s|%g", route, pct)
	if b, ok := a.budgets.Load(key); ok {
		return b.(*hedge.Budget)
	}
	b, _ := a.budgets.LoadOrStore(key, hedge.NewBudget(pct/100, 1))
	return b.(*hedge.Budget)
}

// CheckRules reports rule references the gateway cannot serve: unknown
// providers (upstreams) or pattern sets. Callers reject such a reload.
func (g *Gateway) CheckRules(rs *rules.RuleSet) error {
	var errs []error
	for _, r := range rs.Routes {
		for _, p := range r.AI.Providers {
			if g.upstreams[p] == nil {
				errs = append(errs, fmt.Errorf("route %q: unknown provider %q (no upstream with that name)", r.Pattern, p))
			}
		}
		if s := r.AI.PatternSet; s != "" && g.ai.sets[s] == nil {
			errs = append(errs, fmt.Errorf("route %q: unknown pattern set %q", r.Pattern, s))
		}
	}
	return errors.Join(errs...)
}

// aiResult is a provider stream that has produced its first token.
type aiResult struct {
	stream   *providers.Stream
	first    []providers.Chunk // events read up to and including the first token
	ttft     time.Duration
	provider string
	target   *lb.Target
	once     sync.Once
}

func (r *aiResult) Close() error {
	r.once.Do(func() { r.stream.Close(); r.target.Release() })
	return nil
}

// aiAttempt opens a stream on one provider and waits for its first token.
func (g *Gateway) aiAttempt(up *Upstream, path string, body []byte, hdr http.Header) hedge.Attempt {
	return func(ctx context.Context) (hedge.Result, error) {
		start := time.Now()
		t := up.Balancer.Pick(nil)
		if t == nil {
			return nil, ErrNoHealthyBackend
		}
		if t.Breaker != nil && !t.Breaker.Allow() {
			return nil, ErrCircuitOpen
		}
		t.Acquire()
		fail := func(err error) (hedge.Result, error) {
			t.Release()
			if t.Breaker != nil {
				if ctx.Err() != nil {
					t.Breaker.Ignore() // cancelled (lost a hedge race or client left)
				} else {
					t.Breaker.Record(false)
					t.ObserveResult(false)
				}
			}
			return nil, err
		}
		st, err := providers.Open(ctx, g.ai.clients[up.Name], up.Name, t.URL.String()+path, body, hdr)
		if err != nil {
			return fail(err)
		}
		var first []providers.Chunk
		for {
			c, err := st.Next()
			if err != nil {
				st.Close()
				return fail(err)
			}
			first = append(first, c)
			if c.Content != "" || c.Done {
				break
			}
		}
		if t.Breaker != nil {
			t.Breaker.Record(true)
		}
		t.ObserveResult(true)
		return &aiResult{stream: st, first: first, ttft: time.Since(start), provider: up.Name, target: t}, nil
	}
}

func writeAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "tollgate"}})
}

// serveAI handles a model call: policy with model fields, budget holds,
// (hedged) provider call, streamed and filtered output, settlement.
func (g *Gateway) serveAI(w http.ResponseWriter, r *http.Request, route *Route) {
	ctx, cancel := context.WithTimeout(r.Context(), route.cfg.Timeouts.Total)
	defer cancel()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReplayBody+1))
	if err != nil || len(body) > maxReplayBody {
		writeAIError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var cr tokens.ChatRequest
	if err := json.Unmarshal(body, &cr); err != nil || cr.Model == "" {
		writeAIError(w, http.StatusBadRequest, "expected an OpenAI-style chat request with a model")
		return
	}
	status, pol, _ := g.evalPolicy(r, &env.Request{Model: cr.Model, MaxTokens: cr.MaxTokens, Body: string(body)})
	if status != 0 {
		writePolicyStatus(w, status)
		return
	}

	// Reserve the estimated cost on every applicable budget.
	price := g.ai.price(cr.Model)
	promptEst := cr.PromptTokens()
	maxOut := cr.MaxTokens
	if maxOut <= 0 {
		maxOut = defaultMaxTokens
	}
	estimate := price.Cost(promptEst, maxOut)
	var holds []ratelimit.Hold
	for i := 0; i < pol.d.NBudgets; i++ {
		h, ok := g.policy.limiter.Reserve(ctx, pol.d.Budgets[i], estimate, g.ai.ttl)
		if !ok {
			for _, held := range holds {
				g.policy.limiter.Settle(held, 0) // release the holds already taken
			}
			g.countPolicy(metrics.PolicyLimited)
			writeAIError(w, http.StatusTooManyRequests, "budget exhausted")
			return
		}
		holds = append(holds, h)
		if g.metrics != nil {
			g.metrics.CostReserved(cr.Model, estimate)
		}
	}
	settled := false
	settle := func(actual int64) {
		if settled {
			return
		}
		settled = true
		for _, h := range holds {
			g.policy.limiter.Settle(h, actual)
		}
		if g.metrics != nil && len(holds) > 0 {
			g.metrics.CostSettled(cr.Model, estimate, actual)
		}
	}

	// Providers, in order.
	var settings rules.AISettings
	if pol.route != nil {
		settings = pol.route.AI
	}
	names := settings.Providers
	if len(names) == 0 {
		names = []string{route.upstream.Name}
	}
	ups := make([]*Upstream, 0, len(names))
	for _, n := range names {
		if up := g.upstreams[n]; up != nil {
			ups = append(ups, up)
		}
	}
	if len(ups) == 0 {
		settle(0)
		writeAIError(w, http.StatusBadGateway, "no provider configured")
		return
	}
	hdr := http.Header{}
	for k, v := range r.Header {
		switch k {
		case "Content-Length", "Connection", "Accept-Encoding", "Authorization":
		default:
			hdr[k] = v
		}
	}
	attempts := make([]hedge.Attempt, len(ups))
	for i, up := range ups {
		attempts[i] = g.aiAttempt(up, r.URL.Path, body, hdr)
	}
	after, allow := time.Duration(0), func() bool { return false }
	trackKey := ups[0].Name + "|" + cr.Model
	if settings.HedgeQuantile > 0 && len(ups) > 1 {
		hb := g.ai.hedgeBudget(pol.route.Pattern, settings.HedgeMaxPct)
		hb.Deposit()
		if q, ok := g.ai.tracker.Quantile(trackKey, settings.HedgeQuantile); ok {
			after, allow = q, hb.Withdraw
		}
	}
	raceStart := time.Now()
	res, done, out, err := hedge.Race(ctx, attempts, after, allow)
	if err != nil {
		// Providers bill prompts they received; we can't know, so charge
		// nothing and let the holds go.
		settle(0)
		if ctx.Err() != nil && r.Context().Err() != nil {
			return // client left
		}
		writeAIError(w, http.StatusBadGateway, "provider error: "+err.Error())
		return
	}
	defer done()
	ar := res.(*aiResult)
	defer ar.Close()

	// Latency tracking and hedge accounting.
	g.ai.tracker.Observe(ar.provider+"|"+cr.Model, ar.ttft)
	if out.Winner != 0 && out.Hedged {
		// The primary was still waiting: record a lower bound for it so its
		// p95 doesn't drift down from only seeing winners.
		g.ai.tracker.Observe(trackKey, time.Since(raceStart))
	}
	var lossCost int64
	for _, l := range out.LoserCalled {
		lossCost += price.Cost(promptEst, 0) // the cancelled call was billed for its prompt
		if g.metrics != nil {
			g.metrics.Hedge(ups[l].Name, "cancelled")
		}
	}
	if g.metrics != nil {
		g.metrics.TTFT(ar.provider, cr.Model, ar.ttft.Seconds())
		if out.Hedged {
			outcome := "lost"
			if out.Winner == 1 {
				outcome = "won"
			}
			g.metrics.Hedge(ups[1].Name, outcome)
		}
	}

	// Stream (or collect) the response through the safety filter.
	var fs *filter.Stream
	if settings.PatternSet != "" {
		act, _ := filter.ParseAction(settings.PatternAction)
		fs = g.ai.sets[settings.PatternSet].NewStream(act)
		if g.metrics != nil {
			set := settings.PatternSet
			fs.OnHoldback = func(d time.Duration) { g.metrics.FilterHoldback(set, d.Seconds()) }
		}
	}
	rc := http.NewResponseController(w)
	if cr.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Provider", ar.provider)
		w.WriteHeader(http.StatusOK)
	}
	var origText, collected strings.Builder
	var usage *providers.Usage
	blocked, writeErr := false, false
	write := func(b []byte) {
		if writeErr || !cr.Stream {
			return
		}
		if _, err := w.Write(b); err != nil {
			writeErr = true
			return
		}
		rc.Flush()
	}
	emit := func(content string) {
		if content == "" {
			return
		}
		collected.WriteString(content)
		write(providers.ContentEvent(cr.Model, content))
	}
	handle := func(c providers.Chunk) {
		switch {
		case c.Content != "":
			origText.WriteString(c.Content)
			if fs == nil {
				collected.WriteString(c.Content)
				write(append(append([]byte("data: "), c.Raw...), '\n', '\n'))
				return
			}
			text, err := fs.Write(c.Content)
			if errors.Is(err, filter.ErrBlocked) {
				blocked = true
				return
			}
			emit(text)
		case c.Usage != nil:
			usage = c.Usage
		case c.Done:
		default:
			write(append(append([]byte("data: "), c.Raw...), '\n', '\n'))
		}
	}
	finished := false
	for _, c := range ar.first {
		handle(c)
		finished = finished || c.Done
	}
	for !finished && !blocked && !writeErr {
		c, err := ar.stream.Next()
		if err != nil {
			break // stream ended (EOF) or was cut
		}
		handle(c)
		finished = c.Done
	}
	if fs != nil && !blocked {
		tail, err := fs.Close()
		if errors.Is(err, filter.ErrBlocked) {
			blocked = true
		} else {
			emit(tail)
		}
	}
	if fs != nil && g.metrics != nil && fs.Matches() > 0 {
		g.metrics.FilterMatches(settings.PatternSet, settings.PatternAction, fs.Matches())
	}

	// Settle the real cost: provider usage if reported, else counted tokens.
	in, outTok := promptEst, tokens.Estimate(origText.String())
	if usage != nil {
		in, outTok = usage.PromptTokens, usage.CompletionTokens
	}
	settle(price.Cost(in, outTok) + lossCost)

	if cr.Stream {
		if blocked {
			write(providers.ErrorEvent("response blocked by safety filter"))
		}
		if usage != nil {
			b, _ := json.Marshal(map[string]any{"object": "chat.completion.chunk", "model": cr.Model, "choices": []any{}, "usage": usage})
			write(append(append([]byte("data: "), b...), '\n', '\n'))
		}
		write([]byte("data: [DONE]\n\n"))
		return
	}
	if blocked {
		writeAIError(w, http.StatusUnprocessableEntity, "response blocked by safety filter")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Provider", ar.provider)
	json.NewEncoder(w).Encode(map[string]any{
		"object": "chat.completion", "model": cr.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": collected.String()}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": outTok, "total_tokens": in + outTok},
	})
}
