package ratelimit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/cost"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/peer"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

func budgetKeyOwnedBy(t *testing.T, l *Limiter, owner string) string {
	for i := 0; i < 10_000; i++ {
		k := fmt.Sprintf("user-%d", i)
		if l.ring.Load().Owner(budgetRingKey(3, k)) == owner {
			return k
		}
	}
	t.Fatalf("no budget key owned by %s", owner)
	return ""
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRemoteReserveSettle(t *testing.T) {
	nodes := cluster(t, 3, peer.Config{Timeout: time.Second})
	user := budgetKeyOwnedBy(t, nodes[0].lim, "gw-2")
	br := env.BudgetReq{ID: 3, Amount: 1_000_000, Per: time.Hour, Key: user}
	ck := cost.Key{BudgetID: 3, Value: user}

	h, ok := nodes[0].lim.Reserve(context.Background(), br, 600_000, time.Minute)
	if !ok || h.Owner != "gw-2" || h.Degraded {
		t.Fatalf("hold %+v ok=%v", h, ok)
	}
	// The budget lives on gw-2 only, and a second big hold doesn't fit.
	if _, ok := nodes[2].lim.Reserve(context.Background(), br, 600_000, time.Minute); ok {
		t.Fatal("over-reserved through another gateway")
	}
	nodes[0].lim.Settle(h, 100_000)
	waitUntil(t, func() bool { b, _ := nodes[1].lim.Budgets().Balance(ck); return b >= 899_000 })
	if st := nodes[1].lim.Budgets().Stats(); st.Active != 0 || st.Settled != 1 {
		t.Fatalf("owner stats %+v", st)
	}
}

func TestOwnerDownBudgetDegraded(t *testing.T) {
	nodes := cluster(t, 3, peer.Config{Timeout: 50 * time.Millisecond})
	user := budgetKeyOwnedBy(t, nodes[0].lim, "gw-3")
	nodes[2].srv.Stop()
	br := env.BudgetReq{ID: 3, Amount: 3_000_000, Per: time.Hour, Key: user}
	// Fallback share is amount/3 = 1_000_000.
	h, ok := nodes[0].lim.Reserve(context.Background(), br, 900_000, time.Minute)
	if !ok || !h.Degraded {
		t.Fatalf("hold %+v ok=%v", h, ok)
	}
	if _, ok := nodes[0].lim.Reserve(context.Background(), br, 200_000, time.Minute); ok {
		t.Fatal("degraded budget exceeded its share")
	}
}

// A gateway that reserves and then dies never settles; the owner must
// release the hold at its deadline, so nothing leaks.
func TestCrashedGatewayHoldExpires(t *testing.T) {
	nodes := cluster(t, 3, peer.Config{Timeout: time.Second})
	user := budgetKeyOwnedBy(t, nodes[0].lim, "gw-2")
	br := env.BudgetReq{ID: 3, Amount: 1_000_000, Per: 24 * time.Hour, Key: user}
	if _, ok := nodes[0].lim.Reserve(context.Background(), br, 800_000, 200*time.Millisecond); !ok {
		t.Fatal("reserve failed")
	}
	nodes[0].srv.Stop() // the reserving gateway "crashes": no settle ever comes
	owner := nodes[1].lim.Budgets()
	waitUntil(t, func() bool { return owner.Stats().Expired == 1 }) // expiry runs every second
	if st := owner.Stats(); st.Active != 0 {
		t.Fatalf("leaked: %+v", st)
	}
	if b, _ := owner.Balance(cost.Key{BudgetID: 3, Value: user}); b < 999_000 {
		t.Fatalf("hold not released: balance %d", b)
	}
}
