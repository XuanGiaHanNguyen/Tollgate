// Package types type-checks a parsed program: let bindings (defined before
// use, no redefinition), fields, built-in signatures, operator operands and
// statement requirements. It reports every error it finds.
package types

import (
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

const stage = "types"

// Info is what later stages need from type checking.
type Info struct {
	Lets map[string]*ast.Let // by name
}

type checker struct {
	lets  map[string]*ast.Let
	types map[string]env.Type
	diags []ast.Diagnostic
}

// Check type-checks prog.
func Check(prog *ast.Program) (*Info, []ast.Diagnostic) {
	c := &checker{lets: map[string]*ast.Let{}, types: map[string]env.Type{}}
	for _, d := range prog.Decls {
		switch d := d.(type) {
		case *ast.Let:
			if prev, ok := c.lets[d.Name]; ok {
				c.errorf(d.Pos, "%q already declared at %s", d.Name, prev.Pos)
				continue
			}
			if _, ok := env.LookupBuiltin(d.Name); ok {
				c.errorf(d.Pos, "%q is a built-in function and cannot be redefined", d.Name)
				continue
			}
			if d.Name == "body" {
				c.errorf(d.Pos, "\"body\" is predeclared (the request body) and cannot be redefined")
				continue
			}
			t := c.expr(d.Value)
			c.lets[d.Name] = d
			c.types[d.Name] = t
		case *ast.Route:
			c.routeSettings(d)
			c.stmts(d.Body)
		}
	}
	return &Info{Lets: c.lets}, c.diags
}

func (c *checker) errorf(pos ast.Pos, format string, a ...any) {
	c.diags = append(c.diags, ast.Errorf(stage, pos, format, a...))
}

// routeSettings checks AI route settings: top-level only, at most once.
func (c *checker) routeSettings(r *ast.Route) {
	seen := map[string]ast.Pos{}
	once := func(name string, pos ast.Pos) {
		if prev, ok := seen[name]; ok {
			c.errorf(pos, "%s already set for this route at %s", name, prev)
		}
		seen[name] = pos
	}
	var providers *ast.Providers
	var hedge *ast.Hedge
	for _, s := range r.Body {
		switch s := s.(type) {
		case *ast.Providers:
			once("providers", s.Pos)
			providers = s
			names := map[string]bool{}
			for _, n := range s.Names {
				if n == "" || names[n] {
					c.errorf(s.Pos, "provider names must be non-empty and unique")
				}
				names[n] = true
			}
		case *ast.Hedge:
			once("hedge", s.Pos)
			hedge = s
			if s.MaxPct <= 0 || s.MaxPct > 100 {
				c.errorf(s.Pos, "hedge max must be in (0, 100]%%, found %g%%", s.MaxPct)
			}
		case *ast.BlockPatterns:
			once("block_patterns", s.Pos)
			switch s.Action {
			case "redact", "block", "flag":
			default:
				c.errorf(s.Pos, "block_patterns action must be redact, block or flag, found %q", s.Action)
			}
		}
	}
	if hedge != nil && (providers == nil || len(providers.Names) < 2) {
		c.errorf(hedge.Pos, "hedge needs at least two providers on the route")
	}
}

func (c *checker) stmts(ss []ast.Stmt) { c.stmtsDepth(ss, 0) }

func (c *checker) stmtsDepth(ss []ast.Stmt, depth int) {
	for _, s := range ss {
		switch s := s.(type) {
		case *ast.Providers, *ast.Hedge, *ast.BlockPatterns:
			if depth > 0 {
				c.errorf(s.At(), "route settings (providers, hedge, block_patterns) must be at the top level of a route, not inside when")
			}
		case *ast.Budget:
			if s.Amount <= 0 {
				c.errorf(s.Pos, "budget amount must be positive")
			}
			if t := c.expr(s.Key); t != env.Invalid && t != env.String {
				c.errorf(s.Key.At(), "budget key must be a string, found %s", t)
			}
		case *ast.When:
			if t := c.expr(s.Cond); t != env.Invalid && t != env.Bool {
				c.errorf(s.Cond.At(), "when condition must be bool, found %s", t)
			}
			c.stmtsDepth(s.Body, depth+1)
		case *ast.Limit:
			if s.Count <= 0 {
				c.errorf(s.Pos, "limit count must be positive, found %d", s.Count)
			}
			if t := c.expr(s.Key); t != env.Invalid && t != env.String {
				c.errorf(s.Key.At(), "limit key must be a string, found %s", t)
			}
		case *ast.Deny:
			if s.Status < 400 || s.Status > 599 {
				c.errorf(s.Pos, "deny status must be 400-599, found %d", s.Status)
			}
		case *ast.SetHeader:
			if !validHeaderName(s.Name) {
				c.errorf(s.Pos, "invalid header name %q", s.Name)
			}
			if t := c.expr(s.Value); t != env.Invalid && t != env.String {
				c.errorf(s.Value.At(), "set_header value must be a string, found %s", t)
			}
		case *ast.RequireAuth:
		}
	}
}

// expr returns the expression's type; Invalid means an error was already
// reported, so callers stay quiet to avoid cascades.
func (c *checker) expr(e ast.Expr) env.Type {
	switch e := e.(type) {
	case *ast.IntLit:
		return env.Int
	case *ast.StringLit:
		return env.String
	case *ast.BoolLit:
		return env.Bool
	case *ast.Ident:
		t, ok := c.types[e.Name]
		if !ok {
			c.errorf(e.Pos, "undefined name %q (lets must be declared before use)", e.Name)
			return env.Invalid
		}
		return t
	case *ast.Field:
		id, t, ok := env.LookupField(e.Obj, e.Name)
		if !ok {
			c.errorf(e.Pos, "unknown field %s.%s", e.Obj, e.Name)
			return env.Invalid
		}
		e.ID = int(id)
		return t
	case *ast.Call:
		b, ok := env.LookupBuiltin(e.Fn)
		if !ok {
			c.errorf(e.Pos, "unknown function %q", e.Fn)
			for _, a := range e.Args {
				c.expr(a)
			}
			return env.Invalid
		}
		e.Builtin = int(b.ID)
		if len(e.Args) != len(b.Params) {
			c.errorf(e.Pos, "%s takes %d argument(s), found %d", b.Name, len(b.Params), len(e.Args))
		}
		for i, a := range e.Args {
			t := c.expr(a)
			if i < len(b.Params) && t != env.Invalid && t != b.Params[i] {
				c.errorf(a.At(), "argument %d of %s must be %s, found %s", i+1, b.Name, b.Params[i], t)
			}
		}
		return b.Result
	case *ast.Unary:
		t := c.expr(e.X)
		want := env.Bool
		if e.Op == ast.OpNeg {
			want = env.Int
		}
		if t != env.Invalid && t != want {
			c.errorf(e.Pos, "operator %s needs %s, found %s", e.Op, want, t)
			return env.Invalid
		}
		return want
	case *ast.Binary:
		x, y := c.expr(e.X), c.expr(e.Y)
		if x == env.Invalid || y == env.Invalid {
			return resultType(e.Op)
		}
		switch e.Op {
		case ast.OpOr, ast.OpAnd:
			if x != env.Bool || y != env.Bool {
				c.errorf(e.Pos, "operator %s needs bool operands, found %s and %s", e.Op, x, y)
				return env.Invalid
			}
		case ast.OpEq, ast.OpNe:
			if x != y {
				c.errorf(e.Pos, "cannot compare %s with %s", x, y)
				return env.Invalid
			}
		default: // ordering and arithmetic
			if x != env.Int || y != env.Int {
				c.errorf(e.Pos, "operator %s needs int operands, found %s and %s", e.Op, x, y)
				return env.Invalid
			}
		}
		return resultType(e.Op)
	}
	return env.Invalid
}

func resultType(op ast.Op) env.Type {
	switch op {
	case ast.OpAdd, ast.OpSub, ast.OpMul, ast.OpDiv:
		return env.Int
	}
	return env.Bool
}

// validHeaderName accepts RFC 7230 token characters.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '!' || c == '#' || c == '$' || c == '%' || c == '&' ||
			c == '\'' || c == '*' || c == '+' || c == '.' || c == '^' || c == '`' || c == '|' || c == '~'
		if !ok {
			return false
		}
	}
	return true
}
