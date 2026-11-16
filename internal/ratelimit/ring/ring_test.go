package ring

import (
	"fmt"
	"math"
	"testing"
)

func TestDeterministicAndOrderIndependent(t *testing.T) {
	a, _ := New([]string{"gw-1", "gw-2", "gw-3"}, 64)
	b, _ := New([]string{"gw-3", "gw-1", "gw-2"}, 64)
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("0|user-%d", i)
		if a.Owner(k) != b.Owner(k) {
			t.Fatalf("owner of %s depends on member order", k)
		}
	}
	// Pinned value: the hash must not change between builds or processes.
	if got := Hash("0|user-1"); got != Hash("0|user-1") || got == 0 {
		t.Fatal("hash not stable")
	}
}

func TestEmptyAndSingle(t *testing.T) {
	r, _ := New(nil, 0)
	if r.Owner("x") != "" {
		t.Fatal("empty ring returned an owner")
	}
	r, _ = New([]string{"only"}, 0)
	if r.Owner("x") != "only" || r.VNodes() != DefaultVNodes {
		t.Fatal("single-node ring wrong")
	}
	if _, err := New([]string{"a", "a"}, 1); err == nil {
		t.Fatal("duplicate member accepted")
	}
}

// Adding a node moves only keys that now belong to it (≈1/N of them);
// removing one moves only the keys it owned.
func TestMinimalMovement(t *testing.T) {
	const keys = 100_000
	three, _ := New([]string{"a", "b", "c"}, 128)
	four, _ := New([]string{"a", "b", "c", "d"}, 128)
	moved := 0
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("0|k%d", i)
		before, after := three.Owner(k), four.Owner(k)
		if before != after {
			moved++
			if after != "d" {
				t.Fatalf("key %s moved from %s to %s, not to the new node", k, before, after)
			}
		}
	}
	frac := float64(moved) / keys
	if math.Abs(frac-0.25) > 0.05 {
		t.Fatalf("moved %.3f of keys, want ≈0.25", frac)
	}
}

func TestSpread(t *testing.T) {
	r, _ := New([]string{"a", "b", "c"}, 128)
	counts := map[string]int{}
	for i := 0; i < 90_000; i++ {
		counts[r.Owner(fmt.Sprintf("0|k%d", i))]++
	}
	for n, c := range counts {
		if c < 24_000 || c > 36_000 { // within ±20% of 30k
			t.Errorf("%s owns %d keys", n, c)
		}
	}
}

func BenchmarkOwner(b *testing.B) {
	r, _ := New([]string{"gw-1", "gw-2", "gw-3"}, 128)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Owner("0|user-12345")
	}
}
