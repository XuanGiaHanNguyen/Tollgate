// Command tollgate-gw is the Tollgate gateway (data plane).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/proxy"
)

func main() {
	cfgPath := flag.String("config", "tollgate.yaml", "path to gateway config")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	node := flag.String("node", defaultNode(), "node name used in metric labels")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	if err := run(*cfgPath, *node, log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func defaultNode() string {
	if n := os.Getenv("TOLLGATE_NODE"); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "gw"
}

func run(cfgPath, node string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	m := metrics.NewGateway(node)
	m.SetConfigVersion(1) // static file config; versions come from the control plane in M6
	gw, err := proxy.New(cfg, proxy.Options{Logger: log, Metrics: m})
	if err != nil {
		return err
	}
	gw.Start()
	defer gw.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var draining atomic.Bool
	admin := http.NewServeMux()
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	admin.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})

	admin.Handle("GET /metrics", m.Handler())
	admin.HandleFunc("GET /admin/upstreams", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gw.Status())
	})

	proxyLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	adminLn, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		return err
	}
	log.Info("tollgate-gw started", "node", node, "listen", cfg.Listen, "admin", cfg.AdminListen,
		"routes", len(cfg.Routes), "upstreams", len(cfg.Upstreams))

	go func() {
		<-ctx.Done()
		draining.Store(true)
		log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	}()

	var wg sync.WaitGroup
	var proxyErr, adminErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv := &http.Server{Handler: gw, ReadHeaderTimeout: 10 * time.Second}
		proxyErr = proxy.Serve(ctx, srv, proxyLn, cfg.ShutdownTimeout)
	}()
	go func() {
		defer wg.Done()
		srv := &http.Server{Handler: admin, ReadHeaderTimeout: 5 * time.Second}
		adminErr = proxy.Serve(ctx, srv, adminLn, cfg.ShutdownTimeout)
	}()
	wg.Wait()
	log.Info("stopped")
	if proxyErr != nil {
		return proxyErr
	}
	return adminErr
}
