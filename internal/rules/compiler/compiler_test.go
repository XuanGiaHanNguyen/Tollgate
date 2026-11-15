package compiler

import (
	"os"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/internal/golden"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/opt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
)

func TestGoldenDisassembly(t *testing.T) {
	for _, in := range golden.Inputs(t) {
		t.Run(golden.Base(in), func(t *testing.T) {
			src, _ := os.ReadFile(in)
			prog, d := parser.Parse(string(src))
			if len(d) > 0 {
				t.Fatal(d)
			}
			o, _ := opt.Optimize(prog)
			var b strings.Builder
			for _, r := range o.Routes() {
				ch, cd := Compile(r)
				if len(cd) > 0 {
					t.Fatal(cd)
				}
				b.WriteString("route " + r.Pattern + "\n" + ch.Disassemble())
			}
			golden.Check(t, golden.Base(in)+".golden", b.String())
		})
	}
}

func TestForwardJumpsOnly(t *testing.T) {
	src, _ := os.ReadFile("testdata/spec_example.tg")
	prog, _ := parser.Parse(string(src))
	o, _ := opt.Optimize(prog)
	for _, r := range o.Routes() {
		ch, _ := Compile(r)
		for i, in := range ch.Code {
			switch in.Op {
			case OpJump, OpJumpIfFalse, OpAnd, OpOr:
				if int(in.Arg) <= i {
					t.Fatalf("backward jump at %d", i)
				}
			}
		}
	}
}

func TestStackLimit(t *testing.T) {
	// Right-nested == keeps every left operand on the stack while the right
	// side is evaluated (&& and || would pop it).
	expr := strings.Repeat(`(header("a") == "x") == (`, 70) + "true" + strings.Repeat(")", 70)
	prog, d := parser.Parse(`route "/a" { when ` + expr + ` { deny 403 } }`)
	if len(d) > 0 {
		t.Fatal(d)
	}
	o, _ := opt.Optimize(prog)
	_, cd := Compile(o.Routes()[0])
	if len(cd) == 0 || !strings.Contains(cd[0].Msg, "stack") {
		t.Fatalf("diags = %v", cd)
	}
}
