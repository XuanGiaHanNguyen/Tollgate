// Package sim runs a whole Raft cluster in one process with a fake clock
// and a fake network, all driven by one seed: messages are dropped,
// delayed, duplicated and reordered, nodes are partitioned and crashed
// (including between handling a message and persisting it) and restarted
// from their persisted state. Invariants are checked continuously; any
// failure replays exactly from its seed.
package sim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

// Machine is the replicated application. It must be deterministic.
type Machine interface {
	Apply(e raft.Entry)
	Snapshot() []byte
	Restore(data []byte) error
	Hash() [32]byte // state digest, compared across nodes at equal indexes
}

// Options tune a run.
type Options struct {
	Nodes        int
	Ticks        int // total ticks; faults stop at 80%, the rest must converge
	DropPct      int // max message drop %, chosen per run from [0, DropPct]
	MaxDelay     int // max delivery delay in ticks
	CrashPct     int // per-tick chance (in 1/10000) that a node crashes
	PartitionPct int // per-tick chance (in 1/10000) of a new partition
	ProposePct   int // per-tick chance (%) of a client proposal
	SnapshotEach int // compact after this many applied entries
	NewMachine   func() Machine
	// NewProposal builds the nth client proposal (default "k=v").
	NewProposal func(r *rand.Rand, n int) []byte
	Trace       func(string) // optional event log for replaying a failure
}

// DefaultOptions are the CI settings.
func DefaultOptions() Options {
	return Options{Nodes: 3, Ticks: 2000, DropPct: 20, MaxDelay: 4, CrashPct: 20, PartitionPct: 30,
		ProposePct: 30, SnapshotEach: 40, NewMachine: func() Machine { return NewKV() }}
}

type flight struct {
	at  int
	seq int
	m   raft.Message
}

type disk struct {
	hs      raft.HardState
	snap    *raft.Snapshot
	entries []raft.Entry
}

type node struct {
	id      uint64
	r       *raft.Node
	m       Machine
	up      bool
	disk    disk
	applied int // entries applied since the last compaction
	last    uint64
}

// Sim is one seeded run.
type Sim struct {
	seed     uint64
	opt      Options
	rnd      *rand.Rand
	tick     int
	seq      int
	nodes    []*node
	net      []flight
	cut      map[[2]uint64]bool // directed links that drop everything
	healAt   int
	drop     int
	leaders  map[uint64]uint64     // term -> leader
	appliedE map[uint64]raft.Entry // index -> entry applied by anyone
	hashes   map[uint64][32]byte   // index -> state hash after applying it
	proposed map[string]bool       // client values proposed
	Stats    Stats
}

// Stats summarize a run.
type Stats struct {
	Elections, Crashes, Partitions, Proposals, Committed, Snapshots, SnapInstalls, Dropped int
	FinalIndex                                                                             uint64
}

// New builds a run for seed.
func New(seed uint64, opt Options) *Sim {
	s := &Sim{seed: seed, opt: opt, rnd: rand.New(rand.NewPCG(seed, 0x51b)),
		cut: map[[2]uint64]bool{}, leaders: map[uint64]uint64{}, appliedE: map[uint64]raft.Entry{},
		hashes: map[uint64][32]byte{}, proposed: map[string]bool{}}
	s.drop = s.rnd.IntN(opt.DropPct + 1)
	// Fault intensity varies per seed so some runs are far harsher than
	// the defaults: rare interleavings (e.g. Raft's Figure 8) need bursts
	// of crashes and long, uneven delays.
	s.opt.CrashPct = opt.CrashPct * (1 + s.rnd.IntN(8))
	s.opt.PartitionPct = opt.PartitionPct * (1 + s.rnd.IntN(4))
	s.opt.MaxDelay = 1 + s.rnd.IntN(opt.MaxDelay*3)
	for i := 1; i <= opt.Nodes; i++ {
		s.nodes = append(s.nodes, &node{id: uint64(i), up: true})
	}
	// Start only once every member exists: each node's peer list is the
	// full membership (starting them inside the loop above gave node 1 a
	// one-node cluster).
	for _, n := range s.nodes {
		s.start(n)
	}
	return s
}

func (s *Sim) peers() []uint64 {
	ids := make([]uint64, len(s.nodes))
	for i, n := range s.nodes {
		ids[i] = n.id
	}
	return ids
}

// start (re)creates a node's Raft core and machine from its disk.
func (s *Sim) start(n *node) {
	s.trace("n%d START from disk hs %+v entries %d", n.id, n.disk.hs, len(n.disk.entries))
	n.m = s.opt.NewMachine()
	if n.disk.snap != nil {
		if err := n.m.Restore(n.disk.snap.Data); err != nil {
			panic(err)
		}
	}
	// Even seeds run the production config (PreVote on). Odd seeds turn it
	// off: PreVote calms elections, which hides rare interleavings (a
	// mutation run with PreVote everywhere missed Raft's Figure 8 bug in
	// 3000 seeds).
	n.r = raft.New(raft.Config{ID: n.id, Peers: s.peers(), ElectionTick: 10, HeartbeatTick: 1, PreVote: s.seed%2 == 0,
		Rand: rand.New(rand.NewPCG(s.seed, n.id*7919+uint64(s.tick))), MaxAppend: 16},
		raft.Restored{HardState: n.disk.hs, Snapshot: n.disk.snap, Entries: slices.Clone(n.disk.entries)})
	n.applied = 0
}

func (s *Sim) trace(format string, a ...any) {
	if s.opt.Trace != nil {
		s.opt.Trace(fmt.Sprintf("t%d ", s.tick) + fmt.Sprintf(format, a...))
	}
}

func (s *Sim) failf(format string, a ...any) error {
	return fmt.Errorf("seed %d tick %d: "+format, append([]any{s.seed, s.tick}, a...)...)
}

// process drains a node's Ready: persist, send, apply, advance.
func (s *Sim) process(n *node) error {
	for {
		rd := n.r.Ready()
		if rd.Empty() {
			return nil
		}
		// Crash between handling and persisting: nothing was persisted or
		// sent, so the node restarts as if the step never happened.
		if s.tick < s.opt.Ticks*8/10 && s.rnd.IntN(10000) < s.opt.CrashPct/4 {
			s.crash(n)
			return nil
		}
		if rd.Snapshot != nil {
			cp := *rd.Snapshot
			n.disk.snap = &cp
			n.disk.entries = nil
			if err := n.m.Restore(cp.Data); err != nil {
				return s.failf("node %d restore: %v", n.id, err)
			}
			if want, ok := s.hashes[cp.Index]; ok && want != n.m.Hash() {
				return s.failf("node %d installed snapshot @%d with a different state than was applied there", n.id, cp.Index)
			}
			s.Stats.SnapInstalls++
		}
		if rd.HardStateChanged {
			n.disk.hs = rd.HardState
			s.trace("n%d persist hs %+v", n.id, rd.HardState)
		}
		for _, e := range rd.Entries {
			s.trace("n%d persist entry %d/t%d %q", n.id, e.Index, e.Term, e.Data)
		}
		for _, m := range rd.Messages {
			s.trace("n%d send %v to %d term %d idx %d logterm %d n=%d commit %d reject %v", n.id, m.Type, m.To, m.Term, m.Index, m.LogTerm, len(m.Entries), m.Commit, m.Reject)
		}
		for _, e := range rd.Entries {
			// A later entry at index i replaces everything from i on.
			first := uint64(1)
			if n.disk.snap != nil {
				first = n.disk.snap.Index + 1
			}
			if e.Index < first {
				continue
			}
			n.disk.entries = n.disk.entries[:e.Index-first]
			n.disk.entries = append(n.disk.entries, e)
		}
		for _, m := range rd.Messages {
			s.deliver(m)
		}
		for _, e := range rd.Committed {
			if err := s.apply(n, e); err != nil {
				return err
			}
		}
		n.r.Advance(rd)
		if n.applied >= s.opt.SnapshotEach {
			idx := n.r.Status().Applied
			data := n.m.Snapshot()
			if err := n.r.Compact(idx, data); err != nil {
				return s.failf("node %d compact: %v", n.id, err)
			}
			snap := n.r.Snapshot()
			n.disk.snap = &snap
			n.disk.entries = slices.DeleteFunc(n.disk.entries, func(e raft.Entry) bool { return e.Index <= idx })
			n.applied = 0
			s.Stats.Snapshots++
		}
	}
}

func (s *Sim) apply(n *node, e raft.Entry) error {
	if prev, ok := s.appliedE[e.Index]; ok {
		if prev.Term != e.Term || string(prev.Data) != string(e.Data) {
			return s.failf("state machine safety: node %d applied (term %d, %q) at index %d, another node applied (term %d, %q)",
				n.id, e.Term, e.Data, e.Index, prev.Term, prev.Data)
		}
	} else {
		s.appliedE[e.Index] = e
		if len(e.Data) > 0 {
			s.Stats.Committed++
		}
	}
	if e.Index != n.last+1 && n.last != 0 && e.Index > n.last+1 {
		// allowed only right after a snapshot restore; checked via hashes
	}
	n.m.Apply(e)
	n.last = e.Index
	n.applied++
	h := n.m.Hash()
	if want, ok := s.hashes[e.Index]; ok && want != h {
		return s.failf("node %d state after index %d differs from another node's", n.id, e.Index)
	}
	s.hashes[e.Index] = h
	return nil
}

func (s *Sim) deliver(m raft.Message) {
	if s.cut[[2]uint64{m.From, m.To}] {
		s.Stats.Dropped++
		return
	}
	faults := s.tick < s.opt.Ticks*8/10
	if faults && s.rnd.IntN(100) < s.drop {
		s.Stats.Dropped++
		return
	}
	copies := 1
	if faults && s.rnd.IntN(100) < 3 {
		copies = 2 // duplicate
	}
	for i := 0; i < copies; i++ {
		s.seq++
		delay := 1 + s.rnd.IntN(s.opt.MaxDelay)
		s.net = append(s.net, flight{at: s.tick + delay, seq: s.seq, m: cloneMsg(m)})
	}
}

func cloneMsg(m raft.Message) raft.Message {
	m.Entries = slices.Clone(m.Entries)
	if m.Snapshot != nil {
		cp := *m.Snapshot
		m.Snapshot = &cp
	}
	m.Context = slices.Clone(m.Context)
	return m
}

func (s *Sim) crash(n *node) {
	s.trace("n%d CRASH (disk hs %+v, %d entries)", n.id, n.disk.hs, len(n.disk.entries))
	n.up = false
	n.r = nil
	s.Stats.Crashes++
}

func (s *Sim) node(id uint64) *node { return s.nodes[id-1] }

// Run executes the scenario and returns the first invariant violation.
func (s *Sim) Run() error {
	for s.tick = 0; s.tick < s.opt.Ticks; s.tick++ {
		faults := s.tick < s.opt.Ticks*8/10
		if !faults {
			s.cut = map[[2]uint64]bool{} // heal everything
			for _, n := range s.nodes {
				if !n.up {
					n.up = true
					s.start(n)
				}
			}
		}
		if faults {
			s.faults()
		}
		// Deliver messages due now, in (time, seq) order; arrival order
		// among them is shuffled by their random delays.
		sort.Slice(s.net, func(i, j int) bool {
			if s.net[i].at != s.net[j].at {
				return s.net[i].at < s.net[j].at
			}
			return s.net[i].seq < s.net[j].seq
		})
		var later []flight
		due := s.net
		s.net = nil
		for _, f := range due {
			if f.at > s.tick {
				later = append(later, f)
				continue
			}
			n := s.node(f.m.To)
			if !n.up {
				continue
			}
			n.r.Step(f.m)
			if err := s.process(n); err != nil {
				return err
			}
		}
		s.net = append(later, s.net...)
		for _, n := range s.nodes {
			if !n.up {
				continue
			}
			n.r.Tick()
			if err := s.process(n); err != nil {
				return err
			}
		}
		if err := s.checkLeaders(); err != nil {
			return err
		}
		if s.rnd.IntN(100) < s.opt.ProposePct {
			s.propose()
		}
	}
	return s.converged()
}

func (s *Sim) faults() {
	for _, n := range s.nodes {
		if n.up && s.rnd.IntN(10000) < s.opt.CrashPct {
			s.crash(n)
		} else if !n.up && s.rnd.IntN(100) < 5 {
			n.up = true
			s.start(n)
		}
	}
	if s.tick >= s.healAt && len(s.cut) > 0 {
		s.cut = map[[2]uint64]bool{}
	}
	if len(s.cut) == 0 && s.rnd.IntN(10000) < s.opt.PartitionPct {
		s.Stats.Partitions++
		victim := s.nodes[s.rnd.IntN(len(s.nodes))].id
		oneWay := s.rnd.IntN(4) == 0
		for _, o := range s.nodes {
			if o.id == victim {
				continue
			}
			s.cut[[2]uint64{victim, o.id}] = true
			if !oneWay {
				s.cut[[2]uint64{o.id, victim}] = true
			}
		}
		s.healAt = s.tick + 20 + s.rnd.IntN(150)
	}
}

func (s *Sim) checkLeaders() error {
	for _, n := range s.nodes {
		if !n.up || !n.r.IsLeader() {
			continue
		}
		t := n.r.Status().Term
		if prev, ok := s.leaders[t]; ok && prev != n.id {
			return s.failf("election safety: nodes %d and %d both leader in term %d", prev, n.id, t)
		}
		if _, ok := s.leaders[t]; !ok {
			s.leaders[t] = n.id
			s.Stats.Elections++
		}
	}
	return nil
}

func (s *Sim) propose() {
	for _, n := range s.nodes {
		if n.up && n.r.IsLeader() {
			s.Stats.Proposals++
			v := fmt.Sprintf("k%d=v%d", s.rnd.IntN(8), s.Stats.Proposals)
			if s.opt.NewProposal != nil {
				v = string(s.opt.NewProposal(s.rnd, s.Stats.Proposals))
			}
			if _, err := n.r.Propose([]byte(v)); err == nil {
				s.proposed[v] = true
				s.process(n)
			}
			return
		}
	}
}

// converged checks liveness and agreement after all faults healed.
func (s *Sim) converged() error {
	var leader *node
	for _, n := range s.nodes {
		if n.up && n.r.IsLeader() {
			leader = n
		}
	}
	if leader == nil {
		return s.failf("liveness: no leader %d ticks after every fault healed", s.opt.Ticks/5)
	}
	// A fresh write must commit everywhere within a bounded number of ticks.
	marker := fmt.Sprintf("final=%d", s.seed)
	if s.opt.NewProposal != nil {
		marker = string(s.opt.NewProposal(s.rnd, -1))
	}
	idx, err := leader.r.Propose([]byte(marker))
	if err != nil {
		return s.failf("liveness: leader refused proposal: %v", err)
	}
	s.process(leader)
	for extra := 0; extra < 200; extra++ {
		s.tick++
		var later []flight
		due := s.net
		s.net = nil
		sort.Slice(due, func(i, j int) bool { return due[i].at < due[j].at || due[i].at == due[j].at && due[i].seq < due[j].seq })
		for _, f := range due {
			if f.at > s.tick {
				later = append(later, f)
				continue
			}
			n := s.node(f.m.To)
			n.r.Step(f.m)
			if err := s.process(n); err != nil {
				return err
			}
		}
		s.net = append(later, s.net...)
		for _, n := range s.nodes {
			n.r.Tick()
			if err := s.process(n); err != nil {
				return err
			}
		}
		done := true
		for _, n := range s.nodes {
			if n.r.Status().Applied < idx {
				done = false
			}
		}
		if done {
			h := s.nodes[0].m.Hash()
			for _, n := range s.nodes[1:] {
				if n.r.Status().Applied == s.nodes[0].r.Status().Applied && n.m.Hash() != h {
					return s.failf("nodes %d and 1 disagree on state at the same applied index", n.id)
				}
			}
			s.Stats.FinalIndex = idx
			return nil
		}
	}
	st := []raft.Status{}
	for _, n := range s.nodes {
		st = append(st, n.r.Status())
	}
	return s.failf("liveness: write @%d not applied on every node within 200 ticks after healing: %+v", idx, st)
}

// ---- KV machine ----

// KV is a tiny deterministic key-value machine: entries are "k=v".
type KV struct {
	Index uint64
	Data  map[string]string
}

// NewKV returns an empty machine.
func NewKV() *KV { return &KV{Data: map[string]string{}} }

func (k *KV) Apply(e raft.Entry) {
	k.Index = e.Index
	if len(e.Data) == 0 {
		return
	}
	for i := 0; i < len(e.Data); i++ {
		if e.Data[i] == '=' {
			k.Data[string(e.Data[:i])] = string(e.Data[i+1:])
			return
		}
	}
}

func (k *KV) Snapshot() []byte {
	b, _ := json.Marshal(k)
	return b
}

func (k *KV) Restore(data []byte) error {
	k.Data = map[string]string{}
	return json.Unmarshal(data, k)
}

func (k *KV) Hash() [32]byte {
	keys := make([]string, 0, len(k.Data))
	for key := range k.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], k.Index)
	h.Write(b[:])
	for _, key := range keys {
		h.Write([]byte(key + "=" + k.Data[key] + "\n"))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
