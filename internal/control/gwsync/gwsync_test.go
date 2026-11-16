package gwsync

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCacheRoundTripAndRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if _, ok := LoadCache(path, func(uint64, int64, []byte) error { return nil }); ok {
		t.Fatal("loaded a missing cache")
	}
	if err := saveCache(path, 7, []byte("route \"/*\" { deny 401 }")); err != nil {
		t.Fatal(err)
	}
	var gotV uint64
	var gotSrc string
	v, ok := LoadCache(path, func(ver uint64, ts int64, src []byte) error {
		gotV, gotSrc = ver, string(src)
		if ts != 0 {
			t.Error("cached rules must not report a commit time")
		}
		return nil
	})
	if !ok || v != 7 || gotV != 7 || gotSrc == "" {
		t.Fatalf("v=%d ok=%v", v, ok)
	}
	// A cache the gateway rejects (e.g. names an unknown provider) is not used.
	if _, ok := LoadCache(path, func(uint64, int64, []byte) error { return errors.New("no") }); ok {
		t.Fatal("rejected cache reported as loaded")
	}
}
