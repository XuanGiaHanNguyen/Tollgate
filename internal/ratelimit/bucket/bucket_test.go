package bucket

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func TestBurstThenRefill(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	k := Key{1, "u1"}
	for i := 0; i < 10; i++ {
		if !s.Allow(k, 10, time.Minute) {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	if s.Allow(k, 10, time.Minute) {
		t.Fatal("11th request allowed")
	}
	c.advance(6 * time.Second) // 10/min = one token per 6s
	if !s.Allow(k, 10, time.Minute) {
		t.Fatal("not refilled after 6s")
	}
	if s.Allow(k, 10, time.Minute) {
		t.Fatal("refilled more than one token")
	}
	c.advance(time.Hour)
	n := 0
	for s.Allow(k, 10, time.Minute) {
		n++
	}
	if n != 10 {
		t.Fatalf("refill exceeded capacity: %d", n)
	}
}

func TestKeysAndLimitsIndependent(t *testing.T) {
	s := New((&clock{}).now)
	if !s.Allow(Key{1, "a"}, 1, time.Second) || !s.Allow(Key{1, "b"}, 1, time.Second) || !s.Allow(Key{2, "a"}, 1, time.Second) {
		t.Fatal("buckets interfere")
	}
	if s.Allow(Key{1, "a"}, 1, time.Second) {
		t.Fatal("bucket not enforced")
	}
}

func TestLongRunRate(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	allowed := 0
	// 1000 requests/s for 60s against 100/s: expect 100 burst + 100*60 refill ±1.
	for i := 0; i < 60_000; i++ {
		if s.Allow(Key{1, "k"}, 100, time.Second) {
			allowed++
		}
		c.advance(time.Millisecond)
	}
	if allowed < 6099 || allowed > 6101 {
		t.Fatalf("allowed %d, want ~6100", allowed)
	}
}

func TestSweep(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	s.Allow(Key{1, "old"}, 1, time.Second)
	s.Allow(Key{1, "day"}, 1, 24*time.Hour)
	c.advance(time.Hour)
	s.Allow(Key{1, "new"}, 1, time.Second)
	// "old" has refilled; "new" was just drained; "day" is still refilling.
	if n := s.Sweep(); n != 1 || s.Len() != 2 {
		t.Fatalf("swept %d, left %d", n, s.Len())
	}
}

func TestInvalid(t *testing.T) {
	s := New(nil)
	if s.Allow(Key{1, "k"}, 0, time.Second) || s.Allow(Key{1, "k"}, 1, 0) {
		t.Fatal("invalid limit allowed")
	}
}

func TestConcurrentExactCount(t *testing.T) {
	s := New((&clock{}).now) // frozen clock: no refill
	var ok atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if s.Allow(Key{1, "hot"}, 500, time.Minute) {
					ok.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 500 {
		t.Fatalf("allowed %d, want exactly 500", ok.Load())
	}
}

func BenchmarkAllowParallelKeys(b *testing.B) {
	s := New(nil)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + string(rune('a'+i/26%26))
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Allow(Key{1, keys[i%len(keys)]}, 1_000_000, time.Second)
			i++
		}
	})
}

func TestSlidingWindow(t *testing.T) {
	c := &clock{}
	c.t.Store(int64(time.Minute)) // window-aligned start
	s := New(c.now)
	k := Key{1, "u"}
	allow := func() bool { return s.AllowAlgo(k, 10, time.Minute, SlidingWindow) }
	for i := 0; i < 10; i++ {
		if !allow() {
			t.Fatalf("request %d denied", i)
		}
	}
	if allow() {
		t.Fatal("11th request in the window allowed")
	}
	// Half-way into the next window the previous window still weighs 50%:
	// estimate = 10*0.5 + 0 = 5, so 5 more are admitted.
	c.advance(time.Minute + 30*time.Second)
	n := 0
	for allow() {
		n++
	}
	if n != 5 {
		t.Fatalf("admitted %d half-way into the next window, want 5", n)
	}
	// Two full windows later the history is gone.
	c.advance(2 * time.Minute)
	n = 0
	for allow() {
		n++
	}
	if n != 10 {
		t.Fatalf("admitted %d after idle, want 10", n)
	}
}

func TestSlidingWindowLongRunRate(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	allowed := 0
	// 1000 req/s for 60s against 100/s; sliding windows have no burst
	// allowance beyond the limit, so expect ~100 per second.
	for i := 0; i < 60_000; i++ {
		if s.AllowAlgo(Key{1, "k"}, 100, time.Second, SlidingWindow) {
			allowed++
		}
		c.advance(time.Millisecond)
	}
	if allowed < 5900 || allowed > 6100 {
		t.Fatalf("allowed %d, want ~6000", allowed)
	}
}

func TestAlgorithmChangeResetsState(t *testing.T) {
	s := New((&clock{}).now)
	k := Key{1, "u"}
	s.Allow(k, 1, time.Hour)
	if s.Allow(k, 1, time.Hour) {
		t.Fatal("token bucket not enforced")
	}
	if !s.AllowAlgo(k, 1, time.Hour, SlidingWindow) {
		t.Fatal("state not reset when the algorithm changed")
	}
}

func TestSweepSlidingWindow(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	s.AllowAlgo(Key{1, "a"}, 1, time.Second, SlidingWindow)
	c.advance(time.Second)
	if s.Sweep() != 0 {
		t.Fatal("swept a window that still carries history")
	}
	c.advance(2 * time.Second)
	if s.Sweep() != 1 {
		t.Fatal("did not sweep an idle window")
	}
}
