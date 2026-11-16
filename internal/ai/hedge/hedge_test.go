package hedge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResult struct{ closed *atomic.Int32 }

func (f fakeResult) Close() error { f.closed.Add(1); return nil }

// attempt simulates a provider: first token after d, or an error.
func attempt(d time.Duration, fail bool, cancelled, closed *atomic.Int32) Attempt {
	return func(ctx context.Context) (Result, error) {
		select {
		case <-time.After(d):
			if fail {
				return nil, errors.New("boom")
			}
			return fakeResult{closed}, nil
		case <-ctx.Done():
			cancelled.Add(1) // the cancel actually stopped this request
			return nil, ctx.Err()
		}
	}
}

func TestTrackerQuantile(t *testing.T) {
	tr := NewTracker(100, 10)
	if _, ok := tr.Quantile("p", 0.95); ok {
		t.Fatal("quantile with no samples")
	}
	for i := 1; i <= 100; i++ {
		tr.Observe("p", time.Duration(i)*time.Millisecond)
	}
	if q, _ := tr.Quantile("p", 0.95); q != 96*time.Millisecond {
		t.Fatalf("p95 = %v", q)
	}
	for i := 0; i < 100; i++ { // window rolls over
		tr.Observe("p", time.Millisecond)
	}
	if q, _ := tr.Quantile("p", 0.95); q != time.Millisecond {
		t.Fatalf("old samples kept: %v", q)
	}
}

func TestBudgetNeverExceeded(t *testing.T) {
	b := NewBudget(0.05, 0)
	hedges := 0
	for i := 0; i < 10_000; i++ {
		b.Deposit()
		if b.Withdraw() {
			hedges++
		}
	}
	if hedges > 500 {
		t.Fatalf("%d hedges for 10000 requests at 5%%", hedges)
	}
	if hedges < 490 {
		t.Fatalf("budget too stingy: %d", hedges)
	}
}

func TestPrimaryFastNoHedge(t *testing.T) {
	var cancelled, closed atomic.Int32
	_, done, out, err := Race(context.Background(),
		[]Attempt{attempt(5*time.Millisecond, false, &cancelled, &closed), attempt(time.Millisecond, false, &cancelled, &closed)},
		50*time.Millisecond, func() bool { return true })
	defer done()
	if err != nil || out.Winner != 0 || out.Hedged {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestSlowPrimaryHedgeWinsAndPrimaryCancelled(t *testing.T) {
	var cancelled, closed atomic.Int32
	start := time.Now()
	_, done, out, err := Race(context.Background(),
		[]Attempt{attempt(time.Second, false, &cancelled, &closed), attempt(5*time.Millisecond, false, &cancelled, &closed)},
		20*time.Millisecond, func() bool { return true })
	defer done()
	if err != nil || out.Winner != 1 || !out.Hedged {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("took %v", el)
	}
	deadline := time.Now().Add(time.Second)
	for cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cancelled.Load() != 1 {
		t.Fatal("losing primary was not cancelled")
	}
}

func TestHedgeDeniedByBudget(t *testing.T) {
	var cancelled, closed atomic.Int32
	_, done, out, _ := Race(context.Background(),
		[]Attempt{attempt(60*time.Millisecond, false, &cancelled, &closed), attempt(time.Millisecond, false, &cancelled, &closed)},
		10*time.Millisecond, func() bool { return false })
	defer done()
	if out.Hedged || out.Winner != 0 {
		t.Fatalf("hedged without budget: %+v", out)
	}
}

func TestFailoverAndAllFailed(t *testing.T) {
	var cancelled, closed atomic.Int32
	_, done, out, err := Race(context.Background(),
		[]Attempt{attempt(time.Millisecond, true, &cancelled, &closed), attempt(time.Millisecond, false, &cancelled, &closed)},
		time.Second, func() bool { return true })
	done()
	if err != nil || out.Winner != 1 || !out.FailedOver || out.Hedged {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	_, done, _, err = Race(context.Background(),
		[]Attempt{attempt(time.Millisecond, true, &cancelled, &closed), attempt(time.Millisecond, true, &cancelled, &closed)},
		time.Second, func() bool { return true })
	done()
	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("err = %v", err)
	}
}

// When both produce a first token, the loser's result is closed.
func TestLoserResultClosed(t *testing.T) {
	var cancelled, closed atomic.Int32
	slowButSucceeds := func(ctx context.Context) (Result, error) {
		time.Sleep(30 * time.Millisecond) // ignores ctx: produced a token anyway
		return fakeResult{&closed}, nil
	}
	_, done, out, _ := Race(context.Background(),
		[]Attempt{slowButSucceeds, attempt(15*time.Millisecond, false, &cancelled, &closed)},
		5*time.Millisecond, func() bool { return true })
	defer done()
	if out.Winner != 1 {
		t.Fatalf("out=%+v", out)
	}
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("loser result leaked")
	}
}
