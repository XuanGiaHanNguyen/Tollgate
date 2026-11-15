// Package opt rewrites a type-checked program into an equivalent smaller one:
// inline let bindings, fold constants, drop `when false` blocks, splice
// `when true` blocks, and remove statements after an unconditional deny.
//
// Every int in the language comes from a literal (fields and built-ins never
// return ints), so all integer arithmetic folds at compile time; a constant
// division by zero is reported here and can never happen at runtime. All
// built-ins are pure, so dropping or reordering their calls is safe.
package opt

import (
	"net/netip"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

const stage = "opt"

type optimizer struct {
	lets  map[string]ast.Expr // name -> already-optimized value
	diags []ast.Diagnostic
}

// Optimize returns a new program containing only routes; prog is not
// modified, so the original stays available as a test oracle.
func Optimize(prog *ast.Program) (*ast.Program, []ast.Diagnostic) {
	o := &optimizer{lets: map[string]ast.Expr{}}
	out := &ast.Program{}
	for _, d := range prog.Decls {
		switch d := d.(type) {
		case *ast.Let:
			o.lets[d.Name] = o.expr(d.Value)
		case *ast.Route:
			out.Decls = append(out.Decls, &ast.Route{Pos: d.Pos, Pattern: d.Pattern, Body: o.stmts(d.Body)})
		}
	}
	return out, o.diags
}

func (o *optimizer) stmts(ss []ast.Stmt) []ast.Stmt {
	out, _ := o.block(ss)
	return out
}

// block optimizes a statement list. The second result reports whether the
// block always ends in a deny, so its parent can drop what follows.
func (o *optimizer) block(ss []ast.Stmt) ([]ast.Stmt, bool) {
	var out []ast.Stmt
	for _, s := range ss {
		switch s := s.(type) {
		case *ast.When:
			cond := o.expr(s.Cond)
			if b, ok := cond.(*ast.BoolLit); ok {
				if !b.Value {
					continue // when false: never runs
				}
				body, denies := o.block(s.Body) // when true: splice
				out = append(out, body...)
				if denies {
					return out, true
				}
				continue
			}
			body, _ := o.block(s.Body)
			if len(body) == 0 {
				continue // pure condition, nothing to do
			}
			out = append(out, &ast.When{Pos: s.Pos, Cond: cond, Body: body})
		case *ast.Limit:
			out = append(out, &ast.Limit{Pos: s.Pos, ID: s.ID, Count: s.Count, Unit: s.Unit, Key: o.expr(s.Key)})
		case *ast.SetHeader:
			out = append(out, &ast.SetHeader{Pos: s.Pos, Name: s.Name, Value: o.expr(s.Value)})
		case *ast.Deny:
			out = append(out, &ast.Deny{Pos: s.Pos, Status: s.Status})
			return out, true // everything after is unreachable
		case *ast.RequireAuth:
			out = append(out, &ast.RequireAuth{Pos: s.Pos})
		}
	}
	return out, false
}

func (o *optimizer) expr(e ast.Expr) ast.Expr {
	switch e := e.(type) {
	case *ast.IntLit, *ast.StringLit, *ast.BoolLit, *ast.Field:
		return e
	case *ast.Ident:
		if v, ok := o.lets[e.Name]; ok {
			return v // optimized values are immutable, so sharing is safe
		}
		return e
	case *ast.Call:
		args := make([]ast.Expr, len(e.Args))
		for i, a := range e.Args {
			args[i] = o.expr(a)
		}
		// header("x-debug") -> header("X-Debug"): runtime lookup becomes a
		// direct map access.
		if e.Fn == "header" && len(args) == 1 {
			if s, ok := args[0].(*ast.StringLit); ok {
				args[0] = &ast.StringLit{Pos: s.Pos, Value: env.CanonicalHeaderArg(s.Value)}
			}
		}
		call := &ast.Call{Pos: e.Pos, Fn: e.Fn, Args: args, Builtin: e.Builtin}
		// cidr_match(ip, "10.0.0.0/8"): parse the constant prefix once.
		if e.Fn == "cidr_match" && len(args) == 2 {
			if s, ok := args[1].(*ast.StringLit); ok {
				p, err := netip.ParsePrefix(s.Value)
				call.ConstPrefix, call.PrefixOK, call.Prefix = true, err == nil, p
			}
		}
		return call
	case *ast.Unary:
		x := o.expr(e.X)
		switch e.Op {
		case ast.OpNot:
			if b, ok := x.(*ast.BoolLit); ok {
				return &ast.BoolLit{Pos: e.Pos, Value: !b.Value}
			}
			if u, ok := x.(*ast.Unary); ok && u.Op == ast.OpNot {
				return u.X // !!x -> x
			}
		case ast.OpNeg:
			if i, ok := x.(*ast.IntLit); ok {
				return &ast.IntLit{Pos: e.Pos, Value: -i.Value}
			}
		}
		return &ast.Unary{Pos: e.Pos, Op: e.Op, X: x}
	case *ast.Binary:
		return o.binary(e)
	}
	return e
}

func (o *optimizer) binary(e *ast.Binary) ast.Expr {
	x, y := o.expr(e.X), o.expr(e.Y)
	pos := e.Pos

	// Short-circuit operators with one constant side.
	if e.Op == ast.OpAnd || e.Op == ast.OpOr {
		isAnd := e.Op == ast.OpAnd
		if b, ok := x.(*ast.BoolLit); ok {
			if b.Value == isAnd {
				return y // true && y -> y ; false || y -> y
			}
			return &ast.BoolLit{Pos: pos, Value: b.Value} // false && y ; true || y
		}
		if b, ok := y.(*ast.BoolLit); ok {
			if b.Value == isAnd {
				return x // x && true -> x ; x || false -> x
			}
			return &ast.BoolLit{Pos: pos, Value: b.Value} // pure x can be dropped
		}
		return &ast.Binary{Pos: pos, Op: e.Op, X: x, Y: y}
	}

	xi, xInt := x.(*ast.IntLit)
	yi, yInt := y.(*ast.IntLit)
	if xInt && yInt {
		a, b := xi.Value, yi.Value
		switch e.Op {
		case ast.OpAdd:
			return &ast.IntLit{Pos: pos, Value: a + b}
		case ast.OpSub:
			return &ast.IntLit{Pos: pos, Value: a - b}
		case ast.OpMul:
			return &ast.IntLit{Pos: pos, Value: a * b}
		case ast.OpDiv:
			if b == 0 {
				o.diags = append(o.diags, ast.Errorf(stage, pos, "division by zero"))
				return &ast.IntLit{Pos: pos, Value: 0}
			}
			return &ast.IntLit{Pos: pos, Value: a / b}
		case ast.OpEq:
			return &ast.BoolLit{Pos: pos, Value: a == b}
		case ast.OpNe:
			return &ast.BoolLit{Pos: pos, Value: a != b}
		case ast.OpLt:
			return &ast.BoolLit{Pos: pos, Value: a < b}
		case ast.OpLe:
			return &ast.BoolLit{Pos: pos, Value: a <= b}
		case ast.OpGt:
			return &ast.BoolLit{Pos: pos, Value: a > b}
		case ast.OpGe:
			return &ast.BoolLit{Pos: pos, Value: a >= b}
		}
	}
	if xs, ok := x.(*ast.StringLit); ok {
		if ys, ok := y.(*ast.StringLit); ok {
			switch e.Op {
			case ast.OpEq:
				return &ast.BoolLit{Pos: pos, Value: xs.Value == ys.Value}
			case ast.OpNe:
				return &ast.BoolLit{Pos: pos, Value: xs.Value != ys.Value}
			}
		}
	}
	if xb, ok := x.(*ast.BoolLit); ok {
		if yb, ok := y.(*ast.BoolLit); ok {
			switch e.Op {
			case ast.OpEq:
				return &ast.BoolLit{Pos: pos, Value: xb.Value == yb.Value}
			case ast.OpNe:
				return &ast.BoolLit{Pos: pos, Value: xb.Value != yb.Value}
			}
		}
	}
	return &ast.Binary{Pos: pos, Op: e.Op, X: x, Y: y}
}
