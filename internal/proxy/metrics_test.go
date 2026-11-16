package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
)

func scrape(t *testing.T, m *metrics.Gateway) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestGatewayMetrics(t *testing.T) {
	good, bad := newBackend(t, "good"), newBackend(t, "bad")
	bad.status.Store(503)
	good.delay.Store(int64(100 * time.Millisecond))
	extra := `    breaker: {min_requests: 2, error_rate: 0.5, cooldown: 1h}
routes:
  - name: api
    prefix: /
    upstream: api
    retry: {attempts: 2, base_backoff: 1ms, max_backoff: 1ms}
`
	m := metrics.NewGateway("node-1")
	g, err := New(gatewayConfig(t, extra, good, bad), Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	defer gw.Close()

	for i := 0; i < 4; i++ {
		get(t, gw.URL+"/x")
	}
	get(t, gw.URL+"/") // still routed

	out := scrape(t, m)
	want := []string{
		`tollgate_requests_total{node="node-1",route="api",status="200"} 5`,
		`tollgate_request_duration_seconds_count{node="node-1",route="api"} 5`,
		`tollgate_gateway_overhead_seconds_count{node="node-1"} 5`,
		`tollgate_breaker_state{backend="` + bad.srv.URL + `"} 2`,
		`tollgate_breaker_state{backend="` + good.srv.URL + `"} 0`,
		`tollgate_backend_healthy{backend="` + good.srv.URL + `"} 1`,
		`tollgate_retries_total{route="api"} 2`,
		"go_goroutines",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q", w)
		}
	}
	if t.Failed() {
		t.Log(out)
	}

	// Overhead must exclude the 100ms the backend spends on each of the 4
	// successful requests: if upstream time leaked in, the sum would be at
	// least 400ms. A loose bound keeps this robust on a loaded CI machine.
	var sum float64
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, `tollgate_gateway_overhead_seconds_sum{node="node-1"} `) {
			fmt.Sscan(strings.Fields(l)[1], &sum)
		}
	}
	if sum <= 0 || sum > 0.25 {
		t.Errorf("total gateway overhead %.3fs for 5 requests; upstream time leaked in?\n%s", sum, grep(out, "overhead"))
	}
}

func grep(s, sub string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func TestStatusWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if sw.Unwrap() != rec {
		t.Fatal("Unwrap does not return the wrapped writer")
	}
	sw.WriteHeader(100) // informational: not the final status
	sw.WriteHeader(201)
	if sw.status != 201 {
		t.Fatalf("status = %d", sw.status)
	}
}
