package ratelimit

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/peer"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

type node struct {
	name    string
	lim     *Limiter
	srv     *grpc.Server
	results sync.Map // result -> *atomic.Int64
	batches atomic.Int64
	checks  atomic.Int64
}

func (n *node) count(r string) int64 {
	v, ok := n.results.Load(r)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// cluster starts n limiters on loopback gRPC servers.
func cluster(t *testing.T, size int, pc peer.Config) []*node {
	t.Helper()
	var members []Member
	var lns []net.Listener
	for i := 0; i < size; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		members = append(members, Member{Name: fmt.Sprintf("gw-%d", i+1), Addr: ln.Addr().String()})
	}
	nodes := make([]*node, size)
	for i := range nodes {
		n := &node{name: members[i].Name}
		lim, err := New(Config{
			Self: n.name, Members: members, VNodes: 64, Peer: pc,
			Hooks: peer.Hooks{RPC: func(_ string, _ float64, batch int, err error) {
				if err == nil {
					n.batches.Add(1)
					n.checks.Add(int64(batch))
				}
			}},
			OnDecision: func(r string) {
				v, _ := n.results.LoadOrStore(r, new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		n.lim = lim
		n.srv = grpc.NewServer()
		tollgatev1.RegisterRateLimitServiceServer(n.srv, &peer.Server{Store: lim.Local()})
		go n.srv.Serve(lns[i])
		nodes[i] = n
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.srv.Stop()
			n.lim.Close()
		}
	})
	return nodes
}

// keyOwnedBy finds a key the ring assigns to owner.
func keyOwnedBy(t *testing.T, l *Limiter, owner string) string {
	for i := 0; i < 10_000; i++ {
		k := fmt.Sprintf("user-%d", i)
		if l.Owner(7, k) == owner {
			return k
		}
	}
	t.Fatalf("no key owned by %s", owner)
	return ""
}

func TestClusterEnforcesExactlyOnce(t *testing.T) {
	for _, pc := range []peer.Config{
		{MaxBatch: 100, FlushInterval: 500 * time.Microsecond, Timeout: time.Second},
		{MaxBatch: 1, Timeout: time.Second},
	} {
		t.Run(fmt.Sprintf("batch=%d", pc.MaxBatch), func(t *testing.T) {
			nodes := cluster(t, 3, pc)
			key := keyOwnedBy(t, nodes[0].lim, "gw-2")
			lr := env.LimitReq{ID: 7, Key: key, Count: 50, Per: time.Hour}
			var allowed atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 300; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ok, degraded := nodes[i%3].lim.Allow(context.Background(), lr)
					if degraded {
						t.Error("degraded decision with every node up")
					}
					if ok {
						allowed.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if allowed.Load() != 50 {
				t.Fatalf("allowed %d across the cluster, want exactly 50", allowed.Load())
			}
			// Only gw-1 and gw-3 send RPCs for a gw-2 key.
			rpcChecks := nodes[0].checks.Load() + nodes[2].checks.Load()
			if rpcChecks != 200 || nodes[1].checks.Load() != 0 {
				t.Fatalf("peer checks: gw-1+gw-3=%d gw-2=%d, want 200 and 0", rpcChecks, nodes[1].checks.Load())
			}
			if pc.MaxBatch > 1 && nodes[0].batches.Load()+nodes[2].batches.Load() >= rpcChecks {
				t.Errorf("no batching happened: %d RPCs for %d checks", nodes[0].batches.Load()+nodes[2].batches.Load(), rpcChecks)
			}
		})
	}
}

func TestOwnerDownFallsBackDegraded(t *testing.T) {
	nodes := cluster(t, 3, peer.Config{MaxBatch: 10, FlushInterval: 200 * time.Microsecond, Timeout: 50 * time.Millisecond})
	key := keyOwnedBy(t, nodes[0].lim, "gw-3")
	nodes[2].srv.Stop() // the owner dies

	lr := env.LimitReq{ID: 7, Key: key, Count: 30, Per: time.Hour}
	allowed := 0
	for i := 0; i < 50; i++ {
		ok, degraded := nodes[0].lim.Allow(context.Background(), lr)
		if !degraded {
			t.Fatal("decision for a dead owner's key was not degraded")
		}
		if ok {
			allowed++
		}
	}
	// Fallback allows count / nodes = 30/3 = 10 on this gateway.
	if allowed != 10 {
		t.Fatalf("fallback allowed %d, want 10", allowed)
	}
	if got := nodes[0].count(ResultDegraded); got != 50 {
		t.Fatalf("degraded decisions counted = %d", got)
	}
	// After the breaker opens, fallback decisions no longer wait for the
	// peer timeout.
	start := time.Now()
	nodes[0].lim.Allow(context.Background(), lr)
	if el := time.Since(start); el > 10*time.Millisecond {
		t.Fatalf("decision took %v with the peer's breaker open", el)
	}
}

func TestLocalOwnerNoRPC(t *testing.T) {
	nodes := cluster(t, 3, peer.Config{})
	key := keyOwnedBy(t, nodes[0].lim, "gw-1")
	nodes[0].lim.Allow(context.Background(), env.LimitReq{ID: 7, Key: key, Count: 1, Per: time.Second})
	if nodes[0].checks.Load() != 0 {
		t.Fatal("a key owned by this node went over RPC")
	}
}

func TestNewLocalAndBadConfig(t *testing.T) {
	l := NewLocal(nil)
	lr := env.LimitReq{ID: 1, Key: "k", Count: 2, Per: time.Hour, Algo: env.AlgoSlidingWindow}
	a, _ := l.Allow(context.Background(), lr)
	b, _ := l.Allow(context.Background(), lr)
	c, _ := l.Allow(context.Background(), lr)
	if !a || !b || c {
		t.Fatal("local limiter wrong")
	}
	if _, err := New(Config{Self: "x", Members: []Member{{Name: "y"}}}); err == nil {
		t.Fatal("self outside members accepted")
	}
}
