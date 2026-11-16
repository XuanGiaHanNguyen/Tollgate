// Package cost holds money budgets with reserve-and-settle, like a credit
// card hold. Before a model call the gateway reserves the estimated cost;
// when the stream ends it settles the real cost, refunding the unused part
// or charging the overage. Every reservation has a deadline: if the gateway
// crashes or the stream is cut, the owner releases it at the deadline, so
// budgets never leak. Settles are idempotent by reservation ID.
//
// Amounts are micro-USD. A budget is a balance that refills `amount` per
// `per` continuously, up to `amount` (the token-bucket model, in money).
// Overage may drive the balance negative, which blocks new reservations
// until it refills.
package cost

import (
	"hash/maphash"
	"sync"
	"time"
)

const shards = 64

// Key identifies one budget: the rule's budget ID and the key value
// (e.g. the user ID).
type Key struct {
	BudgetID int
	Value    string
}

type resState uint8

const (
	active resState = iota
	settled
	expired
	denied
)

type reservation struct {
	amount   int64
	deadline int64 // unix nanos
	state    resState
	until    int64 // tombstone retention for settled/expired/denied
}

type budget struct {
	balance float64 // micro-USD; may be negative after overage
	last    int64
	amount  float64
	per     int64
	resv    map[string]*reservation
}

type shard struct {
	mu sync.Mutex
	m  map[Key]*budget
}

// Stats are cumulative counters (for metrics and the leak check).
type Stats struct {
	Reserved, Settled, Expired, Denied int64 // reservations by outcome
	ReservedMicro, SettledMicro        int64
	Active                             int64 // reservations neither settled nor expired
}

// Store holds budgets for the keys this node owns.
type Store struct {
	shards [shards]shard
	seed   maphash.Seed
	now    func() time.Time
	keep   time.Duration // tombstone retention after a reservation ends

	statsMu sync.Mutex
	stats   Stats
	// OnExpire is called for each reservation released at its deadline.
	OnExpire func(Key, int64)
}

// New returns an empty store. now may be nil.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{seed: maphash.MakeSeed(), now: now, keep: time.Hour}
	for i := range s.shards {
		s.shards[i].m = map[Key]*budget{}
	}
	return s
}

func (s *Store) shard(k Key) *shard { return &s.shards[maphash.String(s.seed, k.Value)%shards] }

func (s *Store) get(sh *shard, k Key, amount int64, per time.Duration, now int64) *budget {
	b, ok := sh.m[k]
	if !ok {
		b = &budget{balance: float64(amount), last: now, resv: map[string]*reservation{}}
		sh.m[k] = b
	}
	b.amount, b.per = float64(amount), int64(per)
	if el := now - b.last; el > 0 && b.per > 0 {
		b.balance += float64(el) * b.amount / float64(b.per)
		if b.balance > b.amount {
			b.balance = b.amount
		}
		b.last = now
	}
	return b
}

func (s *Store) count(f func(*Stats)) {
	s.statsMu.Lock()
	f(&s.stats)
	s.statsMu.Unlock()
}

// Stats returns a snapshot of the counters.
func (s *Store) Stats() Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// Reserve holds estimate against the budget for k if the balance covers it.
// Calling it again with the same id returns the first answer (a retried RPC
// never double-reserves). balance is what remains after the call.
func (s *Store) Reserve(id string, k Key, estimate, amount int64, per, ttl time.Duration) (ok bool, balance int64) {
	now := s.now().UnixNano()
	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := s.get(sh, k, amount, per, now)
	if r, seen := b.resv[id]; seen {
		return r.state != denied, int64(b.balance)
	}
	if estimate < 0 {
		estimate = 0
	}
	if b.balance < float64(estimate) {
		b.resv[id] = &reservation{state: denied, until: now + int64(s.keep)}
		s.count(func(st *Stats) { st.Denied++ })
		return false, int64(b.balance)
	}
	b.balance -= float64(estimate)
	b.resv[id] = &reservation{amount: estimate, deadline: now + int64(ttl), state: active}
	s.count(func(st *Stats) { st.Reserved++; st.Active++; st.ReservedMicro += estimate })
	return true, int64(b.balance)
}

// Settle charges the real cost for a reservation. It refunds the unused
// reservation or charges the overage. A second settle for the same id is a
// no-op (applied=false). A settle that arrives after the reservation expired
// (its hold was already released) charges actual in full, so a slow stream
// is still paid for. An unknown id is ignored.
func (s *Store) Settle(id string, k Key, actual int64) (applied bool) {
	now := s.now().UnixNano()
	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b, ok := sh.m[k]
	if !ok {
		return false
	}
	s.get(sh, k, int64(b.amount), time.Duration(b.per), now) // refill
	r, ok := b.resv[id]
	if !ok {
		return false
	}
	if actual < 0 {
		actual = 0
	}
	switch r.state {
	case active:
		b.balance += float64(r.amount - actual)
		s.count(func(st *Stats) { st.Settled++; st.Active--; st.SettledMicro += actual })
	case expired:
		b.balance -= float64(actual)
		s.count(func(st *Stats) { st.Settled++; st.SettledMicro += actual })
	default: // already settled, or denied
		return false
	}
	r.state, r.until = settled, now+int64(s.keep)
	return true
}

// Expire releases every active reservation past its deadline and drops old
// tombstones. Returns how many reservations were released.
func (s *Store) Expire() int {
	now := s.now().UnixNano()
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, b := range sh.m {
			for id, r := range b.resv {
				switch {
				case r.state == active && now >= r.deadline:
					b.balance += float64(r.amount)
					r.state, r.until = expired, now+int64(s.keep)
					n++
					s.count(func(st *Stats) { st.Expired++; st.Active-- })
					if s.OnExpire != nil {
						s.OnExpire(k, r.amount)
					}
				case r.state != active && now >= r.until:
					delete(b.resv, id)
				}
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// Run calls Expire every interval until stop is closed.
func (s *Store) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.Expire()
		}
	}
}

// Balance reports a budget's current balance (tests, admin).
func (s *Store) Balance(k Key) (int64, bool) {
	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b, ok := sh.m[k]
	if !ok {
		return 0, false
	}
	s.get(sh, k, int64(b.amount), time.Duration(b.per), s.now().UnixNano())
	return int64(b.balance), true
}
