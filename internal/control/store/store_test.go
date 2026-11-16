package store

import (
	"fmt"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/raft"
)

func put(idx uint64, k, v string) raft.Entry {
	return raft.Entry{Index: idx, Term: 1, Data: Command{Op: "put", Key: k, Value: []byte(v)}.Encode()}
}

func TestApplyVersionsAndSince(t *testing.T) {
	f := New()
	f.Apply(raft.Entry{Index: 1, Term: 1}) // no-op
	f.Apply(put(2, "rules", "a"))
	f.Apply(put(3, "members", "x"))
	f.Apply(raft.Entry{Index: 4, Term: 1, Data: Command{Op: "delete", Key: "members"}.Encode()})
	if f.Version != 4 || f.Applied != 4 {
		t.Fatalf("version %d applied %d", f.Version, f.Applied)
	}
	if it, ok := f.Get("rules"); !ok || string(it.Value) != "a" || it.Version != 2 {
		t.Fatalf("get %+v", it)
	}
	ch, ok := f.Since(2)
	if !ok || len(ch) != 2 || ch[0].Version != 3 || !ch[1].Deleted {
		t.Fatalf("since(2) = %+v", ch)
	}
	if ch, ok := f.Since(4); !ok || len(ch) != 0 {
		t.Fatalf("since(current) = %+v", ch)
	}
	if _, err := f.Apply(raft.Entry{Index: 5, Data: []byte("garbage")}); err != ErrBadCommand || f.Applied != 5 || f.Version != 4 {
		t.Fatal("bad command must advance applied but not version")
	}
}

func TestTxnAtomicAndConditional(t *testing.T) {
	f := New()
	f.Apply(put(1, "rollout", "canary"))
	txn := Command{Op: "txn", If: &Cond{Key: "rollout", Version: 1}, Ops: []Command{
		{Op: "put", Key: "rules", Value: []byte("new")},
		{Op: "put", Key: "rollout", Value: []byte("promoted")},
	}}
	chs, err := f.Apply(raft.Entry{Index: 2, Term: 1, Data: txn.Encode()})
	if err != nil || len(chs) != 2 || chs[0].Version != 2 || chs[1].Version != 2 {
		t.Fatalf("txn: %+v %v", chs, err)
	}
	if it, _ := f.Get("rules"); string(it.Value) != "new" {
		t.Fatal("txn put not applied")
	}
	// The same guarded txn again: rollout's version is now 2, so it fails
	// and changes nothing (a deposed leader can't decide twice).
	before := f.Hash()
	if _, err := f.Apply(raft.Entry{Index: 3, Term: 1, Data: txn.Encode()}); err != ErrCondFailed {
		t.Fatalf("err = %v", err)
	}
	if f.Applied != 3 {
		t.Fatal("failed txn must still advance applied")
	}
	f.Applied = 2
	if f.Hash() != before {
		t.Fatal("failed txn changed state")
	}
	// Cond version 0 = key must not exist.
	if _, err := f.Apply(raft.Entry{Index: 4, Data: Command{Op: "txn", If: &Cond{Key: "absent"}, Ops: []Command{{Op: "put", Key: "x", Value: []byte("1")}}}.Encode()}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryTrimForcesFullSync(t *testing.T) {
	f := New()
	for i := uint64(1); i <= HistoryLen+10; i++ {
		f.Apply(put(i, fmt.Sprintf("k%d", i%5), "v"))
	}
	if _, ok := f.Since(5); ok {
		t.Fatal("history trimmed past version 5 but Since claims it can serve it")
	}
	if ch, ok := f.Since(f.Dropped); !ok || len(ch) != HistoryLen {
		t.Fatalf("since(dropped) ok=%v n=%d", ok, len(ch))
	}
	if all := f.All(); len(all) != 5 {
		t.Fatalf("full sync %d keys", len(all))
	}
}

func TestSnapshotRestoreDeterministic(t *testing.T) {
	a := New()
	for i := uint64(1); i <= 20; i++ {
		a.Apply(put(i, fmt.Sprintf("k%d", i%3), fmt.Sprint(i)))
	}
	b := New()
	if err := b.Restore(a.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() {
		t.Fatal("restored state differs")
	}
	b.Apply(put(21, "k", "x"))
	if a.Hash() == b.Hash() {
		t.Fatal("hash ignores changes")
	}
}
