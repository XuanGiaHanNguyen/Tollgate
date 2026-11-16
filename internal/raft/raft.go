// Package raft implements the Raft consensus algorithm (Ongaro and
// Ousterhout, 2014) as a deterministic state machine: no goroutines, clock,
// network or disk. A driver feeds it ticks and messages, and acts on the
// Ready it produces: persist the hard state and new entries (fsync) before
// sending that Ready's messages, then apply committed entries. The same
// core runs under the real server and under the deterministic simulator.
//
// Membership is fixed at construction.
package raft

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sort"
)

// StateType is a node's role.
type StateType int

const (
	Follower StateType = iota
	Candidate
	Leader
	PreCandidate
)

func (s StateType) String() string {
	return [...]string{"follower", "candidate", "leader", "pre-candidate"}[s]
}

// MsgType identifies a message.
type MsgType int

const (
	MsgVote MsgType = iota
	MsgVoteResp
	MsgApp
	MsgAppResp
	MsgHeartbeat
	MsgHeartbeatResp
	MsgSnap
	MsgPreVote
	MsgPreVoteResp
)

func (t MsgType) String() string {
	return [...]string{"Vote", "VoteResp", "App", "AppResp", "Heartbeat", "HeartbeatResp", "Snap", "PreVote", "PreVoteResp"}[t]
}

// Entry is one log entry. Data == nil marks the leader's no-op.
type Entry struct {
	Term, Index uint64
	Data        []byte
}

// Snapshot is the application state up to and including Index.
type Snapshot struct {
	Index, Term uint64
	Data        []byte
}

// HardState must be persisted before messages that depend on it are sent.
type HardState struct {
	Term, Vote, Commit uint64
}

// Message is exchanged between nodes.
type Message struct {
	Type     MsgType
	From, To uint64
	Term     uint64
	// MsgApp: index and term of the entry before Entries.
	// MsgVote: the candidate's last log index and term.
	// MsgAppResp: highest matching index on success; the rejected prev index otherwise.
	Index, LogTerm uint64
	Entries        []Entry
	Commit         uint64
	Reject         bool
	RejectHint     uint64 // MsgAppResp reject: the follower's last index
	Snapshot       *Snapshot
	Context        []byte // MsgHeartbeat(Resp): ReadIndex request id
}

// ReadState confirms a linearizable read: once the application has applied
// Index, it may serve the read identified by Context.
type ReadState struct {
	Index   uint64
	Context []byte
}

// Ready is the work a driver must do, in this order: persist HardState,
// Snapshot and Entries (fsync); send Messages; restore Snapshot into the
// application if set; apply Committed; serve ReadStates; call Advance.
type Ready struct {
	HardState        HardState
	HardStateChanged bool
	Snapshot         *Snapshot // received from the leader: persist and restore
	Entries          []Entry   // new log entries to persist
	Messages         []Message
	Committed        []Entry
	ReadStates       []ReadState
}

// Empty reports whether there is nothing to do.
func (rd *Ready) Empty() bool {
	return !rd.HardStateChanged && rd.Snapshot == nil && len(rd.Entries) == 0 &&
		len(rd.Messages) == 0 && len(rd.Committed) == 0 && len(rd.ReadStates) == 0
}

// Config configures a node.
type Config struct {
	ID            uint64
	Peers         []uint64 // every member, including ID
	ElectionTick  int      // election timeout is randomized in [ElectionTick, 2*ElectionTick)
	HeartbeatTick int
	Rand          *rand.Rand // election jitter; seeded for determinism
	MaxAppend     int        // entries per MsgApp
	PreVote       bool       // pre-vote round before raising the term (Raft thesis §9.6)
}

// Restored is persisted state to restart from.
type Restored struct {
	HardState HardState
	Snapshot  *Snapshot
	Entries   []Entry // entries after Snapshot.Index
}

// Errors.
var (
	ErrNotLeader = errors.New("raft: not the leader")
	ErrNotReady  = errors.New("raft: leader has not committed an entry in its term yet")
)

type progress struct{ match, next uint64 }

type readReq struct {
	index uint64
	ctx   []byte
	acks  map[uint64]bool
}

// Node is one Raft member.
type Node struct {
	cfg   Config
	id    uint64
	peers []uint64

	term, vote, lead uint64
	state            StateType

	// log: entries[i].Index == snapIndex+1+i
	snap      Snapshot // last compacted snapshot (sent to lagging followers)
	entries   []Entry
	committed uint64
	applied   uint64
	stable    uint64    // highest index handed to the driver to persist
	pendSnap  *Snapshot // received snapshot not yet handed to the driver

	electionElapsed, heartbeatElapsed, electionTimeout int
	votes                                              map[uint64]bool
	prs                                                map[uint64]*progress
	reads                                              []*readReq
	readStates                                         []ReadState
	msgs                                               []Message
	prevHS                                             HardState
}

// New creates a node, restoring persisted state if any.
func New(cfg Config, r Restored) *Node {
	if cfg.ElectionTick <= 0 {
		cfg.ElectionTick = 10
	}
	if cfg.HeartbeatTick <= 0 {
		cfg.HeartbeatTick = 1
	}
	if cfg.MaxAppend <= 0 {
		cfg.MaxAppend = 256
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(cfg.ID, 1))
	}
	n := &Node{cfg: cfg, id: cfg.ID, peers: append([]uint64(nil), cfg.Peers...)}
	sort.Slice(n.peers, func(i, j int) bool { return n.peers[i] < n.peers[j] })
	if r.Snapshot != nil {
		n.snap = *r.Snapshot
	}
	n.entries = append([]Entry(nil), r.Entries...)
	n.term, n.vote, n.committed = r.HardState.Term, r.HardState.Vote, r.HardState.Commit
	n.applied = n.snap.Index
	if n.committed < n.applied {
		n.committed = n.applied
	}
	n.stable = n.lastIndex()
	n.prevHS = r.HardState
	n.becomeFollower(n.term, 0)
	return n
}

// ---- log helpers ----

func (n *Node) lastIndex() uint64 { return n.snap.Index + uint64(len(n.entries)) }

func (n *Node) lastTerm() uint64 { return n.termAt(n.lastIndex()) }

// termAt returns the term of index i, or 0 if unknown (compacted or absent).
func (n *Node) termAt(i uint64) uint64 {
	switch {
	case i == n.snap.Index:
		return n.snap.Term
	case i < n.snap.Index || i > n.lastIndex():
		return 0
	}
	return n.entries[i-n.snap.Index-1].Term
}

func (n *Node) entriesFrom(lo uint64, max int) []Entry {
	if lo <= n.snap.Index || lo > n.lastIndex() {
		return nil
	}
	es := n.entries[lo-n.snap.Index-1:]
	if len(es) > max {
		es = es[:max]
	}
	return slices.Clone(es)
}

// appendFrom writes es (contiguous) into the log, truncating any
// conflicting suffix. Entries at or below the commit index never conflict.
func (n *Node) appendFrom(es []Entry) {
	for i, e := range es {
		if e.Index <= n.snap.Index {
			continue
		}
		if e.Index <= n.lastIndex() {
			if n.termAt(e.Index) == e.Term {
				continue
			}
			// conflict: drop this index and everything after it
			n.entries = n.entries[:e.Index-n.snap.Index-1]
			if n.stable >= e.Index {
				n.stable = e.Index - 1
			}
		}
		n.entries = append(n.entries, es[i:]...)
		return
	}
}

func (n *Node) quorum() int { return len(n.peers)/2 + 1 }

// send queues a message stamped with this node's id and current term.
// Pre-vote requests and granted pre-vote responses carry the proposed
// next term instead (set by the caller), without changing anyone's term.
func (n *Node) send(m Message) {
	m.From = n.id
	if !((m.Type == MsgPreVote || m.Type == MsgPreVoteResp) && m.Term != 0) {
		m.Term = n.term
	}
	n.msgs = append(n.msgs, m)
}

// ---- roles ----

func (n *Node) resetTimers() {
	n.electionElapsed, n.heartbeatElapsed = 0, 0
	n.electionTimeout = n.cfg.ElectionTick + n.cfg.Rand.IntN(n.cfg.ElectionTick)
}

func (n *Node) becomeFollower(term, lead uint64) {
	if term > n.term {
		n.term, n.vote = term, 0
	}
	n.state, n.lead = Follower, lead
	n.reads = nil
	n.resetTimers()
}

func (n *Node) becomeCandidate() {
	n.term++
	n.vote = n.id
	n.state, n.lead = Candidate, 0
	n.votes = map[uint64]bool{n.id: true}
	n.resetTimers()
}

func (n *Node) becomeLeader() {
	n.state, n.lead = Leader, n.id
	n.prs = map[uint64]*progress{}
	for _, p := range n.peers {
		n.prs[p] = &progress{next: n.lastIndex() + 1}
	}
	n.resetTimers()
	// A no-op in the new term lets earlier-term entries commit (§5.4.2)
	// and enables ReadIndex.
	n.appendLocal(Entry{Data: nil})
	n.bcastAppend()
}

// Tick advances logical time by one tick.
func (n *Node) Tick() {
	if n.state == Leader {
		n.heartbeatElapsed++
		if n.heartbeatElapsed >= n.cfg.HeartbeatTick {
			n.heartbeatElapsed = 0
			n.bcastHeartbeat(nil)
		}
		return
	}
	n.electionElapsed++
	if n.electionElapsed >= n.electionTimeout {
		n.campaign()
	}
}

// campaign starts with a pre-vote round (Raft thesis §9.6): a node that
// could not win (stale log) or whose peers still hear from a leader never
// raises the term, so a restarted or partitioned node cannot depose a
// healthy leader.
func (n *Node) campaign() {
	if len(n.peers) == 1 {
		n.becomeCandidate()
		n.becomeLeader()
		return
	}
	if !n.cfg.PreVote {
		n.realCampaign()
		return
	}
	n.state, n.lead = PreCandidate, 0
	n.votes = map[uint64]bool{n.id: true}
	n.resetTimers()
	for _, p := range n.peers {
		if p != n.id {
			n.send(Message{Type: MsgPreVote, To: p, Term: n.term + 1, Index: n.lastIndex(), LogTerm: n.lastTerm()})
		}
	}
}

func (n *Node) realCampaign() {
	n.becomeCandidate()
	for _, p := range n.peers {
		if p != n.id {
			n.send(Message{Type: MsgVote, To: p, Index: n.lastIndex(), LogTerm: n.lastTerm()})
		}
	}
}

// ---- leader replication ----

func (n *Node) appendLocal(es ...Entry) {
	for i := range es {
		es[i].Term = n.term
		es[i].Index = n.lastIndex() + 1 + uint64(i)
	}
	n.entries = append(n.entries, es...)
	n.prs[n.id].match = n.lastIndex()
	n.prs[n.id].next = n.lastIndex() + 1
	n.maybeCommit()
}

func (n *Node) sendAppend(to uint64) {
	pr := n.prs[to]
	if pr.next <= n.snap.Index {
		s := n.snap
		n.send(Message{Type: MsgSnap, To: to, Snapshot: &s})
		pr.next = n.snap.Index + 1 // optimistic; a reject backs it off
		return
	}
	prev := pr.next - 1
	es := n.entriesFrom(pr.next, n.cfg.MaxAppend)
	n.send(Message{Type: MsgApp, To: to, Index: prev, LogTerm: n.termAt(prev), Entries: es, Commit: n.committed})
	if len(es) > 0 {
		pr.next = es[len(es)-1].Index + 1 // optimistic pipelining
	}
}

func (n *Node) bcastAppend() {
	for _, p := range n.peers {
		if p != n.id {
			n.sendAppend(p)
		}
	}
}

func (n *Node) bcastHeartbeat(ctx []byte) {
	for _, p := range n.peers {
		if p != n.id {
			commit := min(n.prs[p].match, n.committed)
			n.send(Message{Type: MsgHeartbeat, To: p, Commit: commit, Context: ctx})
		}
	}
}

// maybeCommit advances the commit index to the highest index replicated on
// a majority, but only for an entry of the current term.
func (n *Node) maybeCommit() bool {
	matches := make([]uint64, 0, len(n.peers))
	for _, p := range n.peers {
		matches = append(matches, n.prs[p].match)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	idx := matches[n.quorum()-1]
	if idx > n.committed && n.termAt(idx) == n.term {
		n.committed = idx
		n.releaseReads()
		return true
	}
	return false
}

// ---- Step ----

// Step processes one message.
func (n *Node) Step(m Message) {
	switch {
	case m.Type == MsgPreVote || (m.Type == MsgPreVoteResp && !m.Reject):
		// Pre-votes never change anyone's term.
	case m.Term > n.term:
		lead := uint64(0)
		if m.Type == MsgApp || m.Type == MsgHeartbeat || m.Type == MsgSnap {
			lead = m.From
		}
		n.becomeFollower(m.Term, lead)
	case m.Term < n.term:
		// A stale leader learns the new term from our response.
		switch m.Type {
		case MsgApp, MsgHeartbeat, MsgSnap:
			n.send(Message{Type: MsgAppResp, To: m.From, Reject: true, Index: m.Index, RejectHint: n.lastIndex()})
		case MsgVote:
			n.send(Message{Type: MsgVoteResp, To: m.From, Reject: true})
		}
		return
	}

	switch m.Type {
	case MsgPreVote:
		// Grant only if the candidate's log is up to date and we have not
		// heard from a leader within an election timeout.
		upToDate := m.LogTerm > n.lastTerm() || (m.LogTerm == n.lastTerm() && m.Index >= n.lastIndex())
		leaderAlive := n.state == Leader || (n.lead != 0 && n.electionElapsed < n.cfg.ElectionTick)
		if m.Term > n.term && upToDate && !leaderAlive {
			n.send(Message{Type: MsgPreVoteResp, To: m.From, Term: m.Term})
		} else {
			n.send(Message{Type: MsgPreVoteResp, To: m.From, Reject: true})
		}

	case MsgPreVoteResp:
		if n.state != PreCandidate {
			return
		}
		n.votes[m.From] = !m.Reject
		granted, rejected := 0, 0
		for _, v := range n.votes {
			if v {
				granted++
			} else {
				rejected++
			}
		}
		if granted >= n.quorum() {
			n.realCampaign()
		} else if rejected >= n.quorum() {
			n.becomeFollower(n.term, 0)
		}

	case MsgVote:
		upToDate := m.LogTerm > n.lastTerm() || (m.LogTerm == n.lastTerm() && m.Index >= n.lastIndex())
		grant := (n.vote == 0 || n.vote == m.From) && upToDate && n.state != Leader
		if grant {
			n.vote = m.From
			n.electionElapsed = 0
		}
		n.send(Message{Type: MsgVoteResp, To: m.From, Reject: !grant})

	case MsgVoteResp:
		if n.state != Candidate {
			return
		}
		n.votes[m.From] = !m.Reject
		granted, rejected := 0, 0
		for _, v := range n.votes {
			if v {
				granted++
			} else {
				rejected++
			}
		}
		if granted >= n.quorum() {
			n.becomeLeader()
		} else if rejected >= n.quorum() {
			n.becomeFollower(n.term, 0)
		}

	case MsgApp:
		if n.state != Follower {
			n.becomeFollower(n.term, m.From)
		}
		n.lead = m.From
		n.electionElapsed = 0
		n.handleAppend(m)

	case MsgHeartbeat:
		if n.state != Follower {
			n.becomeFollower(n.term, m.From)
		}
		n.lead = m.From
		n.electionElapsed = 0
		if m.Commit > n.committed && m.Commit <= n.lastIndex() {
			n.committed = m.Commit
		}
		n.send(Message{Type: MsgHeartbeatResp, To: m.From, Context: m.Context})

	case MsgSnap:
		if n.state != Follower {
			n.becomeFollower(n.term, m.From)
		}
		n.lead = m.From
		n.electionElapsed = 0
		n.handleSnapshot(m)

	case MsgAppResp:
		if n.state != Leader {
			return
		}
		pr := n.prs[m.From]
		if m.Reject {
			if m.Index < pr.match {
				return // stale rejection of an index already known to match
			}
			// The follower lacks a matching entry at m.Index: retry with a
			// lower prev index, never above the follower's last index.
			next := min(m.Index, m.RejectHint+1)
			if next <= pr.match {
				next = pr.match + 1
			}
			pr.next = max(next, 1)
			n.sendAppend(m.From)
			return
		}
		if m.Index > pr.match {
			pr.match = m.Index
		}
		if pr.next <= pr.match {
			pr.next = pr.match + 1
		}
		if n.maybeCommit() {
			n.bcastAppend() // tell followers the new commit index
		} else if pr.match < n.lastIndex() {
			n.sendAppend(m.From)
		}

	case MsgHeartbeatResp:
		if n.state != Leader {
			return
		}
		if n.prs[m.From].match < n.lastIndex() {
			n.sendAppend(m.From)
		}
		if len(m.Context) > 0 {
			for _, r := range n.reads {
				if string(r.ctx) == string(m.Context) {
					r.acks[m.From] = true
				}
			}
			n.releaseReads()
		}
	}
}

func (n *Node) handleAppend(m Message) {
	if m.Index < n.committed {
		n.send(Message{Type: MsgAppResp, To: m.From, Index: n.committed})
		return
	}
	if n.termAt(m.Index) != m.LogTerm || m.Index > n.lastIndex() {
		n.send(Message{Type: MsgAppResp, To: m.From, Reject: true, Index: m.Index, RejectHint: n.lastIndex()})
		return
	}
	n.appendFrom(m.Entries)
	last := m.Index + uint64(len(m.Entries))
	if c := min(m.Commit, last); c > n.committed {
		n.committed = c
	}
	n.send(Message{Type: MsgAppResp, To: m.From, Index: last})
}

func (n *Node) handleSnapshot(m Message) {
	s := m.Snapshot
	if s.Index <= n.committed {
		n.send(Message{Type: MsgAppResp, To: m.From, Index: n.committed})
		return
	}
	if n.termAt(s.Index) == s.Term {
		// We already have the entry: just fast-forward the commit index.
		// Only the prefix up to s.Index is known to match the leader.
		n.committed = s.Index
		n.send(Message{Type: MsgAppResp, To: m.From, Index: s.Index})
		return
	}
	cp := *s
	n.snap = cp
	n.entries = nil
	n.committed, n.applied, n.stable = s.Index, s.Index, s.Index
	n.pendSnap = &cp
	n.send(Message{Type: MsgAppResp, To: m.From, Index: s.Index})
}

// ---- client operations ----

// Propose appends data to the log on the leader.
func (n *Node) Propose(data []byte) (uint64, error) {
	if n.state != Leader {
		return 0, ErrNotLeader
	}
	if data == nil {
		data = []byte{}
	}
	n.appendLocal(Entry{Data: data})
	n.bcastAppend()
	return n.lastIndex(), nil
}

// ReadIndex starts a linearizable read identified by ctx. The ReadState
// appears in a later Ready once a majority has confirmed leadership.
func (n *Node) ReadIndex(ctx []byte) error {
	if n.state != Leader {
		return ErrNotLeader
	}
	if n.termAt(n.committed) != n.term {
		return ErrNotReady
	}
	r := &readReq{index: n.committed, ctx: slices.Clone(ctx), acks: map[uint64]bool{n.id: true}}
	n.reads = append(n.reads, r)
	if len(n.peers) == 1 {
		n.releaseReads()
		return nil
	}
	n.bcastHeartbeat(r.ctx)
	return nil
}

func (n *Node) releaseReads() {
	if n.state != Leader {
		return
	}
	kept := n.reads[:0]
	for _, r := range n.reads {
		if len(r.acks) >= n.quorum() {
			n.readStates = append(n.readStates, ReadState{Index: r.index, Context: r.ctx})
		} else {
			kept = append(kept, r)
		}
	}
	n.reads = kept
}

// Compact discards log entries up to index (which must be applied) after
// the application has saved a snapshot with data.
func (n *Node) Compact(index uint64, data []byte) error {
	if index <= n.snap.Index {
		return nil
	}
	if index > n.applied {
		return errors.New("raft: compact beyond applied index")
	}
	t := n.termAt(index)
	n.entries = slices.Clone(n.entries[index-n.snap.Index:])
	n.snap = Snapshot{Index: index, Term: t, Data: slices.Clone(data)}
	return nil
}

// ---- Ready ----

// Ready returns pending work. Call Advance with it before the next Ready.
func (n *Node) Ready() Ready {
	rd := Ready{Messages: n.msgs, ReadStates: n.readStates}
	n.msgs, n.readStates = nil, nil
	hs := HardState{Term: n.term, Vote: n.vote, Commit: n.committed}
	if hs != n.prevHS {
		rd.HardState, rd.HardStateChanged = hs, true
	}
	rd.Snapshot = n.pendSnap
	if n.lastIndex() > n.stable {
		rd.Entries = n.entriesFrom(n.stable+1, int(n.lastIndex()-n.stable))
	}
	if n.committed > n.applied {
		lo := max(n.applied+1, n.snap.Index+1)
		rd.Committed = n.entriesFrom(lo, int(n.committed-lo+1))
	}
	return rd
}

// Advance records that the driver persisted and applied rd.
func (n *Node) Advance(rd Ready) {
	if len(rd.Entries) > 0 {
		n.stable = max(n.stable, rd.Entries[len(rd.Entries)-1].Index)
	}
	if len(rd.Committed) > 0 {
		n.applied = max(n.applied, rd.Committed[len(rd.Committed)-1].Index)
	}
	if rd.Snapshot != nil {
		n.applied = max(n.applied, rd.Snapshot.Index)
		n.pendSnap = nil
	}
	if rd.HardStateChanged {
		n.prevHS = rd.HardState
	}
}

// ---- status ----

// Status is a read-only view.
type Status struct {
	ID, Term, Lead, Commit, Applied, LastIndex, SnapIndex uint64
	State                                                 StateType
}

// Status reports the node's state.
func (n *Node) Status() Status {
	return Status{ID: n.id, Term: n.term, Lead: n.lead, Commit: n.committed, Applied: n.applied,
		LastIndex: n.lastIndex(), SnapIndex: n.snap.Index, State: n.state}
}

// IsLeader reports leadership.
func (n *Node) IsLeader() bool { return n.state == Leader }

// Snapshot returns the last compacted snapshot (for the driver to persist).
func (n *Node) Snapshot() Snapshot { return n.snap }
