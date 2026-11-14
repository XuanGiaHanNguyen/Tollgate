package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/config"
)

// backend is a test upstream whose behavior can change mid-test.
type backend struct {
	name   string
	srv    *httptest.Server
	hits   atomic.Int64
	status atomic.Int64 // 0 = 200
	delay  atomic.Int64 // nanoseconds
	down   atomic.Bool  // health endpoint fails
}

func newBackend(t *testing.T, name string) *backend {
	b := &backend{name: name}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if b.down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		b.hits.Add(1)
		if d := b.delay.Load(); d > 0 {
			select {
			case <-time.After(time.Duration(d)):
			case <-r.Context().Done():
				return
			}
		}
		if s := b.status.Load(); s != 0 {
			w.WriteHeader(int(s))
		}
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s|%s|%s", b.name, r.Host, body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func gatewayConfig(t *testing.T, extra string, backends ...*backend) *config.Config {
	t.Helper()
	var targets strings.Builder
	for _, b := range backends {
		fmt.Fprintf(&targets, "      - url: %s\n", b.srv.URL)
	}
	y := fmt.Sprintf(`
upstreams:
  - name: api
    targets:
%s%s`, targets.String(), extra)
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, y)
	}
	return c
}

const defaultRoutes = `
routes:
  - prefix: /
    upstream: api
    retry: {attempts: 3, base_backoff: 1ms, max_backoff: 2ms}
`

func startGateway(t *testing.T, cfg *config.Config) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	g.Start()
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); g.Stop() })
	return g, srv
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestProxyRoundRobinAndHeaders(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	_, gw := startGateway(t, gatewayConfig(t, defaultRoutes, b1, b2))
	for i := 0; i < 10; i++ {
		code, body := get(t, gw.URL+"/x")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		if !strings.Contains(body, strings.TrimPrefix(gw.URL, "http://")) {
			t.Fatalf("original Host not preserved: %q", body)
		}
	}
	if b1.hits.Load() != 5 || b2.hits.Load() != 5 {
		t.Fatalf("hits %d/%d, want 5/5", b1.hits.Load(), b2.hits.Load())
	}
}

func TestNoRoute404(t *testing.T) {
	b1 := newBackend(t, "b1")
	_, gw := startGateway(t, gatewayConfig(t, "\nroutes:\n  - prefix: /api\n    upstream: api\n", b1))
	if code, _ := get(t, gw.URL+"/web"); code != 404 {
		t.Fatalf("status %d, want 404", code)
	}
}

func TestRetryIdempotentOnly(t *testing.T) {
	bad, good := newBackend(t, "bad"), newBackend(t, "good")
	bad.status.Store(503)
	_, gw := startGateway(t, gatewayConfig(t, defaultRoutes+"    \n", bad, good))

	// GET is retried onto the other backend.
	for i := 0; i < 4; i++ {
		code, body := get(t, gw.URL+"/x")
		if code != 200 || !strings.HasPrefix(body, "good") {
			t.Fatalf("GET: %d %q", code, body)
		}
	}
	// POST is never retried; with round robin, half of them hit "bad".
	var fails int
	for i := 0; i < 4; i++ {
		resp, err := http.Post(gw.URL+"/x", "text/plain", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 503 {
			fails++
		}
	}
	if fails != 2 {
		t.Fatalf("POST 503s = %d, want 2 (no retries)", fails)
	}
}

func TestRetryReplaysBody(t *testing.T) {
	bad, good := newBackend(t, "bad"), newBackend(t, "good")
	bad.status.Store(502)
	_, gw := startGateway(t, gatewayConfig(t, defaultRoutes, bad, good))
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest(http.MethodPut, gw.URL+"/x", strings.NewReader("hello"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasSuffix(string(b), "|hello") {
			t.Fatalf("PUT: %d %q", resp.StatusCode, b)
		}
	}
}

func TestRetryBudgetCapsRetries(t *testing.T) {
	b1, b2 := newBackend(t, "b1"), newBackend(t, "b2")
	b1.status.Store(503)
	b2.status.Store(503)
	extra := `    breaker: {disabled: true}
retry_budget: {ratio: 0.2, initial: 1}
routes:
  - prefix: /
    upstream: api
    retry: {attempts: 5, base_backoff: 1ms, max_backoff: 1ms}
`
	_, gw := startGateway(t, gatewayConfig(t, extra, b1, b2))
	const n = 200
	for i := 0; i < n; i++ {
		get(t, gw.URL+"/x")
	}
	retries := b1.hits.Load() + b2.hits.Load() - n
	if limit := int64(n*0.2) + 1; retries > limit {
		t.Fatalf("retries = %d, budget allows %d", retries, limit)
	}
	if retries == 0 {
		t.Fatal("no retries at all")
	}
}

func TestTimeouts(t *testing.T) {
	slow := newBackend(t, "slow")
	slow.delay.Store(int64(500 * time.Millisecond))
	extra := `
routes:
  - prefix: /header
    upstream: api
    timeouts: {header: 50ms}
  - prefix: /total
    upstream: api
    timeouts: {total: 50ms}
`
	_, gw := startGateway(t, gatewayConfig(t, extra, slow))
	for _, p := range []string{"/header", "/total"} {
		start := time.Now()
		code, _ := get(t, gw.URL+p)
		if code != http.StatusGatewayTimeout {
			t.Errorf("%s: status %d, want 504", p, code)
		}
		if el := time.Since(start); el > 300*time.Millisecond {
			t.Errorf("%s: took %v", p, el)
		}
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
upstreams: [{name: api, targets: [{url: %s}]}]
routes: [{prefix: /, upstream: api}]
`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	_, gw := startGateway(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gw.URL+"/x", nil)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, _ = http.DefaultClient.Do(req)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	b1 := newBackend(t, "b1")
	b1.status.Store(500)
	extra := `    breaker: {window: 10s, min_requests: 5, error_rate: 0.5, cooldown: 100ms, half_open_probes: 1}
routes: [{prefix: /, upstream: api}]
`
	g, gw := startGateway(t, gatewayConfig(t, extra, b1))
	for i := 0; i < 5; i++ {
		get(t, gw.URL+"/x")
	}
	hits := b1.hits.Load()
	code, _ := get(t, gw.URL+"/x")
	if code != 503 || b1.hits.Load() != hits {
		t.Fatalf("breaker did not open: status %d, hits %d->%d", code, hits, b1.hits.Load())
	}
	b1.status.Store(0)
	time.Sleep(120 * time.Millisecond)
	if code, _ := get(t, gw.URL+"/x"); code != 200 {
		t.Fatalf("probe status %d", code)
	}
	if s := g.Upstream("api").Targets[0].Breaker.State(); s.String() != "closed" {
		t.Fatalf("breaker %v after successful probe", s)
	}
}

// Acceptance check (M1): killing a backend shifts traffic within the
// health-check interval.
func TestKilledBackendShiftsTraffic(t *testing.T) {
	b1, b2, b3 := newBackend(t, "b1"), newBackend(t, "b2"), newBackend(t, "b3")
	const interval = 100 * time.Millisecond
	extra := fmt.Sprintf(`    health_check: {path: /healthz, interval: %s, timeout: 50ms, unhealthy_threshold: 1}
routes: [{prefix: /, upstream: api}]
`, interval)
	g, gw := startGateway(t, gatewayConfig(t, extra, b1, b2, b3))

	// Let the initial probes finish and land mid-interval, so the kill does
	// not race the first probe and the measurement reflects the interval.
	time.Sleep(interval + interval/3)
	if !g.Upstream("api").Targets[2].Healthy() {
		t.Fatal("b3 unhealthy before kill")
	}
	b3.srv.Close() // kill it: connection refused from now on
	killed := time.Now()
	var shifted time.Duration
	for time.Since(killed) < 2*time.Second {
		before := b1.hits.Load() + b2.hits.Load()
		ok := true
		for i := 0; i < 6; i++ {
			resp, err := http.Get(gw.URL + "/x")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				ok = false
			}
		}
		if ok && b1.hits.Load()+b2.hits.Load()-before == 6 {
			shifted = time.Since(killed)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if shifted == 0 {
		t.Fatal("traffic never shifted off the dead backend")
	}
	// Probe period plus one probe timeout is the worst case.
	if limit := interval + 50*time.Millisecond + 50*time.Millisecond; shifted > limit {
		t.Fatalf("traffic shifted after %v, want <= %v", shifted, limit)
	}
	t.Logf("traffic shifted %v after kill (interval %v)", shifted, interval)
}

func TestLeastConnReleasesOnBodyClose(t *testing.T) {
	b1 := newBackend(t, "b1")
	extra := "    balancer: least_conn\nroutes: [{prefix: /, upstream: api}]\n"
	g, gw := startGateway(t, gatewayConfig(t, extra, b1))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(t, gw.URL+"/x") }()
	}
	wg.Wait()
	waitUntil(t, func() bool { return g.Upstream("api").Targets[0].Inflight() == 0 })
}

func TestGracefulShutdownDrains(t *testing.T) {
	b1 := newBackend(t, "b1")
	b1.delay.Store(int64(200 * time.Millisecond))
	g, err := New(gatewayConfig(t, defaultRoutes, b1), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, &http.Server{Handler: g}, ln, 2*time.Second) }()

	url := "http://" + ln.Addr().String() + "/x"
	result := make(chan int, 1)
	go func() {
		code, _ := get(t, url)
		result <- code
	}()
	time.Sleep(50 * time.Millisecond) // request is in flight
	cancel()
	if code := <-result; code != 200 {
		t.Fatalf("in-flight request got %d during shutdown", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("still accepting after shutdown")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func BenchmarkGatewayServeHTTP(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	cfg, _ := config.Parse([]byte(fmt.Sprintf(`
upstreams: [{name: api, targets: [{url: %s}]}]
routes: [{prefix: /, upstream: api}]
`, srv.URL)))
	g, _ := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/x", nil)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req)
			if w.Code != 200 {
				b.Fatalf("status %d", w.Code)
			}
		}
	})
}
