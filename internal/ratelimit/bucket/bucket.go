// Package bucket implements token-bucket rate limiting keyed by (limit ID,
// key). State is sharded by key hash, each shard with its own mutex, so
// there is no global lock on the request path.
//
// M3 uses it as the local limiter; M4 adds the sliding window and puts it
// behind the distributed hash ring.
package bucket

import (
	"hash/maphash"
	"sync"
	"time"
)

const shards = 64

// Key identifies one bucket.
type Key struct {
	LimitID int
	Value   string
}

type state struct {
	tokens    float64
	last      int64 // unix nanos of last refill
	capacity  float64
	ratePerNs float64
}

type shard struct {
	mu sync.Mutex
	m  map[Key]*state
}

// Store holds every bucket.
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

// Allow takes one token from the bucket for k, which holds up to count
// tokens and refills count tokens per `per`. A new bucket starts full.
func (s *Store) Allow(k Key, count int64, per time.Duration) bool {
	if count <= 0 || per <= 0 {
		return false
	}
	now := s.now().UnixNano()
	capacity := float64(count)
	ratePerNs := capacity / float64(per)

	sh := s.shard(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	st, ok := sh.m[k]
	if !ok {
		st = &state{tokens: capacity, last: now, capacity: capacity, ratePerNs: ratePerNs}
		sh.m[k] = st
	} else if elapsed := now - st.last; elapsed > 0 {
		st.tokens += float64(elapsed) * ratePerNs
		if st.tokens > capacity {
			st.tokens = capacity
		}
		st.last = now
		st.capacity, st.ratePerNs = capacity, ratePerNs // limit may change on reload
	}
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

// Sweep drops buckets that have refilled to capacity: they behave exactly
// like a new bucket, so forgetting them is free. Returns how many were removed.
func (s *Store) Sweep() int {
	now := s.now().UnixNano()
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, st := range sh.m {
			if st.tokens+float64(now-st.last)*st.ratePerNs >= st.capacity {
				delete(sh.m, k)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// Len reports the number of buckets (tests, metrics).
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		n += len(s.shards[i].m)
		s.shards[i].mu.Unlock()
	}
	return n
}
