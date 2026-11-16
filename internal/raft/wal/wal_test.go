package wal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

func ents(term uint64, from, to uint64) []raft.Entry {
	var es []raft.Entry
	for i := from; i <= to; i++ {
		es = append(es, raft.Entry{Term: term, Index: i, Data: []byte{byte(i)}})
	}
	return es
}

func reopen(t *testing.T, dir string) (*Storage, raft.Restored) {
	t.Helper()
	s, r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, r
}

func TestSaveAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, r := reopen(t, dir)
	if len(r.Entries) != 0 || r.Snapshot != nil {
		t.Fatal("fresh dir not empty")
	}
	hs := raft.HardState{Term: 2, Vote: 1, Commit: 3}
	if err := s.Save(&hs, ents(1, 1, 5), nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, r = reopen(t, dir)
	if r.HardState != hs || len(r.Entries) != 5 || r.Entries[4].Index != 5 || r.Entries[2].Data[0] != 3 {
		t.Fatalf("restored %+v", r)
	}
}

func TestConflictingSuffixReplaced(t *testing.T) {
	dir := t.TempDir()
	s, _ := reopen(t, dir)
	s.Save(nil, ents(1, 1, 5), nil)
	s.Save(nil, ents(2, 3, 4), nil) // a new leader overwrote 3.. with term 2
	s.Close()
	_, r := reopen(t, dir)
	if len(r.Entries) != 4 || r.Entries[2].Term != 2 || r.Entries[3].Term != 2 {
		t.Fatalf("entries %+v", r.Entries)
	}
}

func TestTornTailDropped(t *testing.T) {
	dir := t.TempDir()
	s, _ := reopen(t, dir)
	hs := raft.HardState{Term: 1, Commit: 2}
	s.Save(&hs, ents(1, 1, 3), nil)
	s.Close()
	// Simulate a crash mid-write: append half a record.
	f, _ := os.OpenFile(filepath.Join(dir, walName), os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0, 0, 0, 40, 1, 2, 3})
	f.Close()
	s, r := reopen(t, dir)
	if len(r.Entries) != 3 || r.HardState != hs {
		t.Fatalf("restored %+v", r)
	}
	// And the WAL keeps working after the truncated tail.
	s.Save(nil, ents(1, 4, 4), nil)
	s.Close()
	_, r = reopen(t, dir)
	if len(r.Entries) != 4 {
		t.Fatalf("append after torn tail lost: %+v", r.Entries)
	}
}

func TestCorruptRecordStopsReplay(t *testing.T) {
	dir := t.TempDir()
	s, _ := reopen(t, dir)
	s.Save(nil, ents(1, 1, 3), nil)
	s.Close()
	b, _ := os.ReadFile(filepath.Join(dir, walName))
	b[len(b)-1] ^= 0xff // flip a bit in the last record
	os.WriteFile(filepath.Join(dir, walName), b, 0o644)
	_, r := reopen(t, dir)
	if len(r.Entries) != 2 {
		t.Fatalf("corrupt record not dropped: %d entries", len(r.Entries))
	}
}

func TestSnapshotCompactsWAL(t *testing.T) {
	dir := t.TempDir()
	s, _ := reopen(t, dir)
	hs := raft.HardState{Term: 1, Commit: 8}
	s.Save(&hs, ents(1, 1, 10), nil)
	if err := s.SaveSnapshot(raft.Snapshot{Index: 8, Term: 1, Data: []byte("state@8")}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, r := reopen(t, dir)
	if r.Snapshot == nil || r.Snapshot.Index != 8 || string(r.Snapshot.Data) != "state@8" {
		t.Fatalf("snapshot %+v", r.Snapshot)
	}
	if len(r.Entries) != 2 || r.Entries[0].Index != 9 || r.HardState != hs {
		t.Fatalf("entries after snapshot %+v hs %+v", r.Entries, r.HardState)
	}
	// A snapshot received from the leader replaces the whole log.
	s2, _ := reopen(t, dir)
	s2.Save(&raft.HardState{Term: 3, Commit: 20}, nil, &raft.Snapshot{Index: 20, Term: 3, Data: []byte("state@20")})
	s2.Close()
	_, r = reopen(t, dir)
	if r.Snapshot.Index != 20 || len(r.Entries) != 0 {
		t.Fatalf("installed snapshot: %+v %d", r.Snapshot, len(r.Entries))
	}
}

func TestNoOpEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, _ := reopen(t, dir)
	s.Save(nil, []raft.Entry{{Term: 1, Index: 1}}, nil)
	s.Close()
	_, r := reopen(t, dir)
	if len(r.Entries) != 1 || r.Entries[0].Data != nil {
		t.Fatalf("%+v", r.Entries)
	}
}
