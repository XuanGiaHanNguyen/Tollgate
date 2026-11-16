package lexer

import (
	"os"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/internal/golden"
)

func TestGolden(t *testing.T) {
	for _, in := range golden.Inputs(t) {
		t.Run(golden.Base(in), func(t *testing.T) {
			src, _ := os.ReadFile(in)
			toks, diags := All(string(src))
			var b strings.Builder
			for _, tk := range toks {
				b.WriteString(tk.String() + "\n")
			}
			for _, d := range diags {
				b.WriteString(d.String() + "\n")
			}
			golden.Check(t, golden.Base(in)+".golden", b.String())
		})
	}
}

func TestPositions(t *testing.T) {
	toks, _ := All("let x =\n  \"a\" # comment\n  42")
	want := []struct {
		kind      Kind
		line, col int
	}{{Let, 1, 1}, {Ident, 1, 5}, {Assign, 1, 7}, {String, 2, 3}, {Int, 3, 3}, {EOF, 3, 5}}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens: %v", len(toks), toks)
	}
	for i, w := range want {
		if toks[i].Kind != w.kind || toks[i].Pos.Line != w.line || toks[i].Pos.Col != w.col {
			t.Errorf("token %d = %v, want %v at %d:%d", i, toks[i], w.kind, w.line, w.col)
		}
	}
}

func TestEOFRepeats(t *testing.T) {
	l := New("")
	for i := 0; i < 3; i++ {
		if k := l.Next().Kind; k != EOF {
			t.Fatalf("got %v", k)
		}
	}
}

// FuzzLexer: the lexer never panics, always ends with EOF, and makes progress.
func FuzzLexer(f *testing.F) {
	for _, s := range []string{"", "let x = 1", `route "/a/*" { deny 403 }`, "\"\\", "&|!", "\xff", "99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, _ := All(src)
		if toks[len(toks)-1].Kind != EOF {
			t.Fatal("no EOF")
		}
		if len(toks) > len(src)+1 {
			t.Fatalf("%d tokens from %d bytes", len(toks), len(src))
		}
	})
}
