// Package rollout runs canary rollouts of rule changes: a candidate first
// goes to one canary gateway; the controller compares the canary's error
// rate and p99 latency with the other gateways and either promotes the
// candidate to every gateway or rolls it back. Every decision is a
// guarded transaction in the Raft log, so rollout history survives leader
// failover and a deposed leader can never decide twice.
package rollout

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// States of a rollout.
const (
	StateCanary     = "canary"
	StatePromoted   = "promoted"
	StateRolledBack = "rolled_back"
)

// Keys in the config store.
const (
	KeyRules   = "rules"
	KeyRollout = "rollout"
	KeyHistory = "rollouts/" // + id: final record of each rollout
)

// Record is the rollout state stored under KeyRollout.
type Record struct {
	ID        string  `json:"id"`
	State     string  `json:"state"`
	Candidate []byte  `json:"candidate"`
	Canary    string  `json:"canary"`
	StartedTS int64   `json:"started_ts"` // unix nanos, set by the leader
	DecidedTS int64   `json:"decided_ts,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	Report    *Report `json:"report,omitempty"` // the comparison that decided it
}

// Stats summarize traffic on one side of the comparison.
type Stats struct {
	Requests  float64 `json:"requests"`
	Errors    float64 `json:"errors"` // status >= 400
	P99       float64 `json:"p99_seconds"`
	ErrorRate float64 `json:"error_rate"`
}

// Report is what a decision was based on.
type Report struct {
	Canary   Stats  `json:"canary"`
	Baseline Stats  `json:"baseline"`
	Verdict  string `json:"verdict"`
}

// Encode serializes a record.
func (r Record) Encode() []byte { b, _ := json.Marshal(r); return b }

// Decode parses a record.
func Decode(b []byte) (Record, error) {
	var r Record
	err := json.Unmarshal(b, &r)
	return r, err
}

// Policy sets the thresholds.
type Policy struct {
	Window      time.Duration // compare at least this long before promoting
	MaxWait     time.Duration // roll back if the canary has too little traffic by then
	MinRequests float64       // per side, before any verdict
	MaxErrDelta float64       // canary error rate may exceed baseline by this much (absolute)
	MaxP99Ratio float64       // canary p99 may be at most baseline p99 * ratio + P99Slack
	P99Slack    time.Duration
}

// DefaultPolicy are the defaults from DESIGN.md.
func DefaultPolicy() Policy {
	return Policy{Window: 10 * time.Second, MaxWait: 60 * time.Second, MinRequests: 100,
		MaxErrDelta: 0.05, MaxP99Ratio: 1.5, P99Slack: 5 * time.Millisecond}
}

// Verdict of one comparison.
type Verdict int

const (
	Wait Verdict = iota
	Promote
	RollBack
)

func (v Verdict) String() string { return [...]string{"wait", "promote", "roll_back"}[v] }

// Decide compares canary and baseline. compared is how long this leader
// has been comparing (promotion needs a full Window of it); sinceStart is
// how long ago the rollout began, across failovers (for MaxWait). A breach
// rolls back as soon as both sides have enough traffic.
func Decide(p Policy, canary, base Stats, compared, sinceStart time.Duration) (Verdict, string) {
	if canary.Requests < p.MinRequests || base.Requests < p.MinRequests {
		if sinceStart >= p.MaxWait {
			return RollBack, fmt.Sprintf("insufficient traffic after %v (canary %.0f, baseline %.0f requests; need %.0f each)",
				p.MaxWait, canary.Requests, base.Requests, p.MinRequests)
		}
		return Wait, ""
	}
	if d := canary.ErrorRate - base.ErrorRate; d > p.MaxErrDelta {
		return RollBack, fmt.Sprintf("canary error rate %.1f%% vs baseline %.1f%% (allowed +%.1f pts)",
			100*canary.ErrorRate, 100*base.ErrorRate, 100*p.MaxErrDelta)
	}
	limit := base.P99*p.MaxP99Ratio + p.P99Slack.Seconds()
	if canary.P99 > limit {
		return RollBack, fmt.Sprintf("canary p99 %.1fms vs baseline %.1fms (limit %.1fms)",
			1000*canary.P99, 1000*base.P99, 1000*limit)
	}
	if compared >= p.Window {
		return Promote, fmt.Sprintf("canary within thresholds for %v (error rate %.1f%% vs %.1f%%, p99 %.1fms vs %.1fms)",
			p.Window, 100*canary.ErrorRate, 100*base.ErrorRate, 1000*canary.P99, 1000*base.P99)
	}
	return Wait, ""
}

// Finish fills derived fields of Stats.
func (s *Stats) Finish() {
	if s.Requests > 0 {
		s.ErrorRate = s.Errors / s.Requests
	}
	if math.IsNaN(s.P99) {
		s.P99 = 0
	}
}
