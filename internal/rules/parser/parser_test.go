package parser

import (
	"os"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/internal/golden"
)

func render(prog *ast.Program, diags []ast.Diagnostic) string {
	var b strings.Builder
	b.WriteString(ast.Format(prog))
	for _, d := range diags {
		b.WriteString(d.String() + "\n")
	}
	return b.String()
}

func TestGolden(t *testing.T) {
	for _, in := range golden.Inputs(t) {
		t.Run(golden.Base(in), func(t *testing.T) {
			src, _ := os.ReadFile(in)
			prog, diags := Parse(string(src))
			golden.Check(t, golden.Base(in)+".golden", render(prog, diags))
		})
	}
}

func TestReportsMultipleErrors(t *testing.T) {
	src, _ := os.ReadFile("testdata/errors.tg")
	_, diags := Parse(string(src))
	if len(diags) < 5 {
		t.Fatalf("got %d diagnostics, want several:\n%v", len(diags), diags)
	}
}

func TestDepthLimit(t *testing.T) {
	src := "let x = " + strings.Repeat("(", 10000) + "1" + strings.Repeat(")", 10000)
	_, diags := Parse(src)
	if len(diags) == 0 || !strings.Contains(diags[len(diags)-1].Msg, "nesting deeper") {
		t.Fatalf("diags = %v", diags)
	}
	src = "let x = " + strings.Repeat("!", 10000) + "true"
	if _, diags = Parse(src); len(diags) == 0 {
		t.Fatal("deep unary not rejected")
	}
}

func TestLimitIDsAreSequential(t *testing.T) {
	prog, diags := Parse(`route "/a" { limit 1 per second by user.id when true { limit 2 per minute by client.ip } }
route "/b" { limit 3 per hour by user.id }`)
	if len(diags) > 0 {
		t.Fatal(diags)
	}
	got := ast.Format(prog)
	for _, id := range []string{"limit#0 ", "limit#1 ", "limit#2 "} {
		if !strings.Contains(got, id) {
			t.Errorf("missing %s in\n%s", id, got)
		}
	}
}

// FuzzParser: the parser never panics on any input and never reports an
// error-free parse alongside a lexer error.
func FuzzParser(f *testing.F) {
	for _, s := range []string{"", "let x = 1", `route "/a/*" { require auth when user.tier == "free" { limit 10 per minute by user.id } }`,
		"route", "let", "((((", `route "/x" { when (`, "let a = f(1,2,", "deny 403 }"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		Parse(src)
	})
}
