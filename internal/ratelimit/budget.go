package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/cost"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// Hold is a reservation the gateway must settle when the response ends.
type Hold struct {
	ID       string
	Key      cost.Key
	Owner    string // "" = this node's store
	Degraded bool   // held on the local fallback store
	Estimate int64
}

// BudgetHooks receive budget events (metrics). Any may be nil.
type BudgetHooks struct {
	Reserved func(estimate int64, degraded bool)
	Denied   func()
	Settled  func(actual int64)
	Expired  func(amount int64)
}

func newHoldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func budgetRingKey(id int, key string) string { return "b" + ringKey(id, key) }

// Budgets returns the store for budgets this node owns (served to peers).
func (l *Limiter) Budgets() *cost.Store { return l.costLocal }

// Reserve holds estimate against a budget on the owner node. ok=false means
// the budget cannot cover it (answer 429). An unreachable owner falls back
// to a local budget of amount/N, marked degraded.
func (l *Limiter) Reserve(ctx context.Context, br env.BudgetReq, estimate int64, ttl time.Duration) (Hold, bool) {
	h := Hold{ID: newHoldID(), Key: cost.Key{BudgetID: br.ID, Value: br.Key}, Estimate: estimate}
	r := l.ring.Load()
	owner := ""
	if r.Len() > 0 {
		owner = r.Owner(budgetRingKey(br.ID, br.Key))
	}
	if owner != "" && owner != l.self {
		if p := l.peers[owner]; p != nil && p.breaker.Allow() {
			resp, err := p.client.Reserve(ctx, &tollgatev1.ReserveRequest{
				ReservationId: h.ID, BudgetId: int32(br.ID), Key: br.Key, EstimateMicroUsd: estimate,
				AmountMicroUsd: br.Amount, PerNs: int64(br.Per), TtlNs: int64(ttl),
			})
			p.breaker.Record(err == nil)
			if err == nil {
				h.Owner = owner
				l.budgetEvent(resp.Ok, estimate, false)
				return h, resp.Ok
			}
		}
		// Owner unreachable: hold against a local share of the budget.
		share := br.Amount / int64(r.Len())
		h.Degraded = true
		ok, _ := l.costFallback.Reserve(h.ID, h.Key, estimate, share, br.Per, ttl)
		l.budgetEvent(ok, estimate, true)
		return h, ok
	}
	ok, _ := l.costLocal.Reserve(h.ID, h.Key, estimate, br.Amount, br.Per, ttl)
	l.budgetEvent(ok, estimate, false)
	return h, ok
}

func (l *Limiter) budgetEvent(ok bool, estimate int64, degraded bool) {
	switch {
	case ok && l.budgetHooks.Reserved != nil:
		l.budgetHooks.Reserved(estimate, degraded)
	case !ok && l.budgetHooks.Denied != nil:
		l.budgetHooks.Denied()
	}
}

// Settle charges the real cost for a hold. For a remote owner it retries in
// the background (settles are idempotent); if every attempt fails, the
// owner releases the hold at its deadline, which under-charges rather than
// leaks.
func (l *Limiter) Settle(h Hold, actual int64) {
	if l.budgetHooks.Settled != nil {
		l.budgetHooks.Settled(actual)
	}
	switch {
	case h.Degraded:
		l.costFallback.Settle(h.ID, h.Key, actual)
	case h.Owner == "":
		l.costLocal.Settle(h.ID, h.Key, actual)
	default:
		p := l.peers[h.Owner]
		if p == nil {
			return
		}
		req := &tollgatev1.SettleRequest{ReservationId: h.ID, BudgetId: int32(h.Key.BudgetID), Key: h.Key.Value, ActualMicroUsd: actual}
		go func() {
			for attempt, wait := 0, 50*time.Millisecond; attempt < 5; attempt, wait = attempt+1, wait*2 {
				if _, err := p.client.Settle(context.Background(), req); err == nil {
					return
				}
				time.Sleep(wait)
			}
		}()
	}
}
