package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/metrics"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
)

const policyRules = `
let free = user.tier == "free"
route "/api/*" {
  require auth
  when free { limit 3 per minute by user.id }
  when header("X-Debug") != "" { deny 403 }
  set_header "X-User" user.id
}
route "/public/*" {
  limit 2 per hour by client.ip
}
`

func policyGateway(t *testing.T, engine string) (*Gateway, *httptest.Server, *metrics.Gateway, *http.Header) {
	t.Helper()
	var seen http.Header
	bsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Write([]byte("ok"))
	}))
	t.Cleanup(bsrv.Close)
	b := &backend{srv: bsrv}
	extra := `
routes: [{prefix: /, upstream: api}]
rules: {engine: ` + engine + `}
auth:
  api_keys:
    - {key: free-key, user_id: alice, tier: free}
    - {key: pro-key, user_id: bob, tier: pro}
`
	m := metrics.NewGateway("n1")
	g, err := New(gatewayConfig(t, extra, b), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Compile(policyRules, rules.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v := g.SetRules(rs); v != 1 {
		t.Fatalf("version %d", v)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv, m, &seen
}

func do(t *testing.T, url, key string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestPolicyEndToEnd(t *testing.T) {
	for _, engine := range []string{"vm", "interp"} {
		t.Run(engine, func(t *testing.T) {
			_, srv, m, seen := policyGateway(t, engine)

			if c := do(t, srv.URL+"/api/x", "", nil); c != 401 {
				t.Fatalf("no key: %d", c)
			}
			if c := do(t, srv.URL+"/api/x", "wrong", nil); c != 401 {
				t.Fatalf("bad key: %d", c)
			}
			// Free tier: 3 per minute, then 429.
			for i := 0; i < 3; i++ {
				if c := do(t, srv.URL+"/api/x", "free-key", nil); c != 200 {
					t.Fatalf("free request %d: %d", i, c)
				}
			}
			if c := do(t, srv.URL+"/api/x", "free-key", nil); c != 429 {
				t.Fatalf("4th free request: %d", c)
			}
			// Pro tier has no limit here; header set and key stripped.
			for i := 0; i < 5; i++ {
				if c := do(t, srv.URL+"/api/x", "pro-key", nil); c != 200 {
					t.Fatalf("pro request %d: %d", i, c)
				}
			}
			if got := seen.Get("X-User"); got != "bob" {
				t.Fatalf("X-User = %q", got)
			}
			if seen.Get("Authorization") != "" {
				t.Fatal("gateway API key forwarded to backend")
			}
			if c := do(t, srv.URL+"/api/x", "pro-key", map[string]string{"X-Debug": "1"}); c != 403 {
				t.Fatalf("debug header: %d", c)
			}
			// Per-IP limit on public routes; paths without a rule route pass.
			do(t, srv.URL+"/public/a", "", nil)
			do(t, srv.URL+"/public/a", "", nil)
			if c := do(t, srv.URL+"/public/a", "", nil); c != 429 {
				t.Fatalf("public 3rd: %d", c)
			}
			if c := do(t, srv.URL+"/other", "", nil); c != 200 {
				t.Fatalf("unruled path: %d", c)
			}

			out := scrape(t, m)
			for _, w := range []string{
				`tollgate_policy_decisions_total{result="limited"} 2`,
				`tollgate_policy_decisions_total{result="deny"} 3`,
				`tollgate_requests_total{node="n1",route="/",status="429"} 2`,
				`tollgate_rule_eval_seconds_count{engine="` + engine + `"}`,
				`tollgate_config_version{node="n1"} 1`,
				`tollgate_ratelimit_decisions_total{node="n1",result="deny"} 2`,
				`tollgate_ratelimit_decisions_total{node="n1",result="degraded"} 0`,
			} {
				if !strings.Contains(out, w) {
					t.Errorf("metrics missing %q", w)
				}
			}
		})
	}
}

func TestSetRulesSwapsAtomically(t *testing.T) {
	g, srv, _, _ := policyGateway(t, "vm")
	rs, _ := rules.Compile(`route "/*" { deny 418 }`, rules.Options{})
	if v := g.SetRules(rs); v != 2 {
		t.Fatalf("version %d", v)
	}
	if c := do(t, srv.URL+"/anything", "", nil); c != 418 {
		t.Fatalf("after swap: %d", c)
	}
}
