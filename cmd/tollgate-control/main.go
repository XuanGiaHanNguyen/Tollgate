// Command tollgate-control is a control-plane node: a member of the
// 3-node Raft cluster that stores gateway config and streams it to
// gateways.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/rollout"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
)

// parseMembers parses "1=host:port,2=host:port".
func parseMembers(s string) (map[uint64]string, error) {
	out := map[uint64]string{}
	for _, part := range strings.Split(s, ",") {
		id, addr, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.ParseUint(id, 10, 64)
		if !ok || err != nil || n == 0 || addr == "" {
			return nil, fmt.Errorf("bad member %q (want id=host:port)", part)
		}
		out[n] = addr
	}
	return out, nil
}

func main() {
	id := flag.Uint64("id", 0, "this node's ID (1-based)")
	dataDir := flag.String("data", "", "directory for the Raft WAL and snapshots")
	raftListen := flag.String("raft-listen", ":7100", "address for Raft traffic from peers")
	apiListen := flag.String("api-listen", ":7200", "address for the ControlService client API")
	adminListen := flag.String("admin-listen", ":7300", "address for /metrics and /healthz")
	raftPeers := flag.String("raft-peers", "", "all members' Raft addresses: 1=host:7100,2=...,3=...")
	apiPeers := flag.String("api-peers", "", "all members' client API addresses (for leader redirects)")
	tick := flag.Duration("tick", 50*time.Millisecond, "Raft tick; election timeout is 10-20 ticks")
	gateways := flag.String("gateways", "", "gateway admin URLs for canary comparison: gw-1=http://host:9090,...")
	pol := rollout.DefaultPolicy()
	flag.DurationVar(&pol.Window, "rollout-window", pol.Window, "healthy canary time before promotion")
	flag.DurationVar(&pol.MaxWait, "rollout-max-wait", pol.MaxWait, "roll back if the canary lacks traffic by then")
	flag.Float64Var(&pol.MinRequests, "rollout-min-requests", pol.MinRequests, "requests needed on each side before a verdict")
	flag.Float64Var(&pol.MaxErrDelta, "rollout-max-error-delta", pol.MaxErrDelta, "allowed canary error-rate excess (0.05 = 5 points)")
	flag.Float64Var(&pol.MaxP99Ratio, "rollout-max-p99-ratio", pol.MaxP99Ratio, "allowed canary p99 / baseline p99")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	gws := map[string]string{}
	if *gateways != "" {
		for _, part := range strings.Split(*gateways, ",") {
			name, url, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				log.Error("bad -gateways entry", "entry", part)
				os.Exit(2)
			}
			gws[name] = url
		}
	}
	if err := run(*id, *dataDir, *raftListen, *apiListen, *adminListen, *raftPeers, *apiPeers, *tick, gws, pol, log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(id uint64, dataDir, raftListen, apiListen, adminListen, raftPeers, apiPeers string, tick time.Duration,
	gateways map[string]string, pol rollout.Policy, log *slog.Logger) error {
	if id == 0 || dataDir == "" {
		return fmt.Errorf("-id and -data are required")
	}
	raftAddrs, err := parseMembers(raftPeers)
	if err != nil {
		return err
	}
	apiAddrs, err := parseMembers(apiPeers)
	if err != nil {
		return err
	}
	if _, ok := raftAddrs[id]; !ok {
		return fmt.Errorf("-id %d is not in -raft-peers", id)
	}
	var peers []uint64
	for p := range raftAddrs {
		peers = append(peers, p)
	}
	m := metrics.NewControl(fmt.Sprint(id))
	tr, err := control.NewGRPCTransport(id, raftAddrs)
	if err != nil {
		return err
	}
	defer tr.Close()
	srv, err := control.NewServer(control.Config{ID: id, Peers: peers, Dir: dataDir, TickInterval: tick, Log: log,
		Hooks: control.Hooks{Term: m.Term, CommitLatency: m.CommitLatency}}, tr)
	if err != nil {
		return err
	}
	defer srv.Stop()

	raftGS := grpc.NewServer()
	tollgatev1.RegisterRaftServiceServer(raftGS, &control.RaftService{S: srv})
	apiGS := grpc.NewServer()
	tollgatev1.RegisterControlServiceServer(apiGS, &control.API{S: srv, ClientAddrs: apiAddrs})
	rl, err := net.Listen("tcp", raftListen)
	if err != nil {
		return err
	}
	al, err := net.Listen("tcp", apiListen)
	if err != nil {
		return err
	}
	go raftGS.Serve(rl)
	go apiGS.Serve(al)

	admin := http.NewServeMux()
	admin.Handle("GET /metrics", m.Handler())
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	admin.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		st, ver := srv.Status()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": st.ID, "state": st.State.String(), "term": st.Term, "leader": st.Lead,
			"commit": st.Commit, "applied": st.Applied, "version": ver, "last_index": st.LastIndex, "snapshot_index": st.SnapIndex})
	})
	hs := &http.Server{Addr: adminListen, Handler: admin, ReadHeaderTimeout: 5 * time.Second}
	go hs.ListenAndServe()

	log.Info("tollgate-control started", "id", id, "raft", raftListen, "api", apiListen, "admin", adminListen, "members", len(peers))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(gateways) > 0 {
		ctl := &rollout.Controller{Store: srv, Gateways: gateways, Policy: pol, Log: log, OnDecision: m.RolloutDecision}
		go ctl.Run(ctx)
		log.Info("rollout controller enabled", "gateways", len(gateways), "window", pol.Window.String())
	}
	<-ctx.Done()
	log.Info("shutting down")
	apiGS.GracefulStop()
	raftGS.Stop()
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	return nil
}
