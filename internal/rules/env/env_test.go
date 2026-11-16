package env

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestQuery(t *testing.T) {
	r := &Request{RawQuery: "a=1&b=x%20y&c&a=2&d+e=f"}
	for k, want := range map[string]string{"a": "1", "b": "x y", "c": "", "missing": "", "d e": "f"} {
		if got := r.Query(k); got != want {
			t.Errorf("Query(%q) = %q, want %q", k, got, want)
		}
	}
	// More pairs than the cache holds still works (falls back to scanning).
	var parts []string
	for i := 0; i < 40; i++ {
		parts = append(parts, fmt.Sprintf("k%d=v%d", i, i))
	}
	r = &Request{RawQuery: strings.Join(parts, "&")}
	if got := r.Query("k39"); got != "v39" {
		t.Errorf("overflow lookup = %q", got)
	}
}

func TestCIDRMatchCachesClientIP(t *testing.T) {
	r := &Request{ClientIP: "10.1.2.3"}
	if !r.cidrMatch("10.1.2.3", "10.0.0.0/8") || r.cidrMatch("10.1.2.3", "192.168.0.0/16") {
		t.Fatal("wrong match")
	}
	if !r.ipParsed || !r.ipOK {
		t.Fatal("client IP not cached")
	}
	if r.cidrMatch("not-an-ip", "10.0.0.0/8") || r.cidrMatch("10.1.2.3", "bad") {
		t.Fatal("invalid input matched")
	}
	if !r.cidrMatch("::ffff:10.0.0.1", "10.0.0.0/8") {
		t.Fatal("IPv4-mapped address not unmapped")
	}
}

func TestHeaderFastAndSlowPath(t *testing.T) {
	h := http.Header{"X-Debug": {"1"}}
	if Header(h, "X-Debug") != "1" || Header(h, "x-debug") != "1" || Header(h, "Nope") != "" {
		t.Fatal("header lookup wrong")
	}
}
