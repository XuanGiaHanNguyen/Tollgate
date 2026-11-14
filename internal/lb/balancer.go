package lb

import (
	"fmt"
	"sync/atomic"
)

// Balancer picks a target. exclude lists targets already tried for this
// request; a balancer prefers others but may return an excluded target if it
// is the only selectable one. Pick returns nil when nothing is selectable.
type Balancer interface {
	Pick(exclude []*Target) *Target
}

// Policy names accepted in config.
const (
	RoundRobin         = "round_robin"
	LeastConnections   = "least_conn"
	WeightedRoundRobin = "weighted_round_robin"
)

// NewBalancer builds the balancer for a policy name.
func NewBalancer(policy string, targets []*Target) (Balancer, error) {
	switch policy {
	case "", RoundRobin:
		return &roundRobin{targets: targets}, nil
	case LeastConnections:
		return &leastConn{targets: targets}, nil
	case WeightedRoundRobin:
		return newWeighted(targets), nil
	}
	return nil, fmt.Errorf("unknown balancer %q", policy)
}

func excluded(t *Target, exclude []*Target) bool {
	for _, e := range exclude {
		if e == t {
			return true
		}
	}
	return false
}

// pickFrom walks n candidates produced by at(i) from a start offset and
// returns the first selectable non-excluded one, else the first selectable one.
func pickFrom(n int, start uint64, at func(i int) *Target, exclude []*Target) *Target {
	var fallback *Target
	for i := 0; i < n; i++ {
		t := at(int((start + uint64(i)) % uint64(n)))
		if !t.Selectable() {
			continue
		}
		if !excluded(t, exclude) {
			return t
		}
		if fallback == nil {
			fallback = t
		}
	}
	return fallback
}

type roundRobin struct {
	targets []*Target
	next    atomic.Uint64
}

func (r *roundRobin) Pick(exclude []*Target) *Target {
	n := len(r.targets)
	if n == 0 {
		return nil
	}
	start := r.next.Add(1) - 1
	return pickFrom(n, start, func(i int) *Target { return r.targets[i] }, exclude)
}

type leastConn struct {
	targets []*Target
	next    atomic.Uint64 // rotating start so ties spread out
}

func (l *leastConn) Pick(exclude []*Target) *Target {
	n := len(l.targets)
	if n == 0 {
		return nil
	}
	start := l.next.Add(1) - 1
	var best, fallback *Target
	for i := 0; i < n; i++ {
		t := l.targets[(start+uint64(i))%uint64(n)]
		if !t.Selectable() {
			continue
		}
		if excluded(t, exclude) {
			if fallback == nil || t.Inflight() < fallback.Inflight() {
				fallback = t
			}
			continue
		}
		if best == nil || t.Inflight() < best.Inflight() {
			best = t
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// weighted walks a precomputed smooth-weighted-round-robin schedule (the
// nginx algorithm run once at build time), so picks are lock-free.
type weighted struct {
	schedule []*Target
	next     atomic.Uint64
}

func newWeighted(targets []*Target) *weighted {
	total := 0
	for _, t := range targets {
		total += t.Weight
	}
	current := make([]int, len(targets))
	schedule := make([]*Target, 0, total)
	for len(schedule) < total {
		best := -1
		for i, t := range targets {
			current[i] += t.Weight
			if best < 0 || current[i] > current[best] {
				best = i
			}
		}
		current[best] -= total
		schedule = append(schedule, targets[best])
	}
	return &weighted{schedule: schedule}
}

func (w *weighted) Pick(exclude []*Target) *Target {
	n := len(w.schedule)
	if n == 0 {
		return nil
	}
	start := w.next.Add(1) - 1
	return pickFrom(n, start, func(i int) *Target { return w.schedule[i] }, exclude)
}
