package opt

import (
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
)

func optimize(t *testing.T, src string) (string, []ast.Diagnostic) {
	t.Helper()
	prog, d := parser.Parse(src)
	if len(d) > 0 {
		t.Fatal(d)
	}
	out, diags := Optimize(prog)
	return ast.Format(out), diags
}

func TestOptimize(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"inline let and fold", `let n = 2 * 3 + 1
let big = n > 5
route "/a" { when big { deny 403 } }`, "(route \"/a\"\n  (deny 403)\n)\n"},
		{"when false dropped", `route "/a" { when 1 > 2 { deny 403 } limit 5 per second by user.id }`,
			"(route \"/a\"\n  (limit#0 5 per second by user.id)\n)\n"},
		{"after deny removed", `route "/a" { deny 403 limit 5 per second by user.id set_header "X" "y" }`,
			"(route \"/a\"\n  (deny 403)\n)\n"},
		{"deny inside when true ends block", `route "/a" { when true { deny 401 } deny 403 }`,
			"(route \"/a\"\n  (deny 401)\n)\n"},
		{"and/or with constants", `let t = true
route "/a" { when t && user.id == "x" { deny 403 } when user.id == "y" || false { deny 404 } when user.id == "z" && false { deny 405 } }`,
			"(route \"/a\"\n  (when (== user.id \"x\")\n    (deny 403)\n  )\n  (when (== user.id \"y\")\n    (deny 404)\n  )\n)\n"},
		{"string compare folds", `route "/a" { when "a" == "a" { set_header "X" "1" } when "a" != "a" { deny 403 } }`,
			"(route \"/a\"\n  (set_header \"X\" \"1\")\n)\n"},
		{"double negation", `route "/a" { when !!(user.id == "x") { deny 403 } }`,
			"(route \"/a\"\n  (when (== user.id \"x\")\n    (deny 403)\n  )\n)\n"},
		{"empty when dropped", `route "/a" { when user.id == "x" { when false { deny 403 } } }`, "(route \"/a\"\n)\n"},
		{"header name canonicalized", `route "/a" { when header("x-debug") != "" { deny 403 } }`,
			"(route \"/a\"\n  (when (!= (call header \"X-Debug\") \"\")\n    (deny 403)\n  )\n)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := optimize(t, tc.src)
			if len(diags) > 0 {
				t.Fatal(diags)
			}
			if got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestDivisionByZero(t *testing.T) {
	_, diags := optimize(t, `let z = 2 - 2
route "/a" { when 10 / z == 1 { deny 403 } }`)
	if len(diags) != 1 || !strings.Contains(diags[0].Msg, "division by zero") {
		t.Fatalf("diags = %v", diags)
	}
}

func TestOriginalUnchanged(t *testing.T) {
	prog, _ := parser.Parse(`let n = 1 + 1
route "/a" { when n == 2 { deny 403 } }`)
	before := ast.Format(prog)
	Optimize(prog)
	if ast.Format(prog) != before {
		t.Fatal("Optimize modified its input")
	}
}
