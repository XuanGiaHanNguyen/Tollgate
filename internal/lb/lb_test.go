package lb

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkTargets(t *testing.T, weights ...int) []*Target {
	t.Helper()
	out := make([]*Target, len(weights))
	for i, w := range weights {
		u, _ := url.Parse("http://backend-" + string(rune('a'+i)))
		out[i] = NewTarget(u, w, nil, PassiveConfig{})
	}
	return out
}

func countPicks(b Balancer, n int) map[*Target]int {
	got := map[*Target]int{}
	for i := 0; i < n; i++ {
		got[b.Pick(nil)]++
	}
	return got
}

func TestRoundRobinEven(t *testing.T) {
	ts := mkTargets(t, 1, 1, 1)
	b, _ := NewBalancer(RoundRobin, ts)
	got := countPicks(b, 300)
	for _, tg := range ts {
		if got[tg] != 100 {
			t.Fatalf("%v picked %d times, want 100", tg, got[tg])
		}
	}
}

func TestSkipsUnavailable(t *testing.T) {
	for _, policy := range []string{RoundRobin, LeastConnections, WeightedRoundRobin} {
		t.Run(policy, func(t *testing.T) {
			ts := mkTargets(t, 1, 2, 3)
			ts[0].SetHealthy(false)
			ts[1].ejectedUntil.Store(time.Now().Add(time.Hour).UnixNano())
			b, _ := NewBalancer(policy, ts)
			for i := 0; i < 50; i++ {
				if p := b.Pick(nil); p != ts[2] {
					t.Fatalf("picked %v, want only %v", p, ts[2])
				}
			}
			ts[2].SetHealthy(false)
			if p := b.Pick(nil); p != nil {
				t.Fatalf("picked %v with nothing selectable", p)
			}
		})
	}
}

func TestExcludePrefersUntried(t *testing.T) {
	for _, policy := range []string{RoundRobin, LeastConnections, WeightedRoundRobin} {
		t.Run(policy, func(t *testing.T) {
			ts := mkTargets(t, 1, 1)
			b, _ := NewBalancer(policy, ts)
			for i := 0; i < 20; i++ {
				if p := b.Pick([]*Target{ts[0]}); p != ts[1] {
					t.Fatalf("picked %v, want untried %v", p, ts[1])
				}
			}
			// Only an excluded one left: fall back to it rather than fail.
			ts[1].SetHealthy(false)
			if p := b.Pick([]*Target{ts[0]}); p != ts[0] {
				t.Fatalf("picked %v, want fallback %v", p, ts[0])
			}
		})
	}
}

func TestWeightedDistributionAndInterleave(t *testing.T) {
	ts := mkTargets(t, 5, 1, 1)
	b, _ := NewBalancer(WeightedRoundRobin, ts)
	got := countPicks(b, 700)
	want := []int{500, 100, 100}
	for i, tg := range ts {
		if got[tg] != want[i] {
			t.Fatalf("target %d picked %d, want %d", i, got[tg], want[i])
		}
	}
	// Smooth WRR for 5,1,1 is a a b a c a a — never 5 a's in a row.
	w := b.(*weighted)
	run, maxRun := 0, 0
	for _, s := range append(w.schedule, w.schedule...) {
		if s == ts[0] {
			run++
		} else {
			run = 0
		}
		if run > maxRun {
			maxRun = run
		}
	}
	if maxRun > 4 {
		t.Fatalf("heavy target picked %d times in a row; schedule not smooth", maxRun)
	}
}

func TestLeastConnPicksIdle(t *testing.T) {
	ts := mkTargets(t, 1, 1, 1)
	b, _ := NewBalancer(LeastConnections, ts)
	ts[0].Acquire()
	ts[0].Acquire()
	ts[2].Acquire()
	for i := 0; i < 10; i++ {
		if p := b.Pick(nil); p != ts[1] {
			t.Fatalf("picked %v, want idle %v", p, ts[1])
		}
	}
	ts[0].Release()
	ts[0].Release()
	ts[2].Release()
	got := countPicks(b, 300) // all idle: ties rotate
	for _, tg := range ts {
		if got[tg] != 100 {
			t.Fatalf("ties not spread: %v picked %d", tg, got[tg])
		}
	}
}

func TestUnknownPolicy(t *testing.T) {
	if _, err := NewBalancer("random", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestPassiveEjection(t *testing.T) {
	ts := mkTargets(t, 1)
	tg := ts[0]
	now := time.Unix(1000, 0)
	tg.now = func() time.Time { return now }
	tg.passive = PassiveConfig{ConsecutiveFailures: 3, EjectDuration: 10 * time.Second}

	tg.ObserveResult(false)
	tg.ObserveResult(false)
	tg.ObserveResult(true) // resets the streak
	tg.ObserveResult(false)
	tg.ObserveResult(false)
	if tg.Ejected() {
		t.Fatal("ejected without 3 consecutive failures")
	}
	tg.ObserveResult(false)
	if !tg.Ejected() || tg.Selectable() {
		t.Fatal("not ejected after 3 consecutive failures")
	}
	now = now.Add(10 * time.Second)
	if tg.Ejected() {
		t.Fatal("still ejected after eject duration")
	}
}

func TestActiveHealthCheck(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probed %s", r.URL.Path)
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	tg := NewTarget(u, 1, nil, PassiveConfig{})

	var mu sync.Mutex
	var events []bool
	hc := NewHealthChecker(HealthConfig{
		Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second,
		UnhealthyThreshold: 2, HealthyThreshold: 2,
	}, []*Target{tg}, func(_ *Target, h bool) {
		mu.Lock()
		events = append(events, h)
		mu.Unlock()
	})
	hc.Start()
	defer hc.Stop()

	waitFor(t, func() bool { return tg.Healthy() })
	up.Store(false)
	waitFor(t, func() bool { return !tg.Healthy() })
	up.Store(true)
	waitFor(t, func() bool { return tg.Healthy() })

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] || !events[1] {
		t.Fatalf("events = %v, want [false true]", events)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func BenchmarkPick(b *testing.B) {
	for _, policy := range []string{RoundRobin, LeastConnections, WeightedRoundRobin} {
		b.Run(policy, func(b *testing.B) {
			ts := make([]*Target, 8)
			for i := range ts {
				u, _ := url.Parse("http://x")
				ts[i] = NewTarget(u, i+1, nil, PassiveConfig{})
			}
			bal, _ := NewBalancer(policy, ts)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					bal.Pick(nil)
				}
			})
		})
	}
}
