package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ t atomic.Int64 }

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.t.Add(int64(d)) }

func newTest(cfg Config) (*Breaker, *fakeClock) {
	c := &fakeClock{}
	c.t.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return New(cfg, WithClock(c.Now)), c
}

var testCfg = Config{
	Window: 10 * time.Second, Buckets: 10, MinRequests: 10,
	ErrorRate: 0.5, Cooldown: 5 * time.Second, HalfOpenProbes: 2,
}

func record(b *Breaker, ok, fail int) {
	for i := 0; i < ok; i++ {
		if b.Allow() {
			b.Record(true)
		}
	}
	for i := 0; i < fail; i++ {
		if b.Allow() {
			b.Record(false)
		}
	}
}

func TestStaysClosedBelowThresholds(t *testing.T) {
	tests := []struct {
		name      string
		ok, fails int
	}{
		{"below min requests", 0, 9},
		{"below error rate", 6, 4},
		{"all success", 100, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTest(testCfg)
			record(b, tc.ok, tc.fails)
			if s := b.State(); s != Closed {
				t.Fatalf("state = %v, want closed", s)
			}
		})
	}
}

func TestTripsAtErrorRate(t *testing.T) {
	b, _ := newTest(testCfg)
	record(b, 5, 5)
	if s := b.State(); s != Open {
		t.Fatalf("state = %v, want open", s)
	}
	if b.Allow() {
		t.Fatal("open breaker admitted a request")
	}
}

func TestWindowForgetsOldFailures(t *testing.T) {
	b, c := newTest(testCfg)
	record(b, 0, 9) // below min requests
	c.Advance(11 * time.Second)
	record(b, 9, 1)
	if s := b.State(); s != Closed {
		t.Fatalf("state = %v, want closed: old failures should have aged out", s)
	}
	total, fails := b.Counts()
	if total != 10 || fails != 1 {
		t.Fatalf("counts = %d/%d, want 10/1", total, fails)
	}
}

func TestHalfOpenRecovery(t *testing.T) {
	b, c := newTest(testCfg)
	record(b, 0, 10)
	if b.State() != Open {
		t.Fatal("expected open")
	}
	c.Advance(4 * time.Second)
	if b.State() != Open {
		t.Fatal("left open before cooldown")
	}
	c.Advance(time.Second)
	if s := b.State(); s != HalfOpen {
		t.Fatalf("state = %v, want half-open", s)
	}
	// Only HalfOpenProbes concurrent probes are admitted.
	if !b.Allow() || !b.Allow() {
		t.Fatal("probes not admitted")
	}
	if b.Allow() {
		t.Fatal("admitted more than HalfOpenProbes")
	}
	b.Record(true)
	b.Record(true)
	if s := b.State(); s != Closed {
		t.Fatalf("state = %v, want closed after successful probes", s)
	}
	// Window was reset: one failure must not trip immediately.
	record(b, 0, 1)
	if s := b.State(); s != Closed {
		t.Fatalf("state = %v, want closed", s)
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b, c := newTest(testCfg)
	record(b, 0, 10)
	c.Advance(5 * time.Second)
	if !b.Allow() {
		t.Fatal("probe not admitted")
	}
	b.Record(false)
	if s := b.State(); s != Open {
		t.Fatalf("state = %v, want open", s)
	}
	c.Advance(4 * time.Second)
	if b.State() != Open {
		t.Fatal("cooldown should restart from the failed probe")
	}
}

func TestOnChange(t *testing.T) {
	var got []State
	c := &fakeClock{}
	b := New(testCfg, WithClock(c.Now), WithOnChange(func(_, to State) { got = append(got, to) }))
	record(b, 0, 10)
	c.Advance(5 * time.Second)
	b.Allow()
	b.Record(true)
	b.Allow()
	b.Record(true)
	want := []State{Open, HalfOpen, Closed}
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", got, want)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	b := New(testCfg)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if b.Allow() {
					b.Record((i+g)%3 != 0)
				}
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkAllowRecord(b *testing.B) {
	br := New(DefaultConfig())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if br.Allow() {
				br.Record(true)
			}
		}
	})
}
