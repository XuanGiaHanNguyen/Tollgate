// Package ratelimit enforces limits cluster-wide without a central store:
// each gateway owns the keys the hash ring assigns to it and answers peers'
// checks for them over gRPC. When an owner cannot be reached, the gateway
// falls back to a local bucket with the limit divided by the node count and
// counts the decision as degraded.
package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	tollgatev1 "github.com/XuanGiaHanNguyen/Tollgate/api/gen/tollgate/v1"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/breaker"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/bucket"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/peer"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/ring"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// Decision outcomes, matching tollgate_ratelimit_decisions_total.
const (
	ResultAllow    = "allow"
	ResultDeny     = "deny"
	ResultDegraded = "degraded"
)

// Member is one gateway in the cluster.
type Member struct {
	Name string
	Addr string // gRPC address peers dial
}

// Config for a cluster limiter.
type Config struct {
	Self    string
	Members []Member // all gateways, including Self
	VNodes  int
	Peer    peer.Config
	Hooks   peer.Hooks
	// OnDecision is called once per check with ResultAllow/Deny/Degraded.
	OnDecision func(result string)
}

type peerConn struct {
	client  *peer.Client
	breaker *breaker.Breaker // open: skip the peer and fall back at once
}

// Limiter decides limits locally or on the key's owner.
type Limiter struct {
	self       string
	ring       atomic.Pointer[ring.Ring]
	local      *bucket.Store // keys this node owns (also served to peers)
	fallback   *bucket.Store // degraded decisions for unreachable owners
	peers      map[string]*peerConn
	onDecision func(string)
}

// NewLocal returns a single-node limiter (no cluster configured).
func NewLocal(onDecision func(string)) *Limiter {
	l := &Limiter{local: bucket.New(nil), fallback: bucket.New(nil), peers: map[string]*peerConn{}, onDecision: onDecision}
	r, _ := ring.New(nil, 0)
	l.ring.Store(r)
	return l
}

// New builds a cluster limiter and dials every peer.
func New(cfg Config) (*Limiter, error) {
	names := make([]string, 0, len(cfg.Members))
	selfFound := false
	for _, m := range cfg.Members {
		names = append(names, m.Name)
		selfFound = selfFound || m.Name == cfg.Self
	}
	if !selfFound {
		return nil, fmt.Errorf("ratelimit: self %q is not a cluster member", cfg.Self)
	}
	r, err := ring.New(names, cfg.VNodes)
	if err != nil {
		return nil, err
	}
	l := &Limiter{self: cfg.Self, local: bucket.New(nil), fallback: bucket.New(nil),
		peers: map[string]*peerConn{}, onDecision: cfg.OnDecision}
	l.ring.Store(r)
	for _, m := range cfg.Members {
		if m.Name == cfg.Self {
			continue
		}
		c, err := peer.Dial(m.Name, m.Addr, cfg.Peer, cfg.Hooks)
		if err != nil {
			l.Close()
			return nil, err
		}
		l.peers[m.Name] = &peerConn{client: c, breaker: breaker.New(breaker.Config{
			Window: 5 * time.Second, MinRequests: 5, ErrorRate: 0.5, Cooldown: time.Second, HalfOpenProbes: 1,
		})}
	}
	return l, nil
}

// Close shuts down peer connections.
func (l *Limiter) Close() {
	for _, p := range l.peers {
		p.client.Close()
	}
}

// Local is the store for keys this node owns; the gRPC server serves it.
func (l *Limiter) Local() *bucket.Store { return l.local }

// Owner reports which member owns a limit key (tests, tools).
func (l *Limiter) Owner(limitID int, key string) string {
	return l.ring.Load().Owner(ringKey(limitID, key))
}

func ringKey(limitID int, key string) string { return strconv.Itoa(limitID) + "|" + key }

func algo(a uint8) bucket.Algorithm {
	if a == env.AlgoSlidingWindow {
		return bucket.SlidingWindow
	}
	return bucket.TokenBucket
}

// Allow applies one limit. degraded reports a fallback decision.
func (l *Limiter) Allow(ctx context.Context, lr env.LimitReq) (allowed, degraded bool) {
	k := bucket.Key{LimitID: lr.ID, Value: lr.Key}
	a := algo(lr.Algo)
	r := l.ring.Load()
	owner := ""
	if r.Len() > 0 {
		owner = r.Owner(ringKey(lr.ID, lr.Key))
	}
	if owner == "" || owner == l.self {
		allowed = l.local.AllowAlgo(k, lr.Count, lr.Per, a)
		l.decided(allowed, false)
		return allowed, false
	}

	p := l.peers[owner]
	if p != nil && p.breaker.Allow() {
		ok, err := p.client.Check(ctx, &tollgatev1.Check{
			LimitId: int32(lr.ID), Key: lr.Key, Count: lr.Count, PerNs: int64(lr.Per), Algorithm: peer.AlgoToProto(a),
		})
		if err == nil || ctx.Err() == nil {
			p.breaker.Record(err == nil)
		} else {
			p.breaker.Ignore() // the client went away; not the peer's fault
		}
		if err == nil {
			l.decided(ok, false)
			return ok, false
		}
	}

	// Owner unreachable: enforce a share of the limit locally.
	share := lr.Count / int64(r.Len())
	if share < 1 {
		share = 1
	}
	allowed = l.fallback.AllowAlgo(k, share, lr.Per, a)
	l.decided(allowed, true)
	return allowed, true
}

func (l *Limiter) decided(allowed, degraded bool) {
	if l.onDecision == nil {
		return
	}
	switch {
	case degraded:
		l.onDecision(ResultDegraded)
	case allowed:
		l.onDecision(ResultAllow)
	default:
		l.onDecision(ResultDeny)
	}
}

// Sweep drops fully recovered state from both stores.
func (l *Limiter) Sweep() int { return l.local.Sweep() + l.fallback.Sweep() }
