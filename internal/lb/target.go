// Package lb holds backend targets, load-balancing policies and health checks.
package lb

import (
	"net/url"
	"sync/atomic"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/breaker"
)

// PassiveConfig ejects a target after consecutive failures seen on real traffic.
type PassiveConfig struct {
	ConsecutiveFailures int64         // 0 disables passive ejection
	EjectDuration       time.Duration // how long an ejected target is skipped
}

// Target is one backend instance. All fields read on the request path are atomic.
type Target struct {
	URL     *url.URL
	Weight  int
	Breaker *breaker.Breaker

	healthy      atomic.Bool
	ejectedUntil atomic.Int64 // unix nanos; 0 = not ejected
	inflight     atomic.Int64
	consecFails  atomic.Int64

	passive PassiveConfig
	now     func() time.Time
}

// NewTarget returns a target that starts healthy.
func NewTarget(u *url.URL, weight int, br *breaker.Breaker, passive PassiveConfig) *Target {
	if weight <= 0 {
		weight = 1
	}
	t := &Target{URL: u, Weight: weight, Breaker: br, passive: passive, now: time.Now}
	t.healthy.Store(true)
	return t
}

// Healthy reports the active health-check result.
func (t *Target) Healthy() bool { return t.healthy.Load() }

// SetHealthy is used by the active health checker.
func (t *Target) SetHealthy(v bool) { t.healthy.Store(v) }

// Ejected reports whether passive health checking has ejected the target.
func (t *Target) Ejected() bool {
	until := t.ejectedUntil.Load()
	return until != 0 && t.now().UnixNano() < until
}

// Selectable reports whether the balancer may pick the target. It does not
// consult the breaker, because Allow has side effects in half-open state.
func (t *Target) Selectable() bool { return t.Healthy() && !t.Ejected() }

// Inflight is the number of requests currently using the target.
func (t *Target) Inflight() int64 { return t.inflight.Load() }

// Acquire marks a request as started; call Release exactly once when done.
func (t *Target) Acquire() { t.inflight.Add(1) }

// Release marks a request as finished.
func (t *Target) Release() { t.inflight.Add(-1) }

// ObserveResult feeds passive health checking. success means no connection
// error and a non-5xx status.
func (t *Target) ObserveResult(success bool) {
	if success {
		t.consecFails.Store(0)
		return
	}
	if t.passive.ConsecutiveFailures <= 0 {
		return
	}
	if t.consecFails.Add(1) >= t.passive.ConsecutiveFailures {
		t.consecFails.Store(0)
		t.ejectedUntil.Store(t.now().Add(t.passive.EjectDuration).UnixNano())
	}
}

func (t *Target) String() string { return t.URL.String() }
