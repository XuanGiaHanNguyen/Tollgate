package sim

import (
	"flag"
	"testing"
)

var seeds = flag.Int("seeds", 200, "number of seeds to simulate")
var firstSeed = flag.Int("seed", 1, "first seed (replay one failure with -seed N -seeds 1)")

// TestSeeds runs the whole cluster under faults for many seeds. A failure
// prints its seed; rerun with -seed N -seeds 1 to replay it exactly.
func TestSeeds(t *testing.T) { runSeeds(t, DefaultOptions) }

// TestSeedsStore runs the real control-plane config store on the cluster.
func TestSeedsStore(t *testing.T) { runSeeds(t, StoreOptions) }

func runSeeds(t *testing.T, opts func() Options) {
	var tot Stats
	for s := *firstSeed; s < *firstSeed+*seeds; s++ {
		sim := New(uint64(s), opts())
		if err := sim.Run(); err != nil {
			t.Fatal(err)
		}
		st := sim.Stats
		tot.Elections += st.Elections
		tot.Crashes += st.Crashes
		tot.Partitions += st.Partitions
		tot.Committed += st.Committed
		tot.Snapshots += st.Snapshots
		tot.SnapInstalls += st.SnapInstalls
		tot.Dropped += st.Dropped
	}
	t.Logf("%d seeds ok: elections=%d crashes=%d partitions=%d committed=%d snapshots=%d snapshot-installs=%d dropped=%d",
		*seeds, tot.Elections, tot.Crashes, tot.Partitions, tot.Committed, tot.Snapshots, tot.SnapInstalls, tot.Dropped)
}

// The same seed must produce the same run.
func TestDeterministic(t *testing.T) {
	a, b := New(42, DefaultOptions()), New(42, DefaultOptions())
	ea, eb := a.Run(), b.Run()
	if (ea == nil) != (eb == nil) || a.Stats != b.Stats {
		t.Fatalf("seed 42 not deterministic: %+v vs %+v", a.Stats, b.Stats)
	}
}
