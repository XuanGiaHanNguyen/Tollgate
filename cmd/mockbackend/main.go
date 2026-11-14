// Command mockbackend is a test backend with configurable latency and error
// rate. Both can be changed at runtime via POST /admin/config, so benchmarks
// can inject failures mid-run (E7).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

type settings struct {
	Latency   time.Duration `json:"latency"`
	Jitter    time.Duration `json:"jitter"`
	ErrorRate float64       `json:"error_rate"`
}

type server struct {
	name     string
	cur      atomic.Pointer[settings]
	requests atomic.Int64
	errors   atomic.Int64
}

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	name := flag.String("name", hostname(), "name reported in responses")
	latency := flag.Duration("latency", 0, "base response latency")
	jitter := flag.Duration("jitter", 0, "uniform random extra latency in [0, jitter]")
	errRate := flag.Float64("error-rate", 0, "fraction of requests answered with 500")
	flag.Parse()

	s := &server{name: *name}
	s.cur.Store(&settings{Latency: *latency, Jitter: *jitter, ErrorRate: *errRate})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /admin/config", s.getConfig)
	mux.HandleFunc("POST /admin/config", s.setConfig)
	mux.HandleFunc("/", s.handle)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("mockbackend started", "addr", *addr, "name", *name)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	cfg := s.cur.Load()
	s.requests.Add(1)
	d := cfg.Latency
	if cfg.Jitter > 0 {
		d += time.Duration(rand.Int64N(int64(cfg.Jitter) + 1))
	}
	if d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Backend", s.name)
	if cfg.ErrorRate > 0 && rand.Float64() < cfg.ErrorRate {
		s.errors.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"backend":%q,"error":"injected"}`+"\n", s.name)
		return
	}
	fmt.Fprintf(w, `{"backend":%q,"path":%q}`+"\n", s.name, r.URL.Path)
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name": s.name, "settings": s.cur.Load(),
		"requests": s.requests.Load(), "errors": s.errors.Load(),
	})
}

// setConfig takes query params: latency=10ms, jitter=5ms, error_rate=0.5.
// Omitted params keep their current value.
func (s *server) setConfig(w http.ResponseWriter, r *http.Request) {
	next := *s.cur.Load()
	q := r.URL.Query()
	var err error
	if v := q.Get("latency"); v != "" {
		if next.Latency, err = time.ParseDuration(v); err != nil {
			http.Error(w, "bad latency", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("jitter"); v != "" {
		if next.Jitter, err = time.ParseDuration(v); err != nil {
			http.Error(w, "bad jitter", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("error_rate"); v != "" {
		if next.ErrorRate, err = strconv.ParseFloat(v, 64); err != nil || next.ErrorRate < 0 || next.ErrorRate > 1 {
			http.Error(w, "bad error_rate", http.StatusBadRequest)
			return
		}
	}
	s.cur.Store(&next)
	slog.Info("settings changed", "latency", next.Latency, "jitter", next.Jitter, "error_rate", next.ErrorRate)
	s.getConfig(w, r)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "mockbackend"
	}
	return h
}
