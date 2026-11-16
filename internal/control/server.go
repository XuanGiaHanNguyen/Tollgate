// Package control runs the control plane: a Raft node with durable storage
// and a gRPC transport, the config store as its state machine, and the
// client API (writes through the leader, linearizable reads, watch
// streams).
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/store"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft/wal"
)

// Errors returned to clients.
var (
	ErrNotLeader = errors.New("control: not the leader")
	ErrDropped   = errors.New("control: proposal was overwritten by a new leader; retry")
	ErrStopped   = errors.New("control: server stopped")
)

// Transport sends Raft messages to peers. It must not block.
type Transport interface {
	Send(msgs []raft.Message)
}

// Hooks receive events for metrics. Any may be nil.
type Hooks struct {
	Term          func(term uint64, leader bool)
	CommitLatency func(seconds float64)
}

// Config for a control-plane node.
type Config struct {
	ID            uint64
	Peers         []uint64
	Dir           string
	TickInterval  time.Duration // default 50ms; election timeout is 10-20 ticks
	SnapshotEvery uint64        // compact after this many applied entries (default 1000)
	Log           *slog.Logger
	Hooks         Hooks
}

type proposal struct {
	data  []byte
	start time.Time
	done  chan error // buffered
	index uint64
	term  uint64
	ver   *uint64
}

type readReq struct {
	ctx  string
	done chan error
	idx  uint64
}

// Server is one control-plane node.
type Server struct {
	cfg  Config
	log  *slog.Logger
	node *raft.Node
	st   *wal.Storage
	tr   Transport

	mu      sync.RWMutex // guards fsm and the fields below
	fsm     *store.FSM
	status  raft.Status
	changed chan struct{} // closed and replaced after every apply

	recvc  chan raft.Message
	propc  chan *proposal
	readc  chan *readReq
	stopc  chan struct{}
	doneWG sync.WaitGroup

	waiting  map[uint64]*proposal // by log index
	reads    map[string]*readReq
	ready    []*readReq // confirmed, waiting for apply
	sinceSnp uint64
}

// NewServer opens storage, restores the state machine and starts the loop.
func NewServer(cfg Config, tr Transport) (*Server, error) {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 50 * time.Millisecond
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = 1000
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	st, restored, err := wal.Open(cfg.Dir)
	if err != nil {
		return nil, err
	}
	fsm := store.New()
	if restored.Snapshot != nil {
		if err := fsm.Restore(restored.Snapshot.Data); err != nil {
			st.Close()
			return nil, fmt.Errorf("control: restore snapshot: %w", err)
		}
	}
	s := &Server{cfg: cfg, log: cfg.Log, st: st, tr: tr, fsm: fsm, changed: make(chan struct{}),
		recvc: make(chan raft.Message, 4096), propc: make(chan *proposal, 1024), readc: make(chan *readReq, 1024),
		stopc: make(chan struct{}), waiting: map[uint64]*proposal{}, reads: map[string]*readReq{}}
	s.node = raft.New(raft.Config{ID: cfg.ID, Peers: cfg.Peers, ElectionTick: 10, HeartbeatTick: 1, PreVote: true}, restored)
	s.status = s.node.Status()
	s.doneWG.Add(1)
	go s.loop()
	return s, nil
}

// Stop ends the loop and closes storage.
func (s *Server) Stop() {
	select {
	case <-s.stopc:
		return
	default:
	}
	close(s.stopc)
	s.doneWG.Wait()
	s.st.Close()
}

// Receive hands an incoming Raft message to the node (from the transport).
func (s *Server) Receive(ctx context.Context, m raft.Message) error {
	select {
	case s.recvc <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopc:
		return ErrStopped
	}
}

func (s *Server) loop() {
	defer s.doneWG.Done()
	t := time.NewTicker(s.cfg.TickInterval)
	defer t.Stop()
	lastTerm, wasLeader := uint64(0), false
	for {
		select {
		case <-s.stopc:
			s.failAll(ErrStopped)
			return
		case <-t.C:
			s.node.Tick()
		case m := <-s.recvc:
			s.node.Step(m)
		case p := <-s.propc:
			idx, err := s.node.Propose(p.data)
			if err != nil {
				p.done <- err
				break
			}
			p.index, p.term = idx, s.node.Status().Term
			s.waiting[idx] = p
		case r := <-s.readc:
			if err := s.node.ReadIndex([]byte(r.ctx)); err != nil {
				r.done <- err
				break
			}
			s.reads[r.ctx] = r
		}
		if err := s.handleReady(); err != nil {
			s.log.Error("control: storage failure; stopping node", "err", err)
			s.failAll(err)
			return
		}
		st := s.node.Status()
		if st.Term != lastTerm || (st.State == raft.Leader) != wasLeader {
			lastTerm, wasLeader = st.Term, st.State == raft.Leader
			if s.cfg.Hooks.Term != nil {
				s.cfg.Hooks.Term(st.Term, wasLeader)
			}
			if !wasLeader {
				// Reads pending on a deposed leader can never be confirmed.
				for k, r := range s.reads {
					r.done <- ErrNotLeader
					delete(s.reads, k)
				}
			}
		}
	}
}

// handleReady drains the node: persist (fsync), send, apply, advance.
func (s *Server) handleReady() error {
	for {
		rd := s.node.Ready()
		if rd.Empty() {
			return nil
		}
		var hs *raft.HardState
		if rd.HardStateChanged {
			hs = &rd.HardState
		}
		if err := s.st.Save(hs, rd.Entries, rd.Snapshot); err != nil {
			return err
		}
		if len(rd.Messages) > 0 {
			s.tr.Send(rd.Messages)
		}
		s.mu.Lock()
		if rd.Snapshot != nil {
			if err := s.fsm.Restore(rd.Snapshot.Data); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		for _, e := range rd.Committed {
			ch, _ := s.fsm.Apply(e)
			if p := s.waiting[e.Index]; p != nil {
				delete(s.waiting, e.Index)
				if e.Term != p.term {
					p.done <- ErrDropped // another leader's entry took this index
				} else {
					if ch != nil && p.ver != nil {
						*p.ver = ch.Version
					} else if p.ver != nil {
						*p.ver = s.fsm.Version
					}
					if s.cfg.Hooks.CommitLatency != nil {
						s.cfg.Hooks.CommitLatency(time.Since(p.start).Seconds())
					}
					p.done <- nil
				}
			}
		}
		applied := s.fsm.Applied
		s.node.Advance(rd)
		s.status = s.node.Status()
		if len(rd.Committed) > 0 || rd.Snapshot != nil {
			close(s.changed)
			s.changed = make(chan struct{})
		}
		s.mu.Unlock()
		s.sinceSnp += uint64(len(rd.Committed))

		for _, rs := range rd.ReadStates {
			if r := s.reads[string(rs.Context)]; r != nil {
				delete(s.reads, string(rs.Context))
				r.idx = rs.Index
				s.ready = append(s.ready, r)
			}
		}
		kept := s.ready[:0]
		for _, r := range s.ready {
			if applied >= r.idx {
				r.done <- nil
			} else {
				kept = append(kept, r)
			}
		}
		s.ready = kept

		if s.sinceSnp >= s.cfg.SnapshotEvery {
			s.mu.RLock()
			data, idx := s.fsm.Snapshot(), s.fsm.Applied
			s.mu.RUnlock()
			if err := s.node.Compact(idx, data); err == nil {
				if err := s.st.SaveSnapshot(s.node.Snapshot()); err != nil {
					return err
				}
				s.sinceSnp = 0
			}
		}
	}
}

func (s *Server) failAll(err error) {
	for k, p := range s.waiting {
		p.done <- err
		delete(s.waiting, k)
	}
	for k, r := range s.reads {
		r.done <- err
		delete(s.reads, k)
	}
	for _, r := range s.ready {
		r.done <- err
	}
	s.ready = nil
}

// Propose replicates a command and waits until it is applied here.
func (s *Server) Propose(ctx context.Context, c store.Command) (uint64, error) {
	if c.TS == 0 {
		c.TS = time.Now().UnixNano()
	}
	var ver uint64
	p := &proposal{data: c.Encode(), start: time.Now(), done: make(chan error, 1), ver: &ver}
	select {
	case s.propc <- p:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.stopc:
		return 0, ErrStopped
	}
	select {
	case err := <-p.done:
		return ver, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// LinearizableGet confirms leadership (ReadIndex) and reads after applying
// up to the confirmed index.
func (s *Server) LinearizableGet(ctx context.Context, key string) (store.Item, bool, error) {
	var b [8]byte
	rand.Read(b[:])
	r := &readReq{ctx: hex.EncodeToString(b[:]), done: make(chan error, 1)}
	for attempt := 0; ; attempt++ {
		select {
		case s.readc <- r:
		case <-ctx.Done():
			return store.Item{}, false, ctx.Err()
		}
		var err error
		select {
		case err = <-r.done:
		case <-ctx.Done():
			return store.Item{}, false, ctx.Err()
		}
		if errors.Is(err, raft.ErrNotReady) && attempt < 50 {
			// A fresh leader hasn't committed its no-op yet; retry shortly.
			time.Sleep(s.cfg.TickInterval)
			continue
		}
		if err != nil {
			return store.Item{}, false, err
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		it, ok := s.fsm.Get(key)
		return it, ok, nil
	}
}

// Status reports the node's Raft state and store version.
func (s *Server) Status() (raft.Status, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.fsm.Version
}

// Watch streams changes after from to send until ctx ends. The first call
// to send may be a full sync if from is behind the retained history.
func (s *Server) Watch(ctx context.Context, from uint64, send func(full bool, changes []store.Change, version uint64) error) error {
	for {
		s.mu.RLock()
		changed := s.changed
		changes, ok := s.fsm.Since(from)
		full := false
		if !ok {
			changes, full = s.fsm.All(), true
		}
		version := s.fsm.Version
		s.mu.RUnlock()
		if full || len(changes) > 0 {
			if err := send(full, changes, version); err != nil {
				return err
			}
			from = version
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopc:
			return ErrStopped
		case <-changed:
		}
	}
}
