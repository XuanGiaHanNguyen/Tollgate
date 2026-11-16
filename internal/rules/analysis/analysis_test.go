package analysis

import (
	"os"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/internal/golden"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/types"
)

func TestGolden(t *testing.T) {
	for _, in := range golden.Inputs(t) {
		t.Run(golden.Base(in), func(t *testing.T) {
			src, _ := os.ReadFile(in)
			prog, pd := parser.Parse(string(src))
			info, td := types.Check(prog)
			if len(pd)+len(td) > 0 {
				t.Fatalf("front-end errors: %v %v", pd, td)
			}
			var b strings.Builder
			for _, d := range Analyze(prog, info.Lets) {
				b.WriteString(d.String() + "\n")
			}
			golden.Check(t, golden.Base(in)+".golden", b.String())
		})
	}
}

func TestPatterns(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"/api/*", "/api", true},
		{"/api/*", "/api/x/y", true},
		{"/api/*", "/apix", false},
		{"/api", "/api", true},
		{"/api", "/api/x", false},
		{"/*", "/anything", true},
	}
	for _, tc := range tests {
		p, err := ParsePattern(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Match(tc.path); got != tc.want {
			t.Errorf("%s matches %s = %v", tc.pattern, tc.path, got)
		}
	}
	a, _ := ParsePattern("/api/*")
	b, _ := ParsePattern("/api/ai/*")
	c, _ := ParsePattern("/api/ai")
	if !a.Covers(b) || !a.Covers(c) || b.Covers(a) || c.Covers(b) {
		t.Error("Covers wrong")
	}
	if !MoreSpecific(b, a) || !MoreSpecific(c, b) {
		t.Error("MoreSpecific wrong")
	}
}
