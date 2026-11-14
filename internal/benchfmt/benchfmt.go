// Package benchfmt defines the result-file format shared by the benchmark
// runner (bench/run) and the report generator (bench/report.go).
package benchfmt

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Result is one file in bench/results/.
type Result struct {
	Experiment string          `json:"experiment"` // "E1", "E7", ...
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Env        Env             `json:"env"`
	Params     map[string]any  `json:"params"`
	Summary    json.RawMessage `json:"summary"` // experiment-specific
	Raw        json.RawMessage `json:"raw"`     // raw k6 / go test output
}

// Env records where a result was measured.
type Env struct {
	Mode      string  `json:"mode"` // "laptop" or "aws"
	GitCommit string  `json:"git_commit"`
	GitDirty  bool    `json:"git_dirty"`
	Machine   string  `json:"machine"`
	CPU       string  `json:"cpu"`
	CPUs      int     `json:"cpus"`
	MemoryGB  float64 `json:"memory_gb"`
	OS        string  `json:"os"`
	GoVersion string  `json:"go_version"`
	K6Version string  `json:"k6_version,omitempty"`
	NodeCount int     `json:"node_count"`
	Topology  string  `json:"topology"`                       // how processes were run, e.g. "local processes"
	LoadAvg1m string  `json:"load_avg_1m_at_start,omitempty"` // other load on the machine when the run started
	Note      string  `json:"note,omitempty"`
}

func cmdOut(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CaptureEnv fills what can be read from the machine. Mode, NodeCount and
// Topology are set by the caller.
func CaptureEnv() Env {
	e := Env{
		GitCommit: cmdOut("git", "rev-parse", "HEAD"),
		GitDirty:  cmdOut("git", "status", "--porcelain", "--untracked-files=no") != "",
		CPUs:      runtime.NumCPU(),
		OS:        runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion: runtime.Version(),
	}
	if v := cmdOut("k6", "version"); v != "" {
		e.K6Version = strings.SplitN(v, " (", 2)[0]
	}
	switch runtime.GOOS {
	case "darwin":
		e.Machine = cmdOut("sysctl", "-n", "hw.model")
		e.CPU = cmdOut("sysctl", "-n", "machdep.cpu.brand_string")
		if f := strings.Fields(strings.Trim(cmdOut("sysctl", "-n", "vm.loadavg"), "{} ")); len(f) > 0 {
			e.LoadAvg1m = f[0]
		}
		if b := cmdOut("sysctl", "-n", "hw.memsize"); b != "" {
			var n float64
			_ = json.Unmarshal([]byte(b), &n)
			e.MemoryGB = n / (1 << 30)
		}
	case "linux":
		if b, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name"); err == nil {
			e.Machine = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			e.LoadAvg1m = strings.Fields(string(b))[0]
		}
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "model name") {
					e.CPU = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
					break
				}
			}
		}
	}
	return e
}

// Write saves r as bench/results/<exp>-<date>.json under dir and returns the path.
func Write(dir string, r *Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := strings.ToLower(r.Experiment) + "-" + r.StartedAt.UTC().Format("20060102T150405Z") + ".json"
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

// LoadAll reads every result in dir, oldest first.
func LoadAll(dir string) ([]*Result, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Result
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// LatencyStats is one k6 trend, in milliseconds.
type LatencyStats struct {
	Avg float64 `json:"avg_ms"`
	P50 float64 `json:"p50_ms"`
	P90 float64 `json:"p90_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

// K6Summary is the subset of a k6 handleSummary JSON we read.
type K6Summary struct {
	Metrics map[string]struct {
		Values map[string]float64 `json:"values"`
	} `json:"metrics"`
}

// Trend extracts a trend metric (e.g. "http_req_duration{scenario:measure}").
func (s *K6Summary) Trend(name string) (LatencyStats, bool) {
	m, ok := s.Metrics[name]
	if !ok {
		return LatencyStats{}, false
	}
	v := m.Values
	return LatencyStats{Avg: v["avg"], P50: v["med"], P90: v["p(90)"], P95: v["p(95)"], P99: v["p(99)"], Max: v["max"]}, true
}

// Value reads one value of a metric, 0 if absent.
func (s *K6Summary) Value(name, key string) float64 {
	if m, ok := s.Metrics[name]; ok {
		return m.Values[key]
	}
	return 0
}
