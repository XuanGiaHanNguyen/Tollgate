package control

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
)

type tnode struct {
	id       uint64
	srv      *Server
	tr       *GRPCTransport
	gs       *grpc.Server
	api      *grpc.Server
	raftAddr string
	apiAddr  string
	dir      string
}

type tcluster struct {
	t        *testing.T
	nodes    []*tnode
	raft     map[uint64]string
	api      map[uint64]string
	snapshot uint64
}

func newCluster(t *testing.T, n int, snapshotEvery uint64) *tcluster {
	t.Helper()
	c := &tcluster{t: t, raft: map[uint64]string{}, api: map[uint64]string{}, snapshot: snapshotEvery}
	base := t.TempDir()
	for i := 1; i <= n; i++ {
		rl, _ := net.Listen("tcp", "127.0.0.1:0")
		al, _ := net.Listen("tcp", "127.0.0.1:0")
		rl.Close()
		al.Close()
		c.raft[uint64(i)], c.api[uint64(i)] = rl.Addr().String(), al.Addr().String()
		c.nodes = append(c.nodes, &tnode{id: uint64(i), raftAddr: rl.Addr().String(), apiAddr: al.Addr().String(),
			dir: filepath.Join(base, fmt.Sprintf("n%d", i))})
	}
	for _, nd := range c.nodes {
		c.start(nd)
	}
	t.Cleanup(func() {
		for _, nd := range c.nodes {
			c.stop(nd)
		}
	})
	return c
}

func (c *tcluster) start(nd *tnode) {
	c.t.Helper()
	var peers []uint64
	for id := range c.raft {
		peers = append(peers, id)
	}
	tr, err := NewGRPCTransport(nd.id, c.raft)
	if err != nil {
		c.t.Fatal(err)
	}
	srv, err := NewServer(Config{ID: nd.id, Peers: peers, Dir: nd.dir, TickInterval: 10 * time.Millisecond,
		SnapshotEvery: c.snapshot, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, tr)
	if err != nil {
		c.t.Fatal(err)
	}
	gs := grpc.NewServer()
	tollgatev1.RegisterRaftServiceServer(gs, &RaftService{S: srv})
	tollgatev1.RegisterControlServiceServer(gs, &API{S: srv, ClientAddrs: c.api})
	// One listener serves both services here; tests use apiAddr for both.
	ln, err := net.Listen("tcp", nd.raftAddr)
	if err != nil {
		c.t.Fatal(err)
	}
	go gs.Serve(ln)
	gs2 := grpc.NewServer()
	tollgatev1.RegisterControlServiceServer(gs2, &API{S: srv, ClientAddrs: c.api})
	ln2, err := net.Listen("tcp", nd.apiAddr)
	if err != nil {
		c.t.Fatal(err)
	}
	go gs2.Serve(ln2)
	nd.srv, nd.tr, nd.gs, nd.api = srv, tr, gs, gs2
}

func (c *tcluster) stop(nd *tnode) {
	if nd.srv == nil {
		return
	}
	nd.gs.Stop()
	nd.api.Stop()
	nd.tr.Close()
	nd.srv.Stop()
	nd.srv = nil
}

func (c *tcluster) leader() *tnode {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, nd := range c.nodes {
			if nd.srv == nil {
				continue
			}
			if st, _ := nd.srv.Status(); st.State.String() == "leader" {
				return nd
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal("no leader elected")
	return nil
}

func (c *tcluster) endpoints() []string {
	var eps []string
	for _, nd := range c.nodes {
		eps = append(eps, nd.apiAddr)
	}
	return eps
}

func ctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPutGetRedirectWatch(t *testing.T) {
	c := newCluster(t, 3, 1000)
	ld := c.leader()
	cl := client.New(c.endpoints())
	defer cl.Close()

	v, err := cl.Put(ctx(t), "members", []byte("gw-1,gw-2"))
	if err != nil || v == 0 {
		t.Fatalf("put: v=%d err=%v", v, err)
	}
	r, err := cl.Get(ctx(t), "members")
	if err != nil || !r.Found || string(r.Value) != "gw-1,gw-2" || r.Version != v {
		t.Fatalf("get: %+v %v", r, err)
	}
	// A follower redirects to the leader.
	var follower *tnode
	for _, nd := range c.nodes {
		if nd != ld {
			follower = nd
		}
	}
	one := client.New([]string{follower.apiAddr})
	defer one.Close()
	st, _ := one.Status(ctx(t), follower.apiAddr)
	if st.State != "follower" || st.LeaderAddr != ld.apiAddr {
		t.Fatalf("follower status %+v", st)
	}

	// Invalid rules are refused before they reach the log.
	if _, err := cl.Put(ctx(t), "rules", []byte(`route "/a" { deny }`)); err == nil {
		t.Fatal("invalid rules accepted")
	}
	if _, err := cl.Put(ctx(t), "rules", []byte(`route "/a" { deny 403 }`)); err != nil {
		t.Fatal(err)
	}

	// Watch from 0 on a follower sees both writes; then a new one.
	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan *tollgatev1.WatchResponse, 16)
	go one.Watch(wctx, 0, func(r *tollgatev1.WatchResponse) { got <- r })
	seen := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for !(seen["members"] && seen["rules"]) {
		select {
		case r := <-got:
			for _, ch := range r.Changes {
				seen[ch.Key] = true
			}
		case <-timeout:
			t.Fatalf("watch saw %v", seen)
		}
	}
	cl.Put(ctx(t), "pricing", []byte("x"))
	select {
	case r := <-got:
		if len(r.Changes) != 1 || r.Changes[0].Key != "pricing" || r.Changes[0].CommitTsUnixNano == 0 {
			t.Fatalf("watch update %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch missed a live update")
	}
}

// Killing the leader loses no acknowledged write and a new leader takes
// writes; the restarted node recovers from its WAL and catches up.
func TestLeaderFailoverNoLoss(t *testing.T) {
	c := newCluster(t, 3, 1000)
	cl := client.New(c.endpoints())
	defer cl.Close()
	acked := map[string]bool{}
	for i := 0; i < 30; i++ {
		k := fmt.Sprintf("k%d", i)
		if _, err := cl.Put(ctx(t), k, []byte(k)); err != nil {
			t.Fatal(err)
		}
		acked[k] = true
	}
	old := c.leader()
	c.stop(old)
	start := time.Now()
	nl := c.leader()
	if nl == old {
		t.Fatal("old leader still leading")
	}
	if _, err := cl.Put(ctx(t), "after", []byte("1")); err != nil {
		t.Fatal(err)
	}
	t.Logf("failover: new leader %d accepting writes after %v", nl.id, time.Since(start))
	for k := range acked {
		r, err := cl.Get(ctx(t), k)
		if err != nil || !r.Found {
			t.Fatalf("acknowledged write %s lost: %+v %v", k, r, err)
		}
	}
	c.start(old) // restart from its WAL
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, v := old.srv.Status()
		_, lv := nl.srv.Status()
		if v == lv && v > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted node stuck at version %d (leader %d)", v, lv)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A node down while the log is compacted catches up via InstallSnapshot.
func TestLaggingFollowerGetsSnapshot(t *testing.T) {
	c := newCluster(t, 3, 20) // compact every 20 entries
	cl := client.New(c.endpoints())
	defer cl.Close()
	cl.Put(ctx(t), "warm", []byte("1"))
	ld := c.leader()
	var lag *tnode
	for _, nd := range c.nodes {
		if nd != ld {
			lag = nd
			break
		}
	}
	c.stop(lag)
	for i := 0; i < 100; i++ {
		if _, err := cl.Put(ctx(t), fmt.Sprintf("k%d", i%7), []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := ld.srv.Status(); st.SnapIndex == 0 {
		t.Fatal("leader never compacted")
	}
	c.start(lag)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, v := lag.srv.Status()
		_, lv := ld.srv.Status()
		if v == lv && st.SnapIndex > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lagging node did not catch up: %+v version %d vs %d", st, v, lv)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
