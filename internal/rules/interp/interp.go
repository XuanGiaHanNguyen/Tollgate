// Package interp is a tree-walking evaluator over the rule AST. It exists as
// the baseline for experiment E2 and as the oracle in differential tests
// against the VM. It accepts both optimized ASTs and unoptimized ones (with
// let bindings), so the optimizer can be tested too.
package interp

import (
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// Lets maps let names to their (unevaluated) expressions; nil for optimized
// programs where lets are inlined.
type Lets map[string]ast.Expr

type evaluator struct {
	r      *env.Request
	lets   Lets
	d      *env.Decision
	budget int
	done   bool // deny or error reached
}

// Eval runs a route body against r and writes the outcome into d (reset
// first). Steps counts AST nodes visited. budget <= 0 means unlimited.
func Eval(body []ast.Stmt, lets Lets, r *env.Request, budget int, d *env.Decision) {
	*d = env.Decision{}
	e := evaluator{r: r, lets: lets, d: d, budget: budget}
	e.stmts(body)
}

func (e *evaluator) step() bool {
	e.d.Steps++
	if e.budget > 0 && e.d.Steps > e.budget {
		e.fail(env.ErrBudget)
		return false
	}
	return true
}

func (e *evaluator) fail(err error) {
	if !e.done {
		e.d.Err = err
		e.done = true
	}
}

func (e *evaluator) stmts(ss []ast.Stmt) {
	for _, s := range ss {
		if e.done || !e.step() {
			return
		}
		switch s := s.(type) {
		case *ast.RequireAuth:
			if !e.r.Authenticated {
				e.d.Deny = 401
				e.done = true
			}
		case *ast.When:
			c := e.expr(s.Cond)
			if e.done {
				return
			}
			if c.Bool() {
				e.stmts(s.Body)
			}
		case *ast.Limit:
			k := e.expr(s.Key)
			if e.done {
				return
			}
			if !e.d.AddLimit(env.LimitReq{ID: s.ID, Count: s.Count, Per: s.Unit.Duration(), Key: k.S, Algo: s.Algo}) {
				e.fail(env.ErrTooMany)
			}
		case *ast.Deny:
			e.d.Deny = int(s.Status)
			e.done = true
		case *ast.Budget:
			k := e.expr(s.Key)
			if e.done {
				return
			}
			if !e.d.AddBudget(env.BudgetReq{ID: s.ID, Amount: s.Amount, Per: s.Unit.Duration(), Key: k.S}) {
				e.fail(env.ErrTooMany)
			}
		case *ast.SetHeader:
			v := e.expr(s.Value)
			if e.done {
				return
			}
			if !e.d.AddHeader(env.HeaderSet{Name: s.Name, Value: v.S}) {
				e.fail(env.ErrTooMany)
			}
		}
	}
}

func (e *evaluator) expr(x ast.Expr) env.Value {
	if e.done || !e.step() {
		return env.Value{}
	}
	switch x := x.(type) {
	case *ast.IntLit:
		return env.IntVal(x.Value)
	case *ast.StringLit:
		return env.StringVal(x.Value)
	case *ast.BoolLit:
		return env.BoolVal(x.Value)
	case *ast.Ident:
		return e.expr(e.lets[x.Name])
	case *ast.Field:
		return e.r.FieldValue(env.FieldID(x.ID))
	case *ast.Call:
		if x.ConstPrefix {
			ip := e.expr(x.Args[0])
			e.step() // the constant prefix node
			if e.done {
				return env.Value{}
			}
			return env.BoolVal(e.r.CIDRMatchPrefix(ip.S, x.Prefix, x.PrefixOK))
		}
		var buf [2]env.Value
		args := buf[:0]
		for _, a := range x.Args {
			args = append(args, e.expr(a))
		}
		if e.done {
			return env.Value{}
		}
		return env.Call(env.BuiltinID(x.Builtin), args, e.r)
	case *ast.Unary:
		v := e.expr(x.X)
		if x.Op == ast.OpNot {
			return env.BoolVal(!v.Bool())
		}
		return env.IntVal(-v.I)
	case *ast.Binary:
		a := e.expr(x.X)
		switch x.Op {
		case ast.OpAnd:
			if !a.Bool() {
				return a
			}
			return e.expr(x.Y)
		case ast.OpOr:
			if a.Bool() {
				return a
			}
			return e.expr(x.Y)
		}
		b := e.expr(x.Y)
		if e.done {
			return env.Value{}
		}
		switch x.Op {
		case ast.OpEq:
			return env.BoolVal(a.Equal(b))
		case ast.OpNe:
			return env.BoolVal(!a.Equal(b))
		case ast.OpLt:
			return env.BoolVal(a.I < b.I)
		case ast.OpLe:
			return env.BoolVal(a.I <= b.I)
		case ast.OpGt:
			return env.BoolVal(a.I > b.I)
		case ast.OpGe:
			return env.BoolVal(a.I >= b.I)
		case ast.OpAdd:
			return env.IntVal(a.I + b.I)
		case ast.OpSub:
			return env.IntVal(a.I - b.I)
		case ast.OpMul:
			return env.IntVal(a.I * b.I)
		case ast.OpDiv:
			if b.I == 0 {
				e.fail(env.ErrDivZero)
				return env.Value{}
			}
			return env.IntVal(a.I / b.I)
		}
	}
	return env.Value{}
}
