package types

import (
	"os"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/internal/golden"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
)

func TestGolden(t *testing.T) {
	for _, in := range golden.Inputs(t) {
		t.Run(golden.Base(in), func(t *testing.T) {
			src, _ := os.ReadFile(in)
			prog, pdiags := parser.Parse(string(src))
			if len(pdiags) > 0 {
				t.Fatalf("parse errors: %v", pdiags)
			}
			_, diags := Check(prog)
			var b strings.Builder
			for _, d := range diags {
				b.WriteString(d.String() + "\n")
			}
			golden.Check(t, golden.Base(in)+".golden", b.String())
		})
	}
}

func TestValidProgram(t *testing.T) {
	src := `let free = user.tier == "free"
let n = 2 * 3
route "/a/*" {
  require auth
  when free && n > 5 && cidr_match(client.ip, "10.0.0.0/8") { limit 10 per minute by user.id }
  when query("debug") != "" || !(request.method == "GET") { deny 403 }
  set_header "X-Tier" user.tier
}`
	prog, pd := parser.Parse(src)
	if len(pd) > 0 {
		t.Fatal(pd)
	}
	if _, diags := Check(prog); len(diags) > 0 {
		t.Fatalf("unexpected: %v", diags)
	}
}
