package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
listen: ":8080"
upstreams:
  - name: api
    balancer: least_conn
    targets:
      - url: http://b1:9000
      - url: http://b2:9000
        weight: 3
    health_check:
      path: /healthz
      interval: 500ms
routes:
  - prefix: /api/
    upstream: api
    methods: [get, post]
    timeouts:
      total: 5s
    retry:
      attempts: 3
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Upstreams[0].HealthCheck.Interval; got != 500*time.Millisecond {
		t.Errorf("interval = %v", got)
	}
	r := c.Routes[0]
	if r.Name != "/api/" || r.Timeouts.Total != 5*time.Second || r.Timeouts.Connect != 2*time.Second {
		t.Errorf("route defaults not applied: %+v", r)
	}
	if r.Methods[0] != "GET" || r.Methods[1] != "POST" {
		t.Errorf("methods not upper-cased: %v", r.Methods)
	}
	if c.RetryBudget.Ratio != 0.2 || !*c.Transport.ForceHTTP2 {
		t.Errorf("global defaults not applied: %+v", c)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"unknown field", "bogus: 1\n", []string{"field bogus not found"}},
		{"no routes", "upstreams: []\n", []string{"at least one route"}},
		{
			"many problems at once",
			`
upstreams:
  - name: api
    balancer: random
    targets: [{url: "not a url"}]
  - name: api
    targets: [{url: http://x}]
routes:
  - prefix: api
    upstream: missing
  - prefix: /a
    upstream: api
    retry: {attempts: 20}
`,
			[]string{
				`unknown balancer "random"`,
				`invalid url "not a url"`,
				`duplicate upstream "api"`,
				"prefix must start with /",
				`unknown upstream "missing"`,
				"retry.attempts must be in [1,10]",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestRulesAndAuth(t *testing.T) {
	c, err := Parse([]byte(valid + `
rules: {file: policy.tg}
auth:
  api_keys:
    - {key: k1, user_id: u1, tier: free}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Rules.ReloadInterval != time.Second || c.Rules.Budget != 10000 || c.Rules.Engine != "vm" {
		t.Errorf("rules defaults: %+v", c.Rules)
	}
	_, err = Parse([]byte(valid + `
rules: {engine: jit}
auth:
  api_keys:
    - {key: k1, user_id: u1}
    - {key: k1, user_id: u2}
    - {key: "", user_id: u3}
`))
	for _, w := range []string{"rules.engine must be vm or interp", "duplicate key", "key and user_id are required"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v missing %q", err, w)
		}
	}
}

func TestCluster(t *testing.T) {
	c, err := Parse([]byte(valid + `
cluster:
  self: gw-1
  listen: ":7001"
  members:
    - {name: gw-1, addr: "gw-1:7001"}
    - {name: gw-2, addr: "gw-2:7001"}
`))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.Cluster
	if cl.VNodes != 128 || cl.Batch.Max != 100 || cl.Batch.Flush != 500*time.Microsecond || cl.PeerTimeout != 20*time.Millisecond {
		t.Errorf("cluster defaults: %+v", cl)
	}
	_, err = Parse([]byte(valid + `
cluster:
  self: gw-9
  batch: {max: 5000}
  members:
    - {name: gw-1, addr: "a"}
    - {name: gw-1, addr: "b"}
    - {name: gw-2}
`))
	for _, w := range []string{"cluster.listen is required", `duplicate member "gw-1"`, "name and addr are required",
		`cluster.self "gw-9" is not in cluster.members`, "cluster.batch.max must be in [1,1000]"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v missing %q", err, w)
		}
	}
}
