// Package analysis finds rules that are legal but wrong, at compile time:
// rules shadowed by an earlier deny, unreachable statements, duplicate and
// overlapping routes with conflicting limits or a dropped `require auth`,
// unused lets, and routes whose worst-case instruction count exceeds the VM
// budget (provable because the language has no loops).
package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
)

const stage = "analysis"

// Pattern is a parsed route pattern: "/a/b/*" is Prefix "/a/b" with
// Wildcard; "/a/b" is an exact match; "/*" matches every path.
type Pattern struct {
	Prefix   string
	Wildcard bool
}

// ParsePattern validates a route pattern.
func ParsePattern(s string) (Pattern, error) {
	if !strings.HasPrefix(s, "/") {
		return Pattern{}, fmt.Errorf("route pattern %q must start with /", s)
	}
	if strings.HasSuffix(s, "/*") {
		p := Pattern{Prefix: strings.TrimSuffix(s, "/*"), Wildcard: true}
		if strings.Contains(p.Prefix, "*") {
			return Pattern{}, fmt.Errorf("route pattern %q: '*' is only allowed as the final segment", s)
		}
		return p, nil
	}
	if strings.Contains(s, "*") {
		return Pattern{}, fmt.Errorf("route pattern %q: '*' is only allowed as the final segment (\"/x/*\")", s)
	}
	return Pattern{Prefix: s}, nil
}

// Match reports whether path matches the pattern (segment-aware).
func (p Pattern) Match(path string) bool {
	if !p.Wildcard {
		return path == p.Prefix
	}
	if p.Prefix == "" {
		return true
	}
	return path == p.Prefix || strings.HasPrefix(path, p.Prefix+"/")
}

// Covers reports whether every path matching q also matches p.
func (p Pattern) Covers(q Pattern) bool {
	if !p.Wildcard {
		return !q.Wildcard && p.Prefix == q.Prefix
	}
	return p.Match(q.Prefix)
}

// MoreSpecific orders patterns: longer prefix first, exact before wildcard.
func MoreSpecific(a, b Pattern) bool {
	if len(a.Prefix) != len(b.Prefix) {
		return len(a.Prefix) > len(b.Prefix)
	}
	return !a.Wildcard && b.Wildcard
}

// Analyze checks the unoptimized program. lets maps names to declarations.
func Analyze(prog *ast.Program, lets map[string]*ast.Let) []ast.Diagnostic {
	a := &analyzer{lets: lets, used: map[string]bool{}}
	routes := prog.Routes()
	pats := make([]Pattern, len(routes))
	ok := make([]bool, len(routes))
	for i, r := range routes {
		p, err := ParsePattern(r.Pattern)
		if err != nil {
			a.errorf(r.Pos, "%v", err)
			continue
		}
		pats[i], ok[i] = p, true
		a.block(r.Body)
	}
	a.routeConflicts(routes, pats, ok)

	// Unused lets: also count uses from other lets.
	for _, d := range prog.Decls {
		if l, ok := d.(*ast.Let); ok {
			a.markUses(l.Value)
		}
	}
	for _, d := range prog.Decls {
		if l, ok := d.(*ast.Let); ok && !a.used[l.Name] {
			a.warnf(l.Pos, "let %q is never used", l.Name)
		}
	}
	sort.SliceStable(a.diags, func(i, j int) bool {
		pi, pj := a.diags[i].Pos, a.diags[j].Pos
		return pi.Line < pj.Line || pi.Line == pj.Line && pi.Col < pj.Col
	})
	return a.diags
}

// CheckBound reports a route whose bytecode length (an upper bound on the
// instructions executed, since all jumps go forward) exceeds the budget.
func CheckBound(r *ast.Route, codeLen, budget int) []ast.Diagnostic {
	if codeLen > budget {
		return []ast.Diagnostic{ast.Errorf(stage, r.Pos,
			"route %q may execute up to %d instructions, over the VM budget of %d", r.Pattern, codeLen, budget)}
	}
	return nil
}

type analyzer struct {
	lets  map[string]*ast.Let
	used  map[string]bool
	diags []ast.Diagnostic
}

func (a *analyzer) errorf(pos ast.Pos, format string, args ...any) {
	a.diags = append(a.diags, ast.Errorf(stage, pos, format, args...))
}

func (a *analyzer) warnf(pos ast.Pos, format string, args ...any) {
	a.diags = append(a.diags, ast.Warnf(stage, pos, format, args...))
}

type denyCond struct {
	cond ast.Expr // let-expanded
	pos  ast.Pos
}

// block checks one statement list and reports whether it always denies.
func (a *analyzer) block(ss []ast.Stmt) bool {
	var denies []denyCond
	for i, s := range ss {
		a.markStmtUses(s)
		switch s := s.(type) {
		case *ast.Deny:
			if i+1 < len(ss) {
				a.warnf(ss[i+1].At(), "unreachable: follows the unconditional deny at %s", s.Pos)
				for _, rest := range ss[i+1:] {
					a.markStmtUses(rest)
				}
			}
			return true
		case *ast.When:
			cond := a.expand(s.Cond)
			for _, d := range denies {
				if implies(cond, d.cond) {
					a.warnf(s.Pos, "shadowed rule: this when can never take effect because the when at %s already denies every request it matches", d.pos)
					break
				}
			}
			if a.block(s.Body) {
				denies = append(denies, denyCond{cond, s.Pos})
			}
		}
	}
	return false
}

// expand replaces let names with their definitions so `free_tier` and
// `user.tier == "free"` compare equal.
func (a *analyzer) expand(e ast.Expr) ast.Expr {
	return a.expandDepth(e, 0)
}

func (a *analyzer) expandDepth(e ast.Expr, depth int) ast.Expr {
	if depth > 100 {
		return e
	}
	switch e := e.(type) {
	case *ast.Ident:
		if l, ok := a.lets[e.Name]; ok {
			return a.expandDepth(l.Value, depth+1)
		}
	case *ast.Unary:
		return &ast.Unary{Pos: e.Pos, Op: e.Op, X: a.expandDepth(e.X, depth+1)}
	case *ast.Binary:
		return &ast.Binary{Pos: e.Pos, Op: e.Op, X: a.expandDepth(e.X, depth+1), Y: a.expandDepth(e.Y, depth+1)}
	case *ast.Call:
		args := make([]ast.Expr, len(e.Args))
		for i, x := range e.Args {
			args[i] = a.expandDepth(x, depth+1)
		}
		return &ast.Call{Pos: e.Pos, Fn: e.Fn, Args: args, Builtin: e.Builtin}
	}
	return e
}

// implies is a syntactic check that p being true guarantees q is true:
// equal expressions, p = (A && B) where A or B implies q, or q = (A || B)
// where p implies A or B. Sound but incomplete.
func implies(p, q ast.Expr) bool {
	if ast.FormatExpr(p) == ast.FormatExpr(q) {
		return true
	}
	if b, ok := p.(*ast.Binary); ok && b.Op == ast.OpAnd {
		if implies(b.X, q) || implies(b.Y, q) {
			return true
		}
	}
	if b, ok := q.(*ast.Binary); ok && b.Op == ast.OpOr {
		return implies(p, b.X) || implies(p, b.Y)
	}
	return false
}

func (a *analyzer) markStmtUses(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.When:
		a.markUses(s.Cond)
	case *ast.Limit:
		a.markUses(s.Key)
	case *ast.SetHeader:
		a.markUses(s.Value)
	}
}

func (a *analyzer) markUses(e ast.Expr) {
	switch e := e.(type) {
	case *ast.Ident:
		a.used[e.Name] = true
	case *ast.Unary:
		a.markUses(e.X)
	case *ast.Binary:
		a.markUses(e.X)
		a.markUses(e.Y)
	case *ast.Call:
		for _, x := range e.Args {
			a.markUses(x)
		}
	}
}

// ---- route conflicts ----

type limitSig struct {
	key   string
	unit  ast.Unit
	count int64
	pos   ast.Pos
}

func (a *analyzer) limits(ss []ast.Stmt, out *[]limitSig) {
	for _, s := range ss {
		switch s := s.(type) {
		case *ast.Limit:
			*out = append(*out, limitSig{ast.FormatExpr(a.expand(s.Key)), s.Unit, s.Count, s.Pos})
		case *ast.When:
			a.limits(s.Body, out)
		}
	}
}

func requiresAuth(ss []ast.Stmt) bool {
	for _, s := range ss {
		if _, ok := s.(*ast.RequireAuth); ok {
			return true
		}
	}
	return false
}

func (a *analyzer) routeConflicts(routes []*ast.Route, pats []Pattern, ok []bool) {
	for j := range routes {
		if !ok[j] {
			continue
		}
		for i := 0; i < j; i++ {
			if !ok[i] {
				continue
			}
			ri, rj := routes[i], routes[j]
			pi, pj := pats[i], pats[j]
			if pi == pj {
				a.errorf(rj.Pos, "route %q duplicates the route at %s", rj.Pattern, ri.Pos)
				continue
			}
			// Find which one is broader; the more specific route replaces the
			// broader one's policy for the paths it covers.
			var broad, narrow *ast.Route
			switch {
			case pi.Covers(pj):
				broad, narrow = ri, rj
			case pj.Covers(pi):
				broad, narrow = rj, ri
			default:
				continue
			}
			var bl, nl []limitSig
			a.limits(broad.Body, &bl)
			a.limits(narrow.Body, &nl)
			for _, n := range nl {
				for _, b := range bl {
					if n.key == b.key && n.unit == b.unit && n.count != b.count {
						a.warnf(n.pos, "conflicting limits: route %q allows %d per %s by %s, but the overlapping route %q (at %s) allows %d; for paths under %q only %d applies",
							narrow.Pattern, n.count, n.unit, n.key, broad.Pattern, broad.Pos, b.count, narrow.Pattern, n.count)
					}
				}
			}
			if requiresAuth(broad.Body) && !requiresAuth(narrow.Body) {
				a.warnf(narrow.Pos, "route %q overlaps %q (at %s), which requires auth, but does not require auth itself; requests under %q skip authentication",
					narrow.Pattern, broad.Pattern, broad.Pos, narrow.Pattern)
			}
		}
	}
}
