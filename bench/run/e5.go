package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/control/client"
)

type e5Run struct {
	Writes        int       `json:"writes"`
	CommitP50Ms   float64   `json:"commit_p50_ms"`
	CommitP99Ms   float64   `json:"commit_p99_ms"`
	Failovers     []float64 `json:"failover_ms"`   // kill -> first successful write through a new leader
	Elections     []float64 `json:"new_leader_ms"` // kill -> some node reports leader
	FailoverP50Ms float64   `json:"failover_p50_ms"`
	FailoverMaxMs float64   `json:"failover_max_ms"`
	Acked         int       `json:"acknowledged_writes"`
	Lost          int       `json:"lost_entries"`
}

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	i := int(q * float64(len(c)))
	if i >= len(c) {
		i = len(c) - 1
	}
	return c[i]
}

func runE5() (*benchfmt.Result, error) {
	kills := 10
	if *quick {
		kills = 3
	}
	res := newResult("E5", 3, map[string]any{
		"nodes": 3, "tick": "50ms (election timeout 500-1000ms, pre-vote on)", "storage": "WAL with fsync (F_FULLFSYNC on macOS) on every Raft Ready",
		"commit_latency": "client-side Put latency, sequential writes, leader stable, measured window after warm-up",
		"failover":       fmt.Sprintf("%d SIGKILLs of the leader per run while a writer keeps writing (retrying every 5ms); 'first write' counts only writes started after the kill; the killed node is restarted between kills", kills),
		"loss_check":     "every acknowledged write (unique key) read back linearizably at the end of the run",
	})
	res.Env.Topology = "local processes: 3 tollgate-control nodes"
	var runs []e5Run
	for rep := 0; rep < *reps; rep++ {
		c, err := startCluster(clusterSpec{control: 3})
		if err != nil {
			return nil, err
		}
		run, err := e5Once(c, kills, rep)
		c.stop()
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
		log.Printf("E5 rep=%d writes=%d commit p50=%.2fms p99=%.2fms failover p50=%.0fms max=%.0fms acked=%d lost=%d",
			rep+1, run.Writes, run.CommitP50Ms, run.CommitP99Ms, run.FailoverP50Ms, run.FailoverMaxMs, run.Acked, run.Lost)
		time.Sleep(time.Second)
	}
	_, medianIdx := median(runs, func(r e5Run) float64 { return r.CommitP99Ms })
	res.FinishedAt = time.Now().UTC()
	res.Summary = mustJSON(map[string]any{"runs": runs, "median_run": medianIdx})
	res.Raw = mustJSON(runs)
	return res, nil
}

func e5Once(c *cluster, kills, rep int) (e5Run, error) {
	cl := client.New(c.ctlEndpoints())
	defer cl.Close()
	// Retry every 5ms: failover is measured by the cluster, not by the
	// client's backoff (the default 20ms..500ms schedule quantized an
	// earlier run's results to ~500ms steps).
	cl.SetMaxBackoff(5 * time.Millisecond)
	var mu sync.Mutex
	acked := map[string]bool{}
	put := func(k string) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		_, err := cl.Put(ctx, k, []byte(k))
		if err == nil {
			mu.Lock()
			acked[k] = true
			mu.Unlock()
		}
		return time.Since(start), err
	}
	run := e5Run{}
	// 1) Commit latency, leader stable.
	seq := 0
	for end := time.Now().Add(*warmup); time.Now().Before(end); seq++ {
		put(fmt.Sprintf("r%d-w%d", rep, seq))
	}
	var lat []float64
	for end := time.Now().Add(*measure); time.Now().Before(end); seq++ {
		d, err := put(fmt.Sprintf("r%d-m%d", rep, seq))
		if err == nil {
			lat = append(lat, float64(d.Microseconds())/1000)
		}
	}
	run.Writes = len(lat)
	run.CommitP50Ms, run.CommitP99Ms = round(pct(lat, 0.5), 3), round(pct(lat, 0.99), 3)

	// 2) Failovers under a continuous writer.
	stop := make(chan struct{})
	type okWrite struct{ start, end time.Time }
	okAt := make(chan okWrite, 1<<16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			began := time.Now()
			if _, err := put(fmt.Sprintf("r%d-f%d", rep, i)); err == nil {
				select {
				case okAt <- okWrite{began, time.Now()}:
				default:
				}
			}
		}
	}()
	for k := 0; k < kills; k++ {
		ld, err := c.ctlLeader(10 * time.Second)
		if err != nil {
			close(stop)
			wg.Wait()
			return run, err
		}
		time.Sleep(500 * time.Millisecond) // let writes flow
		for len(okAt) > 0 {
			<-okAt
		}
		killed := time.Now()
		_ = ld.proc.Process.Signal(syscall.SIGKILL)
		ld.proc.Wait()
		// new leader elected
		nl, err := c.ctlLeaderExcept(ld, 10*time.Second)
		if err != nil {
			close(stop)
			wg.Wait()
			return run, err
		}
		_ = nl
		run.Elections = append(run.Elections, round(float64(time.Since(killed).Microseconds())/1000, 1))
		// First write that *started* after the kill and succeeded (a write
		// committed by the old leader whose reply arrived late doesn't count).
		for {
			w := <-okAt
			if w.start.After(killed) {
				run.Failovers = append(run.Failovers, round(float64(w.end.Sub(killed).Microseconds())/1000, 1))
				break
			}
		}
		if err := c.startCtl(ld); err != nil { // the dead node rejoins
			close(stop)
			wg.Wait()
			return run, err
		}
		waitOK(ld.admin + "/healthz")
		time.Sleep(time.Second) // let it catch up before the next kill
	}
	close(stop)
	wg.Wait()
	run.FailoverP50Ms, run.FailoverMaxMs = round(pct(run.Failovers, 0.5), 1), round(pct(run.Failovers, 1), 1)

	// 3) Nothing acknowledged was lost.
	mu.Lock()
	keys := make([]string, 0, len(acked))
	for k := range acked {
		keys = append(keys, k)
	}
	mu.Unlock()
	run.Acked = len(keys)
	for _, k := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		r, err := cl.Get(ctx, k)
		cancel()
		if err != nil || !r.Found || string(r.Value) != k {
			run.Lost++
		}
	}
	return run, nil
}

// ctlLeaderExcept waits for a leader other than old.
func (c *cluster) ctlLeaderExcept(old *ctlNode, timeout time.Duration) (*ctlNode, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, nd := range c.ctl {
			if nd == old {
				continue
			}
			var st struct {
				State string `json:"state"`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err := getJSON(ctx, nd.admin+"/status", &st)
			cancel()
			if err == nil && st.State == "leader" {
				return nd, nil
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil, fmt.Errorf("no new leader within %v", timeout)
}
