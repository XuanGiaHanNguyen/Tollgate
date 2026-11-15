package sim

import (
	"os"
	"strconv"
	"testing"
)

// TestTrace replays one seed with a full event log: TRACE_SEED=2 go test -run TestTrace ./internal/sim
func TestTrace(t *testing.T) {
	seed, err := strconv.Atoi(os.Getenv("TRACE_SEED"))
	if err != nil {
		t.Skip("set TRACE_SEED")
	}
	opt := DefaultOptions()
	opt.Trace = func(s string) { t.Log(s) }
	if err := New(uint64(seed), opt).Run(); err != nil {
		t.Fatal(err)
	}
}
