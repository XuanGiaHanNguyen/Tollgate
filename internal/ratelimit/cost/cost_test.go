package cost

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

const (
	day    = 24 * time.Hour
	fiveUSD = 5_000_000 // $5.00
	ttl    = 2 * time.Minute
)

var k = Key{1, "alice"}

func TestReserveSettleRefundAndOverage(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	ok, bal := s.Reserve("r1", k, 1_000_000, fiveUSD, day, ttl)
	if !ok || bal != 4_000_000 {
		t.Fatalf("reserve: ok=%v bal=%d", ok, bal)
	}
	// Real cost was lower: refund the difference.
	if !s.Settle("r1", k, 250_000) {
		t.Fatal("settle not applied")
	}
	if b, _ := s.Balance(k); b != 4_750_000 {
		t.Fatalf("after refund balance=%d", b)
	}
	// Real cost was higher: charge the overage.
	s.Reserve("r2", k, 100_000, fiveUSD, day, ttl)
	s.Settle("r2", k, 900_000)
	if b, _ := s.Balance(k); b != 3_850_000 {
		t.Fatalf("after overage balance=%d", b)
	}
}

func TestDenyWhenBudgetCannotCover(t *testing.T) {
	s := New((&clock{}).now)
	if ok, _ := s.Reserve("big", k, 6_000_000, fiveUSD, day, ttl); ok {
		t.Fatal("reserved more than the fiveUSD")
	}
	if ok, _ := s.Reserve("ok", k, 5_000_000, fiveUSD, day, ttl); !ok {
		t.Fatal("could not reserve the whole fiveUSD")
	}
	if ok, _ := s.Reserve("next", k, 1, fiveUSD, day, ttl); ok {
		t.Fatal("reserved from an empty fiveUSD")
	}
	if st := s.Stats(); st.Denied != 2 || st.Reserved != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestIdempotentReserveAndSettle(t *testing.T) {
	s := New((&clock{}).now)
	s.Reserve("r", k, 1_000_000, fiveUSD, day, ttl)
	if ok, bal := s.Reserve("r", k, 1_000_000, fiveUSD, day, ttl); !ok || bal != 4_000_000 {
		t.Fatalf("retried reserve took twice: ok=%v bal=%d", ok, bal)
	}
	if !s.Settle("r", k, 500_000) || s.Settle("r", k, 500_000) {
		t.Fatal("settle not idempotent")
	}
	if b, _ := s.Balance(k); b != 4_500_000 {
		t.Fatalf("balance %d", b)
	}
	if s.Settle("unknown", k, 1) || s.Settle("r", Key{9, "nobody"}, 1) {
		t.Fatal("unknown reservation applied")
	}
}

func TestExpiryReleasesAndLateSettleCharges(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	var expiredAmt int64
	s.OnExpire = func(_ Key, a int64) { expiredAmt += a }
	s.Reserve("crashed", k, 2_000_000, fiveUSD, day, ttl)
	c.advance(ttl - time.Second)
	if s.Expire() != 0 {
		t.Fatal("expired early")
	}
	c.advance(time.Second)
	if s.Expire() != 1 || expiredAmt != 2_000_000 || s.Stats().Active != 0 {
		t.Fatalf("not expired: %+v", s.Stats())
	}
	b, _ := s.Balance(k)
	if b < 5_000_000-1 { // released (plus a sliver of refill, capped at $5)
		t.Fatalf("hold not released: %d", b)
	}
	// The stream was only slow, not dead: its late settle still pays.
	if !s.Settle("crashed", k, 700_000) {
		t.Fatal("late settle ignored")
	}
	if b2, _ := s.Balance(k); b2 != b-700_000 {
		t.Fatalf("late settle charged %d, want 700000", b-b2)
	}
	if s.Settle("crashed", k, 700_000) {
		t.Fatal("late settle applied twice")
	}
}

func TestRefillOverTime(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	s.Reserve("all", k, fiveUSD, fiveUSD, day, ttl)
	s.Settle("all", k, fiveUSD)
	c.advance(12 * time.Hour)
	if b, _ := s.Balance(k); b < 2_499_999 || b > 2_500_001 {
		t.Fatalf("half-day refill = %d, want ~2.5M", b)
	}
	c.advance(48 * time.Hour)
	if b, _ := s.Balance(k); b != fiveUSD {
		t.Fatalf("refill exceeded capacity: %d", b)
	}
}

// No leaks: whatever happens, after every deadline passes, nothing is held.
func TestNoLeakUnderConcurrency(t *testing.T) {
	c := &clock{}
	s := New(c.now)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := fmt.Sprintf("%d-%d", g, i)
				if ok, _ := s.Reserve(id, k, 1000, 1<<40, day, ttl); ok && i%3 != 0 { // a third "crash"
					s.Settle(id, k, 900)
				}
			}
		}(g)
	}
	wg.Wait()
	c.advance(ttl)
	s.Expire()
	if st := s.Stats(); st.Active != 0 || st.Reserved != st.Settled+st.Expired {
		t.Fatalf("leaked reservations: %+v", st)
	}
}
