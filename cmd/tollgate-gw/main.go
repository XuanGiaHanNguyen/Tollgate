// Command tollgate-gw is the Tollgate gateway (data plane).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/proxy"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/peer"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
)

func main() {
	cfgPath := flag.String("config", "tollgate.yaml", "path to gateway config")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	clusterSelf := flag.String("cluster-self", "", "override cluster.self, so one config file serves every gateway")
	node := flag.String("node", "", "node name used in metric labels (default: cluster.self, $TOLLGATE_NODE or hostname)")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	if err := run(*cfgPath, *node, *clusterSelf, log); err != nil {
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

func run(cfgPath, node, clusterSelf string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if clusterSelf != "" {
		if cfg.Cluster == nil {
			return fmt.Errorf("-cluster-self given but the config has no cluster block")
		}
		cfg.Cluster.Self = clusterSelf
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	if node == "" {
		node = defaultNode()
		if cfg.Cluster != nil {
			node = cfg.Cluster.Self
		}
	}
	m := metrics.NewGateway(node)

	var lim *ratelimit.Limiter
	var peerSrv *grpc.Server
	if cl := cfg.Cluster; cl != nil {
		members := make([]ratelimit.Member, len(cl.Members))
		for i, mb := range cl.Members {
			members[i] = ratelimit.Member{Name: mb.Name, Addr: mb.Addr}
		}
		lim, err = ratelimit.New(ratelimit.Config{
			Self: cl.Self, Members: members, VNodes: cl.VNodes,
			Peer:       peer.Config{MaxBatch: cl.Batch.Max, FlushInterval: cl.Batch.Flush, Timeout: cl.PeerTimeout},
			Hooks:      peer.Hooks{RPC: m.PeerRPC},
			OnDecision: m.RateLimitDecision,
		})
		if err != nil {
			return err
		}
		defer lim.Close()
		ln, err := net.Listen("tcp", cl.Listen)
		if err != nil {
			return err
		}
		peerSrv = grpc.NewServer()
		tollgatev1.RegisterRateLimitServiceServer(peerSrv, &peer.Server{Store: lim.Local()})
		go func() {
			if err := peerSrv.Serve(ln); err != nil {
				log.Error("peer gRPC server stopped", "err", err)
			}
		}()
		log.Info("cluster rate limiting", "self", cl.Self, "members", len(members), "listen", cl.Listen,
			"vnodes", cl.VNodes, "batch_max", cl.Batch.Max, "batch_flush", cl.Batch.Flush.String())
	}

	gw, err := proxy.New(cfg, proxy.Options{Logger: log, Metrics: m, Limiter: lim})
	if err != nil {
		return err
	}
	gw.Start()
	defer gw.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Rules.File != "" {
		path := cfg.Rules.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(cfgPath), path)
		}
		err := rules.Watch(ctx, path, cfg.Rules.ReloadInterval, rules.Options{Budget: cfg.Rules.Budget},
			func(rs *rules.RuleSet, err error) {
				if err != nil {
					log.Error("rules not loaded; keeping the previous version", "file", path, "err", err.Error())
					return
				}
				v := gw.SetRules(rs)
				log.Info("rules loaded", "file", path, "version", v, "routes", len(rs.Routes), "engine", cfg.Rules.Engine)
				for _, w := range rs.Warnings {
					log.Warn("rule warning", "file", path, "msg", w.String())
				}
			})
		if err != nil {
			return fmt.Errorf("rules: %w", err)
		}
	}

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
	admin.HandleFunc("GET /admin/rules", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": gw.RulesVersion(), "file": cfg.Rules.File, "engine": cfg.Rules.Engine})
	})
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
	if peerSrv != nil {
		peerSrv.GracefulStop()
	}
	log.Info("stopped")
	if proxyErr != nil {
		return proxyErr
	}
	return adminErr
}
