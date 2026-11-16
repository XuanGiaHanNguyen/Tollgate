// Package breaker implements a per-backend circuit breaker.
//
// States: closed → open when the error rate in a rolling window crosses a
// threshold → half-open after a cooldown, admitting a few probes → closed
// once enough probes succeed. Any probe failure reopens the breaker.
//
// The rolling window is a ring of time buckets with atomic counters, so the
// request path takes no locks. A bucket reset racing with a record can drop
// a few counts at a bucket boundary; that is acceptable for a threshold.
package breaker

import (
	"sync/atomic"
	"time"
)

// State of a breaker. Values match the tollgate_breaker_state gauge.
type State int32

const (
	Closed   State = 0
	HalfOpen State = 1
	Open     State = 2
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half-open"
	case Open:
		return "open"
	}
	return "unknown"
}

// Config controls when the breaker trips and recovers.
type Config struct {
	Window         time.Duration // rolling window length
	Buckets        int           // number of buckets in the window
	MinRequests    int64         // don't trip below this many requests in the window
	ErrorRate      float64       // trip at or above this failure ratio
	Cooldown       time.Duration // open → half-open delay
	HalfOpenProbes int64         // concurrent probes allowed, and successes needed to close
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		Window:         10 * time.Second,
		Buckets:        10,
		MinRequests:    20,
		ErrorRate:      0.5,
		Cooldown:       5 * time.Second,
		HalfOpenProbes: 3,
	}
}

type bucket struct {
	epoch atomic.Int64 // bucket index in absolute time (now / bucketDur)
	total atomic.Int64
	fails atomic.Int64
}

// Breaker is safe for concurrent use.
type Breaker struct {
	cfg       Config
	bucketDur int64 // nanoseconds
	buckets   []bucket
	now       func() time.Time

	state    atomic.Int32
	openedAt atomic.Int64 // unix nanos
	probes   atomic.Int64 // probes in flight while half-open
	probeOK  atomic.Int64 // successful probes while half-open
	onChange func(from, to State)
}

// Option customizes a Breaker.
type Option func(*Breaker)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(b *Breaker) { b.now = now } }

// WithOnChange registers a callback for state transitions.
func WithOnChange(f func(from, to State)) Option { return func(b *Breaker) { b.onChange = f } }

// New builds a breaker. Zero fields in cfg take defaults.
func New(cfg Config, opts ...Option) *Breaker {
	d := DefaultConfig()
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.Buckets <= 0 {
		cfg.Buckets = d.Buckets
	}
	if cfg.MinRequests <= 0 {
		cfg.MinRequests = d.MinRequests
	}
	if cfg.ErrorRate <= 0 {
		cfg.ErrorRate = d.ErrorRate
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = d.Cooldown
	}
	if cfg.HalfOpenProbes <= 0 {
		cfg.HalfOpenProbes = d.HalfOpenProbes
	}
	b := &Breaker{
		cfg:       cfg,
		bucketDur: int64(cfg.Window) / int64(cfg.Buckets),
		buckets:   make([]bucket, cfg.Buckets),
		now:       time.Now,
	}
	if b.bucketDur <= 0 {
		b.bucketDur = 1
	}
	for _, o := range opts {
		o(b)
	}
	for i := range b.buckets {
		b.buckets[i].epoch.Store(-1)
	}
	return b
}

// State reports the current state, moving open → half-open if the cooldown passed.
func (b *Breaker) State() State {
	s := State(b.state.Load())
	if s == Open && b.now().UnixNano()-b.openedAt.Load() >= int64(b.cfg.Cooldown) {
		if b.transition(Open, HalfOpen) {
			b.probes.Store(0)
			b.probeOK.Store(0)
		}
		return State(b.state.Load())
	}
	return s
}

// Allow reports whether a request may proceed. Every allowed request must be
// followed by exactly one call to Record.
func (b *Breaker) Allow() bool {
	switch b.State() {
	case Closed:
		return true
	case HalfOpen:
		for {
			n := b.probes.Load()
			if n >= b.cfg.HalfOpenProbes {
				return false
			}
			if b.probes.CompareAndSwap(n, n+1) {
				return true
			}
		}
	default:
		return false
	}
}

// Record reports the outcome of a request that Allow admitted.
func (b *Breaker) Record(success bool) {
	switch State(b.state.Load()) {
	case HalfOpen:
		if !success {
			b.trip(HalfOpen)
			return
		}
		if b.probeOK.Add(1) >= b.cfg.HalfOpenProbes {
			if b.transition(HalfOpen, Closed) {
				b.resetWindow()
			}
		}
		return
	case Open:
		return // late result from before the trip
	}

	now := b.now().UnixNano()
	bk := b.bucketFor(now)
	bk.total.Add(1)
	if !success {
		bk.fails.Add(1)
		b.maybeTrip(now)
	}
}

// Ignore ends an admitted request without counting it either way, e.g. when
// the client went away. It frees a half-open probe slot.
func (b *Breaker) Ignore() {
	if State(b.state.Load()) == HalfOpen {
		b.probes.Add(-1)
	}
}

func (b *Breaker) bucketFor(now int64) *bucket {
	epoch := now / b.bucketDur
	bk := &b.buckets[epoch%int64(len(b.buckets))]
	old := bk.epoch.Load()
	if old != epoch && bk.epoch.CompareAndSwap(old, epoch) {
		bk.total.Store(0)
		bk.fails.Store(0)
	}
	return bk
}

// counts sums buckets that fall inside the window ending at now.
func (b *Breaker) counts(now int64) (total, fails int64) {
	cur := now / b.bucketDur
	oldest := cur - int64(len(b.buckets)) + 1
	for i := range b.buckets {
		bk := &b.buckets[i]
		if e := bk.epoch.Load(); e >= oldest && e <= cur {
			total += bk.total.Load()
			fails += bk.fails.Load()
		}
	}
	return
}

// Counts returns requests and failures in the current window.
func (b *Breaker) Counts() (total, fails int64) { return b.counts(b.now().UnixNano()) }

func (b *Breaker) maybeTrip(now int64) {
	total, fails := b.counts(now)
	if total >= b.cfg.MinRequests && float64(fails)/float64(total) >= b.cfg.ErrorRate {
		b.trip(Closed)
	}
}

func (b *Breaker) trip(from State) {
	b.openedAt.Store(b.now().UnixNano())
	b.transition(from, Open)
}

func (b *Breaker) resetWindow() {
	for i := range b.buckets {
		b.buckets[i].epoch.Store(-1)
		b.buckets[i].total.Store(0)
		b.buckets[i].fails.Store(0)
	}
}

func (b *Breaker) transition(from, to State) bool {
	if !b.state.CompareAndSwap(int32(from), int32(to)) {
		return false
	}
	if b.onChange != nil {
		b.onChange(from, to)
	}
	return true
}
