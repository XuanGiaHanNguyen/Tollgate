package ratelimit

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/ratelimit/peer"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// TestPeerLatencyProbe measures per-check latency through a loopback
// cluster at a steady rate. Diagnostic only: run with TOLLGATE_PROBE=1.
func TestPeerLatencyProbe(t *testing.T) {
	if os.Getenv("TOLLGATE_PROBE") == "" {
		t.Skip("set TOLLGATE_PROBE=1")
	}
	for _, pc := range []peer.Config{
		{MaxBatch: 1, Timeout: time.Second},
		{MaxBatch: 100, FlushInterval: 500 * time.Microsecond, Timeout: time.Second},
	} {
		nodes := cluster(t, 3, pc)
		key := keyOwnedBy(t, nodes[0].lim, "gw-3")
		lr := env.LimitReq{ID: 7, Key: key, Count: 1 << 40, Per: time.Second}
		const rate, secs = 2000, 3
		lat := make(chan time.Duration, rate*secs)
		tick := time.NewTicker(time.Second / rate)
		for i := 0; i < rate*secs; i++ {
			<-tick.C
			go func() {
				s := time.Now()
				nodes[0].lim.Allow(context.Background(), lr)
				lat <- time.Since(s)
			}()
		}
		tick.Stop()
		var ds []time.Duration
		for i := 0; i < rate*secs; i++ {
			ds = append(ds, <-lat)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("batch=%d p50=%v p99=%v max=%v rpcs=%d checks=%d", pc.MaxBatch,
			ds[len(ds)/2], ds[len(ds)*99/100], ds[len(ds)-1], nodes[0].batches.Load(), nodes[0].checks.Load())
	}
}
