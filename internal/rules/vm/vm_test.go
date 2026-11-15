package vm

import (
	"net/http"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/compiler"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/opt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
)

func compile(t testing.TB, src string) *compiler.Chunk {
	t.Helper()
	prog, d := parser.Parse(src)
	if len(d) > 0 {
		t.Fatal(d)
	}
	o, _ := opt.Optimize(prog)
	ch, cd := compiler.Compile(o.Routes()[0])
	if len(cd) > 0 {
		t.Fatal(cd)
	}
	if err := Validate(ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

const spec = `route "/api/ai/*" {
  require auth
  when user.tier == "free" { limit 10 per minute by user.id }
  when user.tier == "pro" { limit 200 per minute by user.id }
  when header("X-Debug") != "" { deny 403 }
  set_header "X-Tier" user.tier
}`

func TestRun(t *testing.T) {
	ch := compile(t, spec)
	tests := []struct {
		name   string
		req    env.Request
		deny   int
		limits int
		count  int64
	}{
		{"unauthenticated", env.Request{}, 401, 0, 0},
		{"free", env.Request{Authenticated: true, UserID: "u1", UserTier: "free"}, 0, 1, 10},
		{"pro", env.Request{Authenticated: true, UserID: "u1", UserTier: "pro"}, 0, 1, 200},
		{"debug header", env.Request{Authenticated: true, UserTier: "pro", Header: http.Header{"X-Debug": {"1"}}}, 403, 1, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d env.Decision
			Run(ch, &tc.req, 0, &d)
			if d.Err != nil || d.Deny != tc.deny || d.NLimits != tc.limits {
				t.Fatalf("decision = %+v", d)
			}
			if tc.limits > 0 && (d.Limits[0].Count != tc.count || d.Limits[0].Key != "u1" && tc.deny == 0) {
				t.Fatalf("limit = %+v", d.Limits[0])
			}
			if tc.deny == 0 && (d.NHeaders != 1 || d.Headers[0].Value != tc.req.UserTier) {
				t.Fatalf("headers = %+v", d.Headers[:d.NHeaders])
			}
		})
	}
}

func TestBudget(t *testing.T) {
	ch := compile(t, spec)
	var d env.Decision
	Run(ch, &env.Request{Authenticated: true, UserTier: "free"}, 3, &d)
	if d.Err != env.ErrBudget || d.Steps != 3 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestValidateRejectsBadCode(t *testing.T) {
	good := compile(t, spec)
	mutate := []func(ch *compiler.Chunk){
		func(ch *compiler.Chunk) { ch.Code = nil },
		func(ch *compiler.Chunk) { ch.Code = ch.Code[:len(ch.Code)-1] }, // no RETURN
		func(ch *compiler.Chunk) { ch.Code[0] = compiler.Instr{Op: compiler.OpPushConst, Arg: 999} },
		func(ch *compiler.Chunk) { ch.Code[1] = compiler.Instr{Op: compiler.OpJump, Arg: 0} }, // backward
		func(ch *compiler.Chunk) { ch.Code[1] = compiler.Instr{Op: compiler.OpJump, Arg: 9999} },
		func(ch *compiler.Chunk) { ch.Code[0] = compiler.Instr{Op: 200} },
		func(ch *compiler.Chunk) { ch.Code[0] = compiler.Instr{Op: compiler.OpLimit, Arg: 50} },
	}
	for i, m := range mutate {
		ch := *good
		ch.Code = append([]compiler.Instr(nil), good.Code...)
		m(&ch)
		if Validate(&ch) == nil {
			t.Errorf("mutation %d accepted", i)
		}
	}
}

// Run must not panic even on unvalidated, malformed code.
func TestRunMalformedNoPanic(t *testing.T) {
	ch := &compiler.Chunk{Code: []compiler.Instr{{Op: compiler.OpEq}, {Op: compiler.OpReturn}}}
	var d env.Decision
	Run(ch, &env.Request{}, 0, &d)
	if d.Err != env.ErrBadBytecode {
		t.Fatalf("err = %v", d.Err)
	}
}

func TestZeroAllocs(t *testing.T) {
	ch := compile(t, spec)
	req := &env.Request{Authenticated: true, UserID: "u1", UserTier: "free", Header: http.Header{"X-Debug": {""}}}
	var d env.Decision
	allocs := testing.AllocsPerRun(1000, func() { Run(ch, req, 0, &d) })
	if allocs != 0 {
		t.Fatalf("Run allocates %.1f times per evaluation", allocs)
	}
}
