// Package benchgo holds Go micro-benchmarks behind the experiments.
//
// E2: does bytecode beat tree-walking? Five rule sets from small to large,
// each evaluated by the VM and by the interpreter on the same optimized AST.
//
//	go test ./bench/go -run '^$' -bench E2 -benchmem
package benchgo

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// RuleSizes are the E2 rule sets: number of conditional statements in the
// evaluated route.
var RuleSizes = []struct {
	Name  string
	Stmts int
}{{"xs", 2}, {"s", 10}, {"m", 50}, {"l", 200}, {"xl", 1000}}

// RuleSet builds a deterministic route with n conditional statements. Most
// conditions are false for the benchmark requests, so evaluation walks the
// whole route instead of stopping at an early deny.
func RuleSet(n int) string {
	var b strings.Builder
	b.WriteString("let paid = user.tier == \"pro\" || user.tier == \"team\"\n")
	b.WriteString("route \"/bench/*\" {\n  require auth\n")
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, "  when user.tier == \"tier%d\" && header(\"X-F%d\") != \"\" { limit %d per minute by user.id }\n", i, i%7, i+1)
		case 1:
			fmt.Fprintf(&b, "  when cidr_match(client.ip, \"10.%d.0.0/16\") || query(\"p%d\") == \"1\" { set_header \"X-S%d\" \"v\" }\n", i%250, i, i%8)
		case 2:
			fmt.Fprintf(&b, "  when paid && request.method == \"DELETE\" && header(\"X-Admin\") == \"\" { deny 403 }\n")
		default:
			fmt.Fprintf(&b, "  when !(request.host == \"h%d\") && user.id == \"blocked%d\" { deny 429 }\n", i, i)
		}
	}
	b.WriteString("  limit 1000 per minute by user.id\n}\n")
	return b.String()
}

// Requests are the inputs every engine sees, round-robin.
func Requests() []*env.Request {
	return []*env.Request{
		{Authenticated: true, UserID: "u1", UserTier: "free", ClientIP: "192.168.1.10", Method: "GET", Path: "/bench/a", Host: "api",
			Header: http.Header{"X-F1": {"1"}}, RawQuery: "a=1"},
		{Authenticated: true, UserID: "u2", UserTier: "pro", ClientIP: "8.8.8.8", Method: "POST", Path: "/bench/b", Host: "api",
			Header: http.Header{"X-Admin": {"yes"}}, RawQuery: ""},
		{Authenticated: true, UserID: "u3", UserTier: "team", ClientIP: "172.16.0.1", Method: "GET", Path: "/bench/c", Host: "edge",
			Header: http.Header{}, RawQuery: "x=2&y=3"},
	}
}

func compileSize(tb testing.TB, n int) (*rules.RuleSet, *rules.Route) {
	tb.Helper()
	rs, err := rules.Compile(RuleSet(n), rules.Options{Budget: 1_000_000})
	if err != nil {
		tb.Fatal(err)
	}
	return rs, rs.Match("/bench/x")
}

// TestE2EnginesAgree is the correctness gate for the E2 numbers.
func TestE2EnginesAgree(t *testing.T) {
	for _, sz := range RuleSizes {
		rs, r := compileSize(t, sz.Stmts)
		for _, req := range Requests() {
			var vm, in env.Decision
			rs.Eval(r, rules.VM, req, &vm)
			rs.Eval(r, rules.Interp, req, &in)
			if !vm.SameOutcome(&in) || vm.Err != nil {
				t.Fatalf("size %s: vm %+v interp %+v", sz.Name, vm, in)
			}
		}
	}
}

func BenchmarkE2(b *testing.B) {
	reqs := Requests()
	for _, sz := range RuleSizes {
		rs, r := compileSize(b, sz.Stmts)
		for _, e := range []rules.Engine{rules.VM, rules.Interp} {
			b.Run(fmt.Sprintf("size=%s/engine=%s", sz.Name, e), func(b *testing.B) {
				var d env.Decision
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rs.Eval(r, e, reqs[i%len(reqs)], &d)
				}
				b.ReportMetric(float64(len(r.Chunk.Code)), "instrs")
			})
		}
	}
}
