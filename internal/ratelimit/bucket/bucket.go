// Package bucket implements local rate-limit state keyed by (limit ID, key):
// token bucket (default) and sliding-window counter. State is sharded by key
// hash, each shard with its own mutex, so there is no global lock on the
// request path.
package bucket

import (
	"hash/maphash"
	"sync"
	"time"
)

const shards = 64

// Algorithm selects how a limit is enforced.
type Algorithm uint8

const (
	TokenBucket Algorithm = iota
	SlidingWindow
)

func (a Algorithm) String() string {
	if a == SlidingWindow {
		return "sliding_window"
	}
	return "token_bucket"
}

// Key identifies one limit state.
type Key struct {
	LimitID int
	Value   string
}

type state struct {
	algo  Algorithm
	count float64
	per   int64 // nanoseconds

	// token bucket
	tokens float64
	last   int64 // unix nanos of last refill

	// sliding window: counts in the current and previous fixed windows
	winStart int64
	curr     float64
	prev     float64
}

type shard struct {
	mu sync.Mutex
	m  map[Key]*state
}

// Store holds every limit state.
type Store struct {
	shards [shards]shard
	seed   maphash.Seed
	now    func() time.Time
}

// New returns an empty store. now may be nil (time.Now).
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{seed: maphash.MakeSeed(), now: now}
	for i := range s.shards {
		s.shards[i].m = map[Key]*state{}
	}
	return s
}

func (s *Store) shard(k Key) *shard {
	return &s.shards[maphash.String(s.seed, k.Value)%shards]
}

// Allow admits one request for k against count per `per` using a token
// bucket (a new bucket starts full, so it allows a burst of count).
func (s *Store) Allow(k Key, count int64, per time.Duration) bool {
	return s.AllowAlgo(k, count, per, TokenBucket)
}

// AllowAlgo admits one request for k against count per `per` using algo.
func (s *Store) AllowAlgo(k Key, count int64, per time.Duration, algo Algorithm) bool {
	if count <= 0 || per <= 0 {
		return false
	}
	now := s.now().UnixNano()
	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	st, ok := sh.m[k]
	if !ok || st.algo != algo {
		// New key, or the limit's algorithm changed on a rule reload.
		st = &state{algo: algo, tokens: float64(count), last: now, winStart: now - now%int64(per)}
		sh.m[k] = st
	}
	st.count, st.per = float64(count), int64(per) // limits may change on reload
	if algo == SlidingWindow {
		return st.allowWindow(now)
	}
	return st.allowToken(now)
}

func (st *state) allowToken(now int64) bool {
	if elapsed := now - st.last; elapsed > 0 {
		st.tokens += float64(elapsed) * st.count / float64(st.per)
		if st.tokens > st.count {
			st.tokens = st.count
		}
		st.last = now
	}
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

// allowWindow estimates the rate over the last `per` as the current fixed
// window's count plus the previous window's count weighted by how much of it
// still overlaps the sliding window.
func (st *state) allowWindow(now int64) bool {
	st.roll(now)
	weight := 1 - float64(now-st.winStart)/float64(st.per)
	if st.prev*weight+st.curr >= st.count {
		return false
	}
	st.curr++
	return true
}

func (st *state) roll(now int64) {
	switch elapsed := now - st.winStart; {
	case elapsed >= 2*st.per:
		st.prev, st.curr = 0, 0
		st.winStart = now - now%st.per
	case elapsed >= st.per:
		st.prev, st.curr = st.curr, 0
		st.winStart += st.per
	}
}

// idle reports whether the state carries no information (a fresh state
// would decide identically), so it can be dropped.
func (st *state) idle(now int64) bool {
	if st.algo == SlidingWindow {
		return now-st.winStart >= 2*st.per
	}
	return st.tokens+float64(now-st.last)*st.count/float64(st.per) >= st.count
}

// Sweep drops states that have fully recovered: they behave exactly like new
// ones, so forgetting them is free. Returns how many were removed.
func (s *Store) Sweep() int {
	now := s.now().UnixNano()
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, st := range sh.m {
			if st.idle(now) {
				delete(sh.m, k)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// Len reports the number of states (tests, metrics).
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		n += len(s.shards[i].m)
		s.shards[i].mu.Unlock()
	}
	return n
}
