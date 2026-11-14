package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/lb"
)

// Errors surfaced to the proxy's error handler.
var (
	ErrNoHealthyBackend = errors.New("no healthy backend")
	ErrCircuitOpen      = errors.New("circuit open for all backends")
)

// maxReplayBody is the largest request body buffered so it can be retried.
const maxReplayBody = 1 << 20

// Observer receives per-attempt events (metrics hook; may be nil).
type Observer interface {
	Retry(route string)
}

// retryTransport picks a target per attempt, applies breaker and passive
// health accounting, and retries idempotent requests within the budget.
type retryTransport struct {
	route    string
	base     http.RoundTripper
	upstream *Upstream
	retry    config.Retry
	budget   *RetryBudget
	observer Observer
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	}
	return false
}

func retryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	canRetry := t.retry.Attempts > 1 && idempotent(req.Method)

	var body []byte
	if canRetry && req.Body != nil && req.Body != http.NoBody {
		if req.ContentLength > maxReplayBody {
			canRetry = false
		} else {
			orig := req.Body
			b, err := io.ReadAll(io.LimitReader(orig, maxReplayBody+1))
			if err != nil {
				orig.Close()
				return nil, err
			}
			if len(b) > maxReplayBody {
				// Too large to replay: send what we read followed by the rest.
				canRetry = false
				req.Body = readCloser{io.MultiReader(bytes.NewReader(b), orig), orig}
			} else {
				orig.Close()
				body = b
			}
		}
	}
	t.budget.Deposit()

	var tried []*lb.Target
	for attempt := 0; ; attempt++ {
		target, err := t.pick(tried)
		if err != nil {
			return nil, err
		}
		out := req.Clone(ctx)
		out.URL.Scheme = target.URL.Scheme
		out.URL.Host = target.URL.Host
		if body != nil {
			out.Body = io.NopCloser(bytes.NewReader(body))
			out.ContentLength = int64(len(body))
			out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}

		target.Acquire()
		resp, err := t.base.RoundTrip(out)
		clientGone := ctx.Err() != nil
		success := err == nil && resp.StatusCode < 500
		if clientGone && err != nil {
			// Not the backend's fault; don't penalize it.
			if target.Breaker != nil {
				target.Breaker.Ignore()
			}
		} else {
			target.ObserveResult(success)
			if target.Breaker != nil {
				target.Breaker.Record(success)
			}
		}

		retryable := !clientGone && (err != nil || retryableStatus(resp.StatusCode))
		if !retryable || !canRetry || attempt+1 >= t.retry.Attempts || !t.budget.Withdraw() {
			if err != nil {
				target.Release()
				return nil, err
			}
			resp.Body = &releaseBody{ReadCloser: resp.Body, target: target}
			return resp, nil
		}

		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		target.Release()
		tried = append(tried, target)
		if t.observer != nil {
			t.observer.Retry(t.route)
		}
		if err := sleepCtx(ctx, backoff(attempt, t.retry.BaseBackoff, t.retry.MaxBackoff)); err != nil {
			return nil, err
		}
	}
}

// pick asks the balancer for a target whose breaker admits the request.
func (t *retryTransport) pick(tried []*lb.Target) (*lb.Target, error) {
	exclude := tried
	denied := 0
	for i := 0; i < len(t.upstream.Targets); i++ {
		target := t.upstream.Balancer.Pick(exclude)
		if target == nil {
			break
		}
		if target.Breaker == nil || target.Breaker.Allow() {
			return target, nil
		}
		denied++
		if containsTarget(exclude, target) {
			break // balancer fell back to an already-excluded target
		}
		exclude = append(exclude[:len(exclude):len(exclude)], target)
	}
	if denied > 0 {
		return nil, ErrCircuitOpen
	}
	return nil, ErrNoHealthyBackend
}

func containsTarget(ts []*lb.Target, t *lb.Target) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// backoff is exponential with full jitter: rand(0, min(max, base*2^attempt)).
func backoff(attempt int, base, maxD time.Duration) time.Duration {
	d := base << attempt
	if d <= 0 || d > maxD {
		d = maxD
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

type readCloser struct {
	io.Reader
	io.Closer
}

// releaseBody frees the target's in-flight slot when the body is closed, so
// least-connections counts streaming responses until they finish.
type releaseBody struct {
	io.ReadCloser
	target *lb.Target
	done   bool
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.done {
		b.done = true
		b.target.Release()
	}
	return err
}
