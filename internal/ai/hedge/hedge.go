// Package hedge implements hedged requests across model providers, after
// Dean and Barroso's "The Tail at Scale": when the primary has not produced
// its first token by its p95, send the same request to the next provider,
// keep whichever produces a first token first, and cancel the other.
package hedge

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Tracker keeps a rolling window of time-to-first-token samples per key
// (provider and model). Each window has its own lock; the map of windows
// is read-mostly.
type Tracker struct {
	size, min int
	mu        sync.RWMutex
	w         map[string]*window
}

type window struct {
	mu      sync.Mutex
	samples []time.Duration
	next    int
	full    bool
}

// NewTracker keeps the last size samples per key and reports quantiles only
// after min samples.
func NewTracker(size, min int) *Tracker {
	if size <= 0 {
		size = 256
	}
	if min <= 0 {
		min = 20
	}
	return &Tracker{size: size, min: min, w: map[string]*window{}}
}

func (t *Tracker) window(key string) *window {
	t.mu.RLock()
	w := t.w[key]
	t.mu.RUnlock()
	if w != nil {
		return w
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if w = t.w[key]; w == nil {
		w = &window{samples: make([]time.Duration, t.size)}
		t.w[key] = w
	}
	return w
}

// Observe records one time-to-first-token.
func (t *Tracker) Observe(key string, d time.Duration) {
	w := t.window(key)
	w.mu.Lock()
	w.samples[w.next] = d
	w.next = (w.next + 1) % len(w.samples)
	w.full = w.full || w.next == 0
	w.mu.Unlock()
}

// Quantile returns the q-quantile of the window, or false with too few samples.
func (t *Tracker) Quantile(key string, q float64) (time.Duration, bool) {
	w := t.window(key)
	w.mu.Lock()
	n := w.next
	if w.full {
		n = len(w.samples)
	}
	if n < t.min {
		w.mu.Unlock()
		return 0, false
	}
	cp := append([]time.Duration(nil), w.samples[:n]...)
	w.mu.Unlock()
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	i := int(q * float64(n))
	if i >= n {
		i = n - 1
	}
	return cp[i], true
}

// Budget caps hedges to a share of requests: each request deposits ratio
// tokens, each hedge withdraws one (lock-free, milli-tokens).
type Budget struct {
	balance atomic.Int64
	deposit int64
	max     int64
}

// NewBudget allows ratio hedges per request plus a small initial allowance.
func NewBudget(ratio float64, initial int) *Budget {
	b := &Budget{deposit: int64(ratio * 1000), max: int64(max(initial, 20)) * 1000}
	b.balance.Store(int64(initial) * 1000)
	return b
}

// Deposit records one request.
func (b *Budget) Deposit() {
	for {
		cur := b.balance.Load()
		next := min(cur+b.deposit, b.max)
		if next == cur || b.balance.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Withdraw takes one hedge token.
func (b *Budget) Withdraw() bool {
	for {
		cur := b.balance.Load()
		if cur < 1000 {
			return false
		}
		if b.balance.CompareAndSwap(cur, cur-1000) {
			return true
		}
	}
}

// Result is what an attempt produces once its first token is available.
type Result interface{ Close() error }

// Attempt starts one provider call and returns when its first token is
// available (or it failed).
type Attempt func(ctx context.Context) (Result, error)

// Outcome describes how a race went.
type Outcome struct {
	Winner      int  // index into attempts
	Hedged      bool // a backup was started because the primary was late
	FailedOver  bool // a backup was started because the primary failed
	LoserCalled []int
}

// ErrAllFailed is returned when every attempt failed.
var ErrAllFailed = errors.New("all providers failed")

// Race runs attempts[0] and, if it has not produced a first token after
// `after` and allowHedge() says yes, attempts[1]. If the running attempt
// fails, the next one starts at once (failover, not a hedge). The first
// attempt to produce a first token wins; the others are cancelled and
// their results closed. The returned cancel must be called when the caller
// is done with the winning result.
func Race(ctx context.Context, attempts []Attempt, after time.Duration, allowHedge func() bool) (Result, context.CancelFunc, Outcome, error) {
	type res struct {
		i   int
		r   Result
		err error
	}
	out := Outcome{Winner: -1}
	results := make(chan res, len(attempts))
	cancels := make([]context.CancelFunc, len(attempts))
	started, finished := 0, 0
	start := func() {
		i := started
		started++
		actx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		go func() {
			r, err := attempts[i](actx)
			results <- res{i, r, err}
		}()
	}
	start()

	var timer <-chan time.Time
	if len(attempts) > 1 && after > 0 {
		t := time.NewTimer(after)
		defer t.Stop()
		timer = t.C
	}
	var lastErr error
	for {
		select {
		case <-timer:
			timer = nil
			if started < len(attempts) && allowHedge() {
				out.Hedged = true
				start()
			}
		case r := <-results:
			finished++
			if r.err != nil {
				lastErr = r.err
				cancels[r.i]()
				if started < len(attempts) { // fail over to the next provider
					out.FailedOver = true
					start()
					continue
				}
				if finished == started {
					return nil, func() {}, out, errors.Join(ErrAllFailed, lastErr)
				}
				continue
			}
			out.Winner = r.i
			for j := 0; j < started; j++ {
				if j != r.i {
					cancels[j]()
					out.LoserCalled = append(out.LoserCalled, j)
				}
			}
			// Close losers' results as they come back.
			if pending := started - finished; pending > 0 {
				go func(n int) {
					for k := 0; k < n; k++ {
						if l := <-results; l.r != nil {
							l.r.Close()
						}
					}
				}(pending)
			}
			return r.r, cancels[r.i], out, nil
		case <-ctx.Done():
			for j := 0; j < started; j++ {
				cancels[j]()
			}
			return nil, func() {}, out, ctx.Err()
		}
	}
}
