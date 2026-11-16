package main

import (
	"fmt"
	"log"
	"math"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/ring"
)

type e4Spread struct {
	Nodes      int     `json:"nodes"`
	MaxOverAvg float64 `json:"max_over_mean"`
	MinOverAvg float64 `json:"min_over_mean"`
	StdDevPct  float64 `json:"stddev_pct_of_mean"`
}

type e4Row struct {
	VNodes        int      `json:"vnodes"`
	AddMovedPct   float64  `json:"add_moved_pct"`    // 3 -> 4 nodes
	AddWrongMoves int      `json:"add_wrong_moves"`  // keys that moved somewhere other than the new node
	RemMovedPct   float64  `json:"remove_moved_pct"` // 4 -> 3 nodes
	RemWrongMoves int      `json:"remove_wrong_moves"`
	IdealPct      float64  `json:"ideal_pct"` // 1/4 in both directions
	Spread3       e4Spread `json:"spread_3_nodes"`
	Spread4       e4Spread `json:"spread_4_nodes"`
}

func runE4() (*benchfmt.Result, error) {
	const keys = 1_000_000
	vnodeSet := []int{1, 16, 64, 128, 256}
	res := newResult("E4", 4, map[string]any{
		"keys": keys, "vnodes": vnodeSet, "key_format": "<limitID>|user-<n>",
		"hash":      "FNV-1a 64 + splitmix64 finalizer",
		"scenarios": "add gw-4 to {gw-1..gw-3}; remove gw-4 from {gw-1..gw-4}",
	})
	res.Env.Topology = "single process (pure computation, deterministic)"
	delete(res.Params, "measure_seconds")
	delete(res.Params, "warmup_seconds")
	res.Params["reps"] = 1

	hashes := make([]uint64, keys)
	for i := range hashes {
		hashes[i] = ring.Hash(fmt.Sprintf("0|user-%d", i))
	}
	three := []string{"gw-1", "gw-2", "gw-3"}
	four := append(append([]string(nil), three...), "gw-4")

	var rows []e4Row
	for _, v := range vnodeSet {
		r3, _ := ring.New(three, v)
		r4, _ := ring.New(four, v)
		row := e4Row{VNodes: v, IdealPct: 25}
		counts3, counts4 := map[string]int{}, map[string]int{}
		addMoved, remMoved := 0, 0
		for _, h := range hashes {
			o3, o4 := r3.OwnerHash(h), r4.OwnerHash(h)
			counts3[o3]++
			counts4[o4]++
			if o3 != o4 {
				addMoved++ // adding gw-4 changed this key's owner
				if o4 != "gw-4" {
					row.AddWrongMoves++
				}
				remMoved++ // removing gw-4 moves it back
				if o4 != "gw-4" {
					row.RemWrongMoves++
				}
			}
		}
		row.AddMovedPct = round(100*float64(addMoved)/keys, 3)
		row.RemMovedPct = round(100*float64(remMoved)/keys, 3)
		row.Spread3 = spread(counts3, 3, keys)
		row.Spread4 = spread(counts4, 4, keys)
		log.Printf("E4 vnodes=%d add moved %.2f%% (ideal 25%%) wrong=%d; 3-node max/mean=%.3f stddev=%.1f%%",
			v, row.AddMovedPct, row.AddWrongMoves, row.Spread3.MaxOverAvg, row.Spread3.StdDevPct)
		rows = append(rows, row)
	}
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"rows": rows})
	res.Raw = mustJSON(rows)
	return res, nil
}

func spread(counts map[string]int, nodes, keys int) e4Spread {
	mean := float64(keys) / float64(nodes)
	mx, mn := 0.0, math.Inf(1)
	var ss float64
	for i := 0; i < nodes; i++ {
		c := float64(counts[fmt.Sprintf("gw-%d", i+1)])
		mx, mn = math.Max(mx, c), math.Min(mn, c)
		ss += (c - mean) * (c - mean)
	}
	return e4Spread{Nodes: nodes, MaxOverAvg: round(mx/mean, 4), MinOverAvg: round(mn/mean, 4),
		StdDevPct: round(100*math.Sqrt(ss/float64(nodes))/mean, 2)}
}
