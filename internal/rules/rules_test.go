package rules

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

const specExample = `let free_tier = user.tier == "free"

route "/api/ai/*" {
  require auth
  when free_tier        { limit 10 per minute by user.id }
  when user.tier == "pro" { limit 200 per minute by user.id }
  when header("X-Debug") != "" { deny 403 }
}

route "/api/public/*" {
  limit 30 per minute by client.ip
}
`

func TestCompileAndEvalSpecExample(t *testing.T) {
	rs, err := Compile(specExample, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Warnings) != 0 {
		t.Fatalf("warnings: %v", rs.Warnings)
	}
	tests := []struct {
		path string
		req  env.Request
		deny int
		key  string
	}{
		{"/api/ai/chat", env.Request{}, 401, ""},
		{"/api/ai/chat", env.Request{Authenticated: true, UserID: "u7", UserTier: "free"}, 0, "u7"},
		{"/api/ai/chat", env.Request{Authenticated: true, UserID: "u7", UserTier: "pro", Header: http.Header{"X-Debug": {"on"}}}, 403, "u7"},
		{"/api/public/x", env.Request{ClientIP: "1.2.3.4"}, 0, "1.2.3.4"},
	}
	for _, tc := range tests {
		r := rs.Match(tc.path)
		if r == nil {
			t.Fatalf("no route for %s", tc.path)
		}
		for _, e := range []Engine{VM, Interp} {
			var d env.Decision
			rs.Eval(r, e, &tc.req, &d)
			if d.Err != nil || d.Deny != tc.deny {
				t.Errorf("%s %v: %+v", tc.path, e, d)
			}
			if tc.key != "" && (d.NLimits == 0 || d.Limits[0].Key != tc.key) {
				t.Errorf("%s %v: limits %+v", tc.path, e, d.Limits[:d.NLimits])
			}
		}
	}
	if rs.Match("/other") != nil {
		t.Error("matched /other")
	}
}

func TestCompileErrorsListEverything(t *testing.T) {
	_, err := Compile(`route "/a" { when user.tier { deny 99 } limit 0 per minute by 5 }`, Options{})
	var ce *CompileError
	if !errors.As(err, &ce) || len(ce.Diags) != 4 {
		t.Fatalf("err = %v", err)
	}
}

func TestBoundRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString(`route "/a" {` + "\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "  when header(\"h%d\") == \"x\" { set_header \"X-%d\" \"y\" }\n", i, i)
	}
	b.WriteString("}\n")
	if _, err := Compile(b.String(), Options{Budget: 100}); err == nil || !strings.Contains(err.Error(), "over the VM budget") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Compile(b.String(), Options{Budget: 10000}); err != nil {
		t.Fatal(err)
	}
}

func TestMostSpecificRouteWins(t *testing.T) {
	rs, err := Compile(`route "/*" { deny 404 }
route "/api/*" { deny 401 }
route "/api/ai/*" { deny 402 }
route "/api/ai" { deny 403 }`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/x": 404, "/api/x": 401, "/api/ai/x": 402, "/api/ai": 403} {
		var d env.Decision
		rs.Eval(rs.Match(path), VM, &env.Request{}, &d)
		if d.Deny != want {
			t.Errorf("%s -> %d, want %d", path, d.Deny, want)
		}
	}
}

// ---- differential testing ----

// gen builds random well-typed programs. Ints come only from literals (as in
// real programs), divisions use non-zero literal divisors.
type gen struct {
	r       *rand.Rand
	strLets []string
	intLets []string
	boolLet []string
	b       strings.Builder
}

var (
	strConsts  = []string{`""`, `"free"`, `"pro"`, `"u1"`, `"10.1.2.3"`, `"GET"`, `"x"`}
	strFields  = []string{"user.id", "user.tier", "client.ip", "request.method", "request.path", "request.host"}
	headerArgs = []string{`"X-A"`, `"x-b"`, `"X-Debug"`}
	queryArgs  = []string{`"q"`, `"debug"`}
	cidrs      = []string{`"10.0.0.0/8"`, `"192.168.0.0/16"`, `"bad"`}
)

func (g *gen) pick(xs []string) string { return xs[g.r.IntN(len(xs))] }

func (g *gen) strExpr(depth int) string {
	switch n := g.r.IntN(6); {
	case n == 0 && len(g.strLets) > 0:
		return g.pick(g.strLets)
	case n == 1:
		return g.pick(strFields)
	case n == 2:
		return "header(" + g.pick(headerArgs) + ")"
	case n == 3:
		return "query(" + g.pick(queryArgs) + ")"
	default:
		return g.pick(strConsts)
	}
}

func (g *gen) intExpr(depth int) string {
	if depth <= 0 || g.r.IntN(3) == 0 {
		if len(g.intLets) > 0 && g.r.IntN(3) == 0 {
			return g.pick(g.intLets)
		}
		return fmt.Sprint(g.r.IntN(20) - 5)
	}
	switch g.r.IntN(5) {
	case 0:
		return "(" + g.intExpr(depth-1) + " + " + g.intExpr(depth-1) + ")"
	case 1:
		return "(" + g.intExpr(depth-1) + " - " + g.intExpr(depth-1) + ")"
	case 2:
		return "(" + g.intExpr(depth-1) + " * " + g.intExpr(depth-1) + ")"
	case 3:
		return "(" + g.intExpr(depth-1) + " / " + fmt.Sprint(g.r.IntN(5)+1) + ")"
	default:
		return "-" + g.intExpr(depth-1)
	}
}

func (g *gen) boolExpr(depth int) string {
	if depth <= 0 {
		switch g.r.IntN(4) {
		case 0:
			if len(g.boolLet) > 0 {
				return g.pick(g.boolLet)
			}
			return "true"
		case 1:
			return g.pick([]string{"true", "false"})
		case 2:
			return "(" + g.strExpr(0) + " == " + g.strExpr(0) + ")"
		default:
			return "cidr_match(client.ip, " + g.pick(cidrs) + ")"
		}
	}
	switch g.r.IntN(7) {
	case 0:
		return "(" + g.boolExpr(depth-1) + " && " + g.boolExpr(depth-1) + ")"
	case 1:
		return "(" + g.boolExpr(depth-1) + " || " + g.boolExpr(depth-1) + ")"
	case 2:
		return "!" + g.boolExpr(depth-1)
	case 3:
		return "(" + g.strExpr(depth-1) + " != " + g.strExpr(depth-1) + ")"
	case 4:
		op := g.pick([]string{"<", "<=", ">", ">=", "==", "!="})
		return "(" + g.intExpr(depth-1) + " " + op + " " + g.intExpr(depth-1) + ")"
	case 5:
		return "(" + g.boolExpr(depth-1) + " == " + g.boolExpr(depth-1) + ")"
	default:
		return g.boolExpr(0)
	}
}

func (g *gen) stmts(depth, n int, indent string) {
	for i := 0; i < n; i++ {
		switch g.r.IntN(7) {
		case 0:
			g.b.WriteString(indent + "require auth\n")
		case 1, 2:
			if depth > 0 {
				g.b.WriteString(indent + "when " + g.boolExpr(3) + " {\n")
				g.stmts(depth-1, g.r.IntN(4), indent+"  ")
				g.b.WriteString(indent + "}\n")
				continue
			}
			fallthrough
		case 3:
			fmt.Fprintf(&g.b, "%slimit %d per %s by %s\n", indent, g.r.IntN(100)+1,
				g.pick([]string{"second", "minute", "hour", "day"}), g.strExpr(1))
		case 4:
			if g.r.IntN(3) == 0 {
				fmt.Fprintf(&g.b, "%sdeny %d\n", indent, 400+g.r.IntN(200))
				continue
			}
			fallthrough
		default:
			fmt.Fprintf(&g.b, "%sset_header \"X-H%d\" %s\n", indent, g.r.IntN(3), g.strExpr(1))
		}
	}
}

func (g *gen) program() string {
	for i := 0; i < g.r.IntN(4); i++ {
		name := fmt.Sprintf("l%d", i)
		switch g.r.IntN(3) {
		case 0:
			fmt.Fprintf(&g.b, "let %s = %s\n", name, g.strExpr(1))
			g.strLets = append(g.strLets, name)
		case 1:
			fmt.Fprintf(&g.b, "let %s = %s\n", name, g.intExpr(2))
			g.intLets = append(g.intLets, name)
		default:
			fmt.Fprintf(&g.b, "let %s = %s\n", name, g.boolExpr(2))
			g.boolLet = append(g.boolLet, name)
		}
	}
	for i := 0; i < 1+g.r.IntN(3); i++ {
		fmt.Fprintf(&g.b, "route \"/r%d/*\" {\n", i)
		g.stmts(3, 1+g.r.IntN(6), "  ")
		g.b.WriteString("}\n")
	}
	return g.b.String()
}

func randomRequest(r *rand.Rand) *env.Request {
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	h := http.Header{}
	for _, k := range []string{"X-A", "X-B", "X-Debug"} {
		if r.IntN(2) == 0 {
			h.Set(k, pick("", "x", "1", "free"))
		}
	}
	return &env.Request{
		Authenticated: r.IntN(2) == 0,
		UserID:        pick("", "u1", "u2"),
		UserTier:      pick("", "free", "pro"),
		ClientIP:      pick("10.1.2.3", "192.168.1.1", "8.8.8.8", "not-an-ip", "::ffff:10.0.0.1"),
		Method:        pick("GET", "POST"),
		Path:          pick("/r0/x", "/r1"),
		Host:          pick("a", "b"),
		Header:        h,
		RawQuery:      pick("", "q=x", "debug=1&q=free", "q=%66ree", "q"),
	}
}

func checkDifferential(t *testing.T, seed uint64) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	g := &gen{r: r}
	src := g.program()
	rs, err := Compile(src, Options{Budget: 1_000_000})
	if err != nil {
		t.Fatalf("seed %d: generated program does not compile: %v\n%s", seed, err, src)
	}
	for i := 0; i < 30; i++ {
		req := randomRequest(r)
		for _, route := range rs.Routes {
			var src0, opt, vm env.Decision
			rs.EvalSource(route, req, &src0)
			rs.Eval(route, Interp, req, &opt)
			rs.Eval(route, VM, req, &vm)
			if !src0.SameOutcome(&opt) || !opt.SameOutcome(&vm) {
				t.Fatalf("seed %d route %s request %+v\nsource interp: %+v\noptimized interp: %+v\nvm: %+v\nprogram:\n%s\nbytecode:\n%s",
					seed, route.Pattern, req, src0, opt, vm, src, route.Chunk.Disassemble())
			}
		}
	}
}

// TestDifferential: the interpreter on the unoptimized source, the
// interpreter on the optimized AST, and the VM agree on random programs.
func TestDifferential(t *testing.T) {
	n := 3000
	if testing.Short() {
		n = 300
	}
	for seed := uint64(1); seed <= uint64(n); seed++ {
		checkDifferential(t, seed)
	}
}

func FuzzDifferential(f *testing.F) {
	for s := uint64(0); s < 20; s++ {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed uint64) { checkDifferential(t, seed) })
}

// FuzzVMBudget: for any source that compiles, the VM never executes more
// instructions than the proven bound (code length) or the budget.
func FuzzVMBudget(f *testing.F) {
	f.Add(specExample, uint64(1))
	f.Add(`route "/a" { when header("x") == "1" || query("q") == "" { deny 403 } set_header "X" user.id }`, uint64(2))
	f.Fuzz(func(t *testing.T, src string, seed uint64) {
		const budget = 500
		rs, err := Compile(src, Options{Budget: budget})
		if err != nil {
			return
		}
		r := rand.New(rand.NewPCG(seed, 1))
		for _, route := range rs.Routes {
			for i := 0; i < 5; i++ {
				var d env.Decision
				rs.Eval(route, VM, randomRequest(r), &d)
				if d.Steps > len(route.Chunk.Code) || d.Steps > budget {
					t.Fatalf("executed %d instructions; bound %d, budget %d", d.Steps, len(route.Chunk.Code), budget)
				}
				if d.Err != nil && !errors.Is(d.Err, env.ErrTooMany) {
					t.Fatalf("unexpected VM error %v", d.Err)
				}
			}
		}
	})
}
