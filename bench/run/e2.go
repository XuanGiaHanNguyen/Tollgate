package main

import (
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

type e2Engine struct {
	NsPerOp     float64   `json:"ns_per_op"` // median of reps
	BytesPerOp  float64   `json:"bytes_per_op"`
	AllocsPerOp float64   `json:"allocs_per_op"`
	AllNsPerOp  []float64 `json:"all_ns_per_op"`
}

type e2Size struct {
	Name    string   `json:"name"`
	Stmts   int      `json:"stmts"`
	Instrs  float64  `json:"bytecode_instrs"`
	VM      e2Engine `json:"vm"`
	Interp  e2Engine `json:"interp"`
	Speedup float64  `json:"speedup"` // interp ns / vm ns
}

type e2Summary struct {
	EnginesAgree bool     `json:"engines_agree"`
	Sizes        []e2Size `json:"sizes"`
}

var e2Line = regexp.MustCompile(`^BenchmarkE2/size=(\w+)/engine=(\w+)-\d+\s+\d+\s+(.*)$`)

func runE2() (*benchfmt.Result, error) {
	res := newResult("E2", 1, map[string]any{
		"benchmark": "go test ./bench/go -bench E2 -benchmem",
		"sizes":     "xs=2, s=10, m=50, l=200, xl=1000 conditional statements in the evaluated route",
		"gate":      "TestE2EnginesAgree and internal/rules TestDifferential must pass first",
	})
	res.Env.Topology = "single process (go test)"
	delete(res.Params, "measure_seconds")
	delete(res.Params, "warmup_seconds")

	gate := exec.Command("go", "test", "-count=1", "-run", "TestE2EnginesAgree", "./bench/go")
	if out, err := gate.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("engines disagree; refusing to benchmark:\n%s", out)
	}
	gate = exec.Command("go", "test", "-count=1", "-run", "TestDifferential", "./internal/rules")
	if out, err := gate.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("differential test failed; refusing to benchmark:\n%s", out)
	}

	benchtime := "1s"
	if *quick {
		benchtime = "100ms"
	}
	cmd := exec.Command("go", "test", "./bench/go", "-run", "^$", "-bench", "E2", "-benchmem",
		"-count", strconv.Itoa(*reps), "-benchtime", benchtime)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go test: %v\n%s", err, out)
	}
	log.Printf("E2 benchmark output:\n%s", out)

	type key struct{ size, engine string }
	samples := map[key][]map[string]float64{}
	for _, line := range strings.Split(string(out), "\n") {
		m := e2Line.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		f := strings.Fields(m[3])
		vals := map[string]float64{}
		for i := 0; i+1 < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err == nil {
				vals[f[i+1]] = v
			}
		}
		k := key{m[1], m[2]}
		samples[k] = append(samples[k], vals)
	}

	stmts := map[string]int{"xs": 2, "s": 10, "m": 50, "l": 200, "xl": 1000}
	sum := e2Summary{EnginesAgree: true}
	for _, name := range []string{"xs", "s", "m", "l", "xl"} {
		sz := e2Size{Name: name, Stmts: stmts[name]}
		for _, eng := range []string{"vm", "interp"} {
			runs := samples[key{name, eng}]
			if len(runs) == 0 {
				return nil, fmt.Errorf("no benchmark output for size=%s engine=%s", name, eng)
			}
			sort.Slice(runs, func(i, j int) bool { return runs[i]["ns/op"] < runs[j]["ns/op"] })
			med := runs[(len(runs)-1)/2]
			e := e2Engine{NsPerOp: med["ns/op"], BytesPerOp: med["B/op"], AllocsPerOp: med["allocs/op"]}
			for _, r := range runs {
				e.AllNsPerOp = append(e.AllNsPerOp, r["ns/op"])
			}
			if eng == "vm" {
				sz.VM = e
				sz.Instrs = med["instrs"]
			} else {
				sz.Interp = e
			}
		}
		if sz.VM.NsPerOp > 0 {
			sz.Speedup = round(sz.Interp.NsPerOp/sz.VM.NsPerOp, 2)
		}
		sum.Sizes = append(sum.Sizes, sz)
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(sum)
	res.Raw = mustJSON(string(out))
	return res, nil
}
