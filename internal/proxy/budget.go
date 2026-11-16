package proxy

import "sync/atomic"

const milli = 1000

// RetryBudget limits retries to a share of requests (Finagle-style). Each
// request deposits ratio tokens; each retry withdraws one. The balance is
// capped so a long quiet period cannot bank an unbounded retry storm.
// Lock-free: one atomic int64 in milli-tokens.
type RetryBudget struct {
	balance atomic.Int64
	deposit int64
	max     int64
}

// NewRetryBudget allows ratio retries per request plus an initial allowance.
func NewRetryBudget(ratio float64, initial int) *RetryBudget {
	capTokens := int64(initial)
	if capTokens < 100 {
		capTokens = 100
	}
	b := &RetryBudget{deposit: int64(ratio * milli), max: capTokens * milli}
	b.balance.Store(int64(initial) * milli)
	return b
}

// Deposit records one request.
func (b *RetryBudget) Deposit() {
	for {
		cur := b.balance.Load()
		next := cur + b.deposit
		if next > b.max {
			next = b.max
		}
		if next == cur || b.balance.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Withdraw takes one retry token, reporting false if the budget is spent.
func (b *RetryBudget) Withdraw() bool {
	for {
		cur := b.balance.Load()
		if cur < milli {
			return false
		}
		if b.balance.CompareAndSwap(cur, cur-milli) {
			return true
		}
	}
}
