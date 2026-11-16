// Command run executes a benchmark experiment end to end and writes a
// result file to bench/results/. Laptop mode runs every component as a
// local process built from this repo.
//
//	go run ./bench/run e1            # full run (≥60 s per level, 3 reps)
//	go run ./bench/run -quick e7     # short smoke run, written to a temp dir
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/benchfmt"
)

var (
	quick   = flag.Bool("quick", false, "short smoke run; results go to a temp dir unless -out is set")
	outDir  = flag.String("out", "", "results directory (default bench/results, or a temp dir with -quick)")
	reps    = flag.Int("reps", 3, "repetitions per measurement; the median run is reported")
	measure = flag.Duration("duration", 60*time.Second, "measured window per run (after warm-up)")
	warmup  = flag.Duration("warmup", 10*time.Second, "warm-up before each measured window")
	note    = flag.String("note", "", "free-text note stored with the result")

	// E1
	levels   = flag.String("levels", "500,1000,2500,5000,7500,10000,15000,20000", "E1 arrival rates (req/s)")
	p99Limit = flag.Duration("p99-limit", 10*time.Millisecond, "E1 pass condition: p99 below this")
	targets  = flag.String("targets", "direct,gw1,gw3", "E1 targets to measure")

	// E7
	e7Rate = flag.Int("e7-rate", 600, "E7 arrival rate (req/s)")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./bench/run [flags] e1|e2|e3|e4|e7")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(log.Ltime)
	if *quick {
		*reps = 1
		*measure = 5 * time.Second
		*warmup = 2 * time.Second
		if *levels == "500,1000,2500,5000,7500,10000,15000,20000" {
			*levels = "500,2500"
		}
	}
	dir := *outDir
	if dir == "" {
		dir = "bench/results"
		if *quick {
			dir = filepath.Join(os.TempDir(), "tollgate-quick-results")
		}
	}
	if exp := strings.ToLower(flag.Arg(0)); exp != "e2" && exp != "e4" { // pure Go, no binaries
		if err := build(); err != nil {
			log.Fatalf("build: %v", err)
		}
	}

	var res *benchfmt.Result
	var err error
	switch strings.ToLower(flag.Arg(0)) {
	case "e1":
		res, err = runE1()
	case "e2":
		res, err = runE2()
	case "e3":
		res, err = runE3()
	case "e4":
		res, err = runE4()
	case "e7":
		res, err = runE7()
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
	if *quick {
		res.Params["quick"] = true
	}
	if *note != "" {
		res.Env.Note = *note
	}
	path, err := benchfmt.Write(dir, res)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", path)
}

func build() error {
	for _, b := range []string{"tollgate-gw", "mockbackend"} {
		cmd := exec.Command("go", "build", "-o", "bin/"+b, "./cmd/"+b)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

func newResult(exp string, nodes int, params map[string]any) *benchfmt.Result {
	env := benchfmt.CaptureEnv()
	env.Mode = "laptop"
	env.NodeCount = nodes
	env.Topology = "local processes on one machine; k6, gateways and backends share its CPUs"
	params["reps"] = *reps
	params["measure_seconds"] = measure.Seconds()
	params["warmup_seconds"] = warmup.Seconds()
	return &benchfmt.Result{Experiment: exp, StartedAt: time.Now().UTC(), Env: env, Params: params}
}

// ---------- processes ----------

type cluster struct {
	dir      string
	procs    []*exec.Cmd
	backends []string // base URLs
	gateways []string // proxy base URLs
	admins   []string // gateway admin base URLs
	gwProcs  []*exec.Cmd
}

type clusterSpec struct {
	backends      int
	gateways      int
	latency       time.Duration
	jitter        time.Duration
	upstreamExtra string // YAML appended to the upstream (breaker, passive, health)
	rules         string // rule file contents; "" = no policy
	gatewayExtra  string // YAML appended to every gateway config (auth, ...)
	cluster       bool   // gateways share limits over gRPC
	batchMax      int
	batchFlush    time.Duration
}

func startCluster(spec clusterSpec) (*cluster, error) {
	dir, err := os.MkdirTemp("", "tollgate-bench-")
	if err != nil {
		return nil, err
	}
	c := &cluster{dir: dir}
	var targets strings.Builder
	for i := 1; i <= spec.backends; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", 9100+i)
		c.backends = append(c.backends, "http://"+addr)
		fmt.Fprintf(&targets, "      - url: http://%s\n", addr)
		if err := c.spawn("bin/mockbackend", "-addr", addr, "-name", fmt.Sprintf("backend-%d", i),
			"-latency", spec.latency.String(), "-jitter", spec.jitter.String()); err != nil {
			c.stop()
			return nil, err
		}
	}
	for i := 1; i <= spec.gateways; i++ {
		listen := fmt.Sprintf("127.0.0.1:%d", 18080+i)
		admin := fmt.Sprintf("127.0.0.1:%d", 19090+i)
		extra := spec.gatewayExtra
		if spec.rules != "" {
			if err := os.WriteFile(filepath.Join(dir, "rules.tg"), []byte(spec.rules), 0o644); err != nil {
				c.stop()
				return nil, err
			}
			extra += "rules: {file: rules.tg}\n"
		}
		if spec.cluster {
			extra += fmt.Sprintf("cluster:\n  self: gw-%d\n  listen: \"127.0.0.1:%d\"\n  vnodes: 128\n  peer_timeout: 20ms\n  batch: {max: %d, flush: %s}\n  members:\n",
				i, 17070+i, spec.batchMax, spec.batchFlush)
			for j := 1; j <= spec.gateways; j++ {
				extra += fmt.Sprintf("    - {name: gw-%d, addr: \"127.0.0.1:%d\"}\n", j, 17070+j)
			}
		}
		cfg := fmt.Sprintf(`listen: %q
admin_listen: %q
shutdown_timeout: 5s
upstreams:
  - name: api
    balancer: round_robin
    targets:
%s    health_check: {path: /healthz, interval: 1s, timeout: 500ms, unhealthy_threshold: 1, healthy_threshold: 2}
%sroutes:
  - name: api
    prefix: /
    upstream: api
    timeouts: {connect: 1s, header: 5s, total: 10s}
    retry: {attempts: 2, base_backoff: 5ms, max_backoff: 50ms}
%s`, listen, admin, targets.String(), spec.upstreamExtra, extra)
		path := filepath.Join(dir, fmt.Sprintf("gw-%d.yaml", i))
		if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
			c.stop()
			return nil, err
		}
		c.gateways = append(c.gateways, "http://"+listen)
		c.admins = append(c.admins, "http://"+admin)
		if err := c.spawn("bin/tollgate-gw", "-config", path, "-node", fmt.Sprintf("gw-%d", i), "-log-level", "warn"); err != nil {
			c.stop()
			return nil, err
		}
		c.gwProcs = append(c.gwProcs, c.procs[len(c.procs)-1])
	}
	for _, u := range c.backends {
		if err := waitOK(u + "/healthz"); err != nil {
			c.stop()
			return nil, err
		}
	}
	for _, u := range c.admins {
		if err := waitOK(u + "/readyz"); err != nil {
			c.stop()
			return nil, err
		}
	}
	return c, nil
}

func (c *cluster) spawn(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	logf, err := os.Create(filepath.Join(c.dir, filepath.Base(bin)+"-"+strconv.Itoa(len(c.procs))+".log"))
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	c.procs = append(c.procs, cmd)
	return nil
}

func (c *cluster) stop() {
	for _, p := range c.procs {
		_ = p.Process.Signal(syscall.SIGTERM)
	}
	for _, p := range c.procs {
		done := make(chan struct{})
		go func() { _ = p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			_ = p.Process.Kill()
		}
	}
	_ = os.RemoveAll(c.dir)
}

func waitOK(url string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s not ready", url)
}

// ---------- k6 ----------

// sleepSlack is how much longer than planned a k6 run may take before the
// runner assumes the machine slept or stalled and discards the run.
const sleepSlack = 20 * time.Second

func runK6(script string, expected time.Duration, env map[string]string) (*benchfmt.K6Summary, json.RawMessage, error) {
	start := time.Now()
	s, raw, err := runK6Once(script, env)
	if err == nil && time.Since(start) > expected+sleepSlack {
		return nil, nil, fmt.Errorf("k6 run took %v, expected about %v: the machine likely slept or stalled, so the run is discarded (keep the lid open, ideally on AC power)",
			time.Since(start).Round(time.Second), expected)
	}
	return s, raw, err
}

func runK6Once(script string, env map[string]string) (*benchfmt.K6Summary, json.RawMessage, error) {
	f, err := os.CreateTemp("", "k6-summary-*.json")
	if err != nil {
		return nil, nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	cmd := exec.Command("k6", "run", "--quiet", "--no-color", script)
	cmd.Env = append(os.Environ(), "SUMMARY="+f.Name())
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("k6: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, nil, err
	}
	var s benchfmt.K6Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, err
	}
	// Keep only the metrics block as raw output; the rest repeats options.
	var full map[string]json.RawMessage
	_ = json.Unmarshal(raw, &full)
	return &s, full["metrics"], nil
}

// ---------- Prometheus scraping ----------

type promSample map[string]float64 // series (name{labels}) -> value

func scrape(adminURL string) (promSample, error) {
	resp, err := http.Get(adminURL + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	s := promSample{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			s[line[:i]] = v
		}
	}
	return s, sc.Err()
}

// histQuantile estimates a quantile from the delta of two scrapes of a
// histogram, interpolating linearly inside the bucket (as PromQL's
// histogram_quantile does). Returns seconds.
func histQuantile(before, after promSample, name string, q float64) float64 {
	type bkt struct{ le, count float64 }
	byLe := map[float64]float64{} // summed across series (e.g. several nodes)
	prefix := name + "_bucket{"
	for k, v := range after {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		i := strings.Index(k, `le="`)
		if i < 0 {
			continue
		}
		rest := k[i+4:]
		leStr := rest[:strings.IndexByte(rest, '"')]
		le, err := strconv.ParseFloat(leStr, 64)
		if leStr == "+Inf" {
			le, err = math.Inf(1), nil
		}
		if err != nil {
			continue
		}
		byLe[le] += v - before[k]
	}
	bs := make([]bkt, 0, len(byLe))
	for le, c := range byLe {
		bs = append(bs, bkt{le, c})
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].le < bs[j].le })
	if len(bs) == 0 || bs[len(bs)-1].count == 0 {
		return 0
	}
	rank := q * bs[len(bs)-1].count
	prevLe, prevCount := 0.0, 0.0
	for _, b := range bs {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				return prevLe
			}
			if b.count == prevCount {
				return b.le
			}
			return prevLe + (b.le-prevLe)*(rank-prevCount)/(b.count-prevCount)
		}
		prevLe, prevCount = b.le, b.count
	}
	return prevLe
}

// ---------- helpers ----------

func median[T any](runs []T, key func(T) float64) (T, int) {
	idx := make([]int, len(runs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return key(runs[idx[a]]) < key(runs[idx[b]]) })
	m := idx[(len(idx)-1)/2]
	return runs[m], m
}

func secs(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func getJSON(ctx context.Context, url string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
