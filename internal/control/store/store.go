// Package store is the control plane's replicated state machine: a
// versioned key-value store applied from the Raft log. A change's version
// is its Raft index, so versions increase monotonically and every node
// agrees on them. It keeps a bounded history of changes so gateways can
// resume a watch from their last version; if they fall behind the history,
// they get a full snapshot first. It is deterministic and does no I/O, so
// the simulator runs the same code.
package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

// HistoryLen bounds the change history kept for watchers.
const HistoryLen = 1024

// Command is what gets proposed to Raft.
type Command struct {
	Op    string `json:"op"` // "put" or "delete"
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"`
	TS    int64  `json:"ts"` // proposer's wall clock (unix nanos), for propagation timing
}

// Encode serializes a command.
func (c Command) Encode() []byte { b, _ := json.Marshal(c); return b }

// Change is one applied write.
type Change struct {
	Version uint64 `json:"version"`
	Key     string `json:"key"`
	Value   []byte `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	TS      int64  `json:"ts"`
}

// Item is a current value.
type Item struct {
	Value   []byte `json:"value"`
	Version uint64 `json:"version"`
}

// FSM is the state machine.
type FSM struct {
	Version uint64          `json:"version"` // last applied change's version
	Applied uint64          `json:"applied"` // last applied Raft index (incl. no-ops)
	KV      map[string]Item `json:"kv"`
	History []Change        `json:"history"`
	Dropped uint64          `json:"dropped"` // version of the newest change trimmed from History (0 = none)
}

// New returns an empty store.
func New() *FSM { return &FSM{KV: map[string]Item{}} }

// ErrBadCommand marks an entry that is not a valid command (ignored, but
// still advances Applied, so every node stays in lockstep).
var ErrBadCommand = errors.New("store: invalid command")

// Apply applies one committed entry and returns the change, if any.
func (f *FSM) Apply(e raft.Entry) (*Change, error) {
	f.Applied = e.Index
	if len(e.Data) == 0 {
		return nil, nil // leader no-op
	}
	var c Command
	if err := json.Unmarshal(e.Data, &c); err != nil || c.Key == "" || (c.Op != "put" && c.Op != "delete") {
		return nil, ErrBadCommand
	}
	ch := Change{Version: e.Index, Key: c.Key, TS: c.TS}
	switch c.Op {
	case "put":
		f.KV[c.Key] = Item{Value: c.Value, Version: e.Index}
		ch.Value = c.Value
	case "delete":
		if _, ok := f.KV[c.Key]; !ok {
			return nil, nil // nothing to delete: no change, no version bump
		}
		delete(f.KV, c.Key)
		ch.Deleted = true
	}
	f.Version = e.Index
	f.History = append(f.History, ch)
	if over := len(f.History) - HistoryLen; over > 0 {
		f.Dropped = f.History[over-1].Version
		f.History = append([]Change(nil), f.History[over:]...)
	}
	return &ch, nil
}

// Get returns the current value of key.
func (f *FSM) Get(key string) (Item, bool) {
	it, ok := f.KV[key]
	return it, ok
}

// Since returns every change after version, oldest first. ok is false when
// history no longer reaches back that far (a change after version was
// trimmed): the caller must send a full snapshot (All) instead.
func (f *FSM) Since(version uint64) (changes []Change, ok bool) {
	if version < f.Dropped {
		return nil, false
	}
	i := sort.Search(len(f.History), func(i int) bool { return f.History[i].Version > version })
	return append([]Change(nil), f.History[i:]...), true
}

// All returns the current state as changes (a full sync for watchers).
func (f *FSM) All() []Change {
	keys := make([]string, 0, len(f.KV))
	for k := range f.KV {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Change, 0, len(keys))
	for _, k := range keys {
		it := f.KV[k]
		out = append(out, Change{Version: it.Version, Key: k, Value: it.Value})
	}
	return out
}

// Snapshot serializes the whole state (JSON maps encode with sorted keys,
// so the bytes are deterministic).
func (f *FSM) Snapshot() []byte { b, _ := json.Marshal(f); return b }

// Restore replaces the state from a snapshot.
func (f *FSM) Restore(data []byte) error {
	g := New()
	if err := json.Unmarshal(data, g); err != nil {
		return err
	}
	if g.KV == nil {
		g.KV = map[string]Item{}
	}
	*f = *g
	return nil
}

// Hash digests the state (simulation cross-checks).
func (f *FSM) Hash() [32]byte { return sha256.Sum256(f.Snapshot()) }
