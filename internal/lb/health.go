package lb

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// HealthConfig drives active health checks.
type HealthConfig struct {
	Path               string // "" disables active checks
	Interval           time.Duration
	Timeout            time.Duration
	UnhealthyThreshold int // consecutive failures before marking down
	HealthyThreshold   int // consecutive successes before marking up
}

// HealthChecker probes every target of a pool on an interval.
type HealthChecker struct {
	cfg      HealthConfig
	client   *http.Client
	targets  []*Target
	onChange func(t *Target, healthy bool)

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewHealthChecker builds a checker; call Start to run it.
func NewHealthChecker(cfg HealthConfig, targets []*Target, onChange func(*Target, bool)) *HealthChecker {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Second
	}
	if cfg.UnhealthyThreshold <= 0 {
		cfg.UnhealthyThreshold = 2
	}
	if cfg.HealthyThreshold <= 0 {
		cfg.HealthyThreshold = 1
	}
	return &HealthChecker{
		cfg:      cfg,
		client:   &http.Client{Timeout: cfg.Timeout},
		targets:  targets,
		onChange: onChange,
		stop:     make(chan struct{}),
	}
}

// Start launches one probe loop per target.
func (h *HealthChecker) Start() {
	if h.cfg.Path == "" {
		return
	}
	for _, t := range h.targets {
		h.wg.Add(1)
		go h.loop(t)
	}
}

// Stop ends all probe loops and waits for them.
func (h *HealthChecker) Stop() {
	close(h.stop)
	h.wg.Wait()
}

func (h *HealthChecker) loop(t *Target) {
	defer h.wg.Done()
	tick := time.NewTicker(h.cfg.Interval)
	defer tick.Stop()
	var fails, oks int
	for {
		ok := h.probe(t)
		if ok {
			fails, oks = 0, oks+1
			if !t.Healthy() && oks >= h.cfg.HealthyThreshold {
				t.SetHealthy(true)
				h.notify(t, true)
			}
		} else {
			oks, fails = 0, fails+1
			if t.Healthy() && fails >= h.cfg.UnhealthyThreshold {
				t.SetHealthy(false)
				h.notify(t, false)
			}
		}
		select {
		case <-h.stop:
			return
		case <-tick.C:
		}
	}
}

func (h *HealthChecker) notify(t *Target, healthy bool) {
	if h.onChange != nil {
		h.onChange(t, healthy)
	}
}

func (h *HealthChecker) probe(t *Target) bool {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.Timeout)
	defer cancel()
	u := *t.URL
	u.Path = h.cfg.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
