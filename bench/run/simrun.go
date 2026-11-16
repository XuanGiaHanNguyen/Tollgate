package main

import (
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

var simLine = regexp.MustCompile(`(\d+) seeds ok: elections=(\d+) crashes=(\d+) partitions=(\d+) committed=(\d+) snapshots=(\d+) snapshot-installs=(\d+) dropped=(\d+)`)

// runSim records the deterministic-simulation acceptance run and the
// mutation check as a result file.
func runSim() (*benchfmt.Result, error) {
	seeds := 5000
	if *quick {
		seeds = 100
	}
	res := newResult("SIM", 3, map[string]any{
		"seeds_per_suite": seeds,
		"suites":          "TestSeeds (KV machine) and TestSeedsStore (control-plane config store)",
		"faults":          "message drop/delay/duplicate/reorder, one- and two-way partitions, crashes incl. between step and persist; intensity varies per seed; PreVote on even seeds",
		"invariants":      "election safety, state machine safety (entry and state hash per index), snapshot consistency, liveness after healing",
		"mutants":         "bench/sim/mutants.py: five planted Raft bugs, each must be caught",
	})
	res.Env.Topology = "single process (deterministic simulation)"
	delete(res.Params, "warmup_seconds")
	delete(res.Params, "measure_seconds")
	start := time.Now()
	out, err := exec.Command("go", "test", "-count=1", "-timeout", "120m", "-v", "-run", "TestSeeds", "./internal/sim",
		"-args", "-seeds", strconv.Itoa(seeds)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("simulation failed:\n%s", out)
	}
	var suites []map[string]int
	for _, m := range simLine.FindAllStringSubmatch(string(out), -1) {
		row := map[string]int{}
		for i, k := range []string{"seeds", "elections", "crashes", "partitions", "committed", "snapshots", "snapshot_installs", "dropped"} {
			row[k], _ = strconv.Atoi(m[i+1])
		}
		suites = append(suites, row)
	}
	simSecs := time.Since(start).Seconds()
	mut, merr := exec.Command("python3", "bench/sim/mutants.py", "3000").CombinedOutput()
	mutants := strings.Split(strings.TrimSpace(string(mut)), "\n")
	log.Printf("SIM %d seeds x %d suites ok in %.0fs; mutants:\n%s", seeds, len(suites), simSecs, mut)
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"suites": suites, "seconds": round(simSecs, 1), "mutants": mutants, "all_mutants_caught": merr == nil})
	res.Raw = mustJSON(string(out))
	return res, nil
}
