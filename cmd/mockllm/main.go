// Command mockllm is a fake OpenAI-compatible model provider for tests and
// benchmarks. It streams chat completions over SSE with a configurable
// time-to-first-token distribution (log-normal with a heavy tail), token
// rate and output length, reports usage, and keeps billing totals per user
// (the X-User-Id header) so experiments can measure true spend. Nothing in
// it calls a real model API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type settings struct {
	TTFTMedian time.Duration `json:"ttft_median"`
	TTFTSigma  float64       `json:"ttft_sigma"` // log-normal sigma
	TailProb   float64       `json:"tail_prob"`  // chance a request lands in the slow tail
	TailMult   float64       `json:"tail_mult"`  // tail TTFT multiplier
	TPS        float64       `json:"tokens_per_second"`
	FracMin    float64       `json:"output_frac_min"` // output tokens as a fraction of max_tokens
	FracMax    float64       `json:"output_frac_max"`
	OmitUsage  bool          `json:"omit_usage"`
	Inject     []string      `json:"inject"`
	InjectProb float64       `json:"inject_prob"`
}

type usage struct {
	Requests         int64 `json:"requests"`
	Completed        int64 `json:"completed"`
	Cancelled        int64 `json:"cancelled"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"` // generated, including partial streams
	Injected         int64 `json:"injected"`
}

type server struct {
	name string
	cur  atomic.Pointer[settings]
	mu   sync.Mutex
	tot  usage
	by   map[string]*usage
	rng  *rand.Rand
	rmu  sync.Mutex
	seq  atomic.Int64
}

func main() {
	addr := flag.String("addr", ":9200", "listen address")
	name := flag.String("name", "mockllm", "provider name reported in responses")
	s := settings{}
	flag.DurationVar(&s.TTFTMedian, "ttft-median", 200*time.Millisecond, "median time to first token")
	flag.Float64Var(&s.TTFTSigma, "ttft-sigma", 0.25, "log-normal sigma of time to first token")
	flag.Float64Var(&s.TailProb, "tail-prob", 0, "probability of a slow (tail) request")
	flag.Float64Var(&s.TailMult, "tail-mult", 10, "TTFT multiplier for tail requests")
	flag.Float64Var(&s.TPS, "tps", 100, "tokens per second after the first token")
	flag.Float64Var(&s.FracMin, "output-frac-min", 0.2, "min output length as a fraction of max_tokens")
	flag.Float64Var(&s.FracMax, "output-frac-max", 1.0, "max output length as a fraction of max_tokens")
	flag.BoolVar(&s.OmitUsage, "omit-usage", false, "do not report usage (gateway must count tokens)")
	inject := flag.String("inject", "", "comma-separated phrases to inject into output, split across chunks")
	flag.Float64Var(&s.InjectProb, "inject-prob", 0, "probability a response contains an injected phrase")
	seed := flag.Uint64("seed", 1, "random seed")
	flag.Parse()
	if *inject != "" {
		s.Inject = strings.Split(*inject, ",")
	}

	srv := &server{name: *name, by: map[string]*usage{}, rng: rand.New(rand.NewPCG(*seed, *seed^0xabcdef))}
	srv.cur.Store(&s)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /v1/chat/completions", srv.chat)
	mux.HandleFunc("GET /admin/usage", srv.getUsage)
	mux.HandleFunc("POST /admin/reset", srv.reset)
	mux.HandleFunc("GET /admin/config", srv.getConfig)
	mux.HandleFunc("POST /admin/config", srv.setConfig)

	hs := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("mockllm started", "addr", *addr, "name", *name)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func (s *server) rand(f func(r *rand.Rand) float64) float64 {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return f(s.rng)
}

type chatReq struct {
	Model         string `json:"model"`
	MaxTokens     int64  `json:"max_tokens"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// promptTokens counts words plus 4 per message: deliberately not the
// gateway's estimator, so estimates have real error.
func (r *chatReq) promptTokens() int64 {
	var n int64
	for _, m := range r.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) != nil {
			s = string(m.Content)
		}
		n += int64(len(strings.Fields(s))) + 4
	}
	return n
}

func (s *server) account(user string, f func(u *usage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.tot)
	u := s.by[user]
	if u == nil {
		u = &usage{}
		s.by[user] = u
	}
	f(u)
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	cfg := s.cur.Load()
	user := r.Header.Get("X-User-Id")
	if req.MaxTokens <= 0 {
		req.MaxTokens = 256
	}
	prompt := req.promptTokens()
	frac := cfg.FracMin + s.rand(func(r *rand.Rand) float64 { return r.Float64() })*(cfg.FracMax-cfg.FracMin)
	n := int64(math.Max(1, math.Round(frac*float64(req.MaxTokens))))
	ttft := float64(cfg.TTFTMedian) * math.Exp(cfg.TTFTSigma*s.rand(func(r *rand.Rand) float64 { return r.NormFloat64() }))
	if s.rand(func(r *rand.Rand) float64 { return r.Float64() }) < cfg.TailProb {
		ttft *= cfg.TailMult
	}
	var inject string
	if len(cfg.Inject) > 0 && s.rand(func(r *rand.Rand) float64 { return r.Float64() }) < cfg.InjectProb {
		inject = cfg.Inject[int(s.rand(func(r *rand.Rand) float64 { return float64(r.IntN(len(cfg.Inject))) }))]
	}
	s.account(user, func(u *usage) { u.Requests++; u.PromptTokens += prompt })

	if !sleepCtx(r.Context(), time.Duration(ttft)) {
		s.account(user, func(u *usage) { u.Cancelled++ }) // cancelled before the first token
		return
	}
	id := fmt.Sprintf("chatcmpl-%s-%d", s.name, s.seq.Add(1))
	gap := time.Duration(float64(time.Second) / cfg.TPS)
	pieces := outputPieces(n, inject)

	if !req.Stream {
		if !sleepCtx(r.Context(), gap*time.Duration(n)) {
			s.account(user, func(u *usage) { u.Cancelled++; u.CompletionTokens += n })
			return
		}
		s.account(user, func(u *usage) { u.Completed++; u.CompletionTokens += n; u.Injected += b2i(inject != "") })
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Provider", s.name)
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": strings.Join(pieces, "")}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": n, "total_tokens": prompt + n},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Provider", s.name)
	flusher, _ := w.(http.Flusher)
	send := func(v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	var sent int64
	for i, p := range pieces {
		if i > 0 && !sleepCtx(r.Context(), gap) {
			break
		}
		if !send(map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": p}}}}) {
			break
		}
		sent++
	}
	tokens := min(sent, n)
	if r.Context().Err() != nil || sent < int64(len(pieces)) {
		s.account(user, func(u *usage) { u.Cancelled++; u.CompletionTokens += tokens })
		return
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	if !cfg.OmitUsage && (req.StreamOptions == nil || req.StreamOptions.IncludeUsage) {
		send(map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model, "choices": []any{},
			"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": n, "total_tokens": prompt + n}})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	s.account(user, func(u *usage) { u.Completed++; u.CompletionTokens += n; u.Injected += b2i(inject != "") })
}

// outputPieces returns n token pieces; an injected phrase is spliced in as
// 3-byte fragments so it crosses chunk boundaries.
func outputPieces(n int64, inject string) []string {
	pieces := make([]string, 0, n+8)
	for i := int64(0); i < n; i++ {
		pieces = append(pieces, " tok"+strconv.FormatInt(i%97, 10))
	}
	if inject != "" {
		var frag []string
		s := " " + inject + " "
		for len(s) > 0 {
			k := min(3, len(s))
			frag = append(frag, s[:k])
			s = s[k:]
		}
		mid := len(pieces) / 2
		pieces = append(pieces[:mid], append(frag, pieces[mid:]...)...)
	}
	return pieces
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *server) getUsage(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"name": s.name, "total": s.tot, "by_user": s.by})
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.tot, s.by = usage{}, map[string]*usage{}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.cur.Load())
}

// setConfig replaces settings from a JSON body; omitted fields keep their values.
func (s *server) setConfig(w http.ResponseWriter, r *http.Request) {
	next := *s.cur.Load()
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.cur.Store(&next)
	s.getConfig(w, r)
}
