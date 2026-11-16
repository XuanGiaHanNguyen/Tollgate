// Package vm runs compiled rule bytecode on a stack machine. The value
// stack is a fixed-size array on the Go stack and values are plain structs,
// so a run allocates nothing. Every run has an instruction budget so a bad
// rule cannot hang a gateway.
//
// Validate is a bytecode verifier (in the spirit of the JVM's): it proves
// operand ranges, forward-only jumps and the exact stack depth at every
// instruction. Run relies on that proof and does no per-instruction stack or
// operand checks; a recover() turns any panic on unverified code into
// ErrBadBytecode instead of crashing the gateway.
package vm

import (
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/compiler"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// DefaultBudget is the per-evaluation instruction budget.
const DefaultBudget = 10_000

// Validate verifies a chunk. Code the compiler produces always passes; the
// check guards against anything else (e.g. corrupted config from a peer).
func Validate(ch *compiler.Chunk) error {
	n := len(ch.Code)
	if n == 0 || ch.Code[n-1].Op != compiler.OpReturn || ch.MaxStack > compiler.MaxStack {
		return env.ErrBadBytecode
	}
	isStr := func(i int32) bool { return i >= 0 && int(i) < len(ch.Consts) && ch.Consts[i].T == env.String }
	isConst := func(i int32) bool { return i >= 0 && int(i) < len(ch.Consts) }
	isField := func(i int32) bool { return i >= 0 && int(i) < env.NumFields }
	isPrefix := func(i int32) bool { return i >= 0 && int(i) < len(ch.Prefixes) }

	// depth[pc] is the proven stack depth before pc; -1 = not reached yet.
	depth := make([]int, n)
	for i := range depth {
		depth[i] = -1
	}
	depth[0] = 0
	flow := func(target, d int) bool { // propagate d to target
		if target < 0 || target >= n || d < 0 || d > compiler.MaxStack {
			return false
		}
		if depth[target] == -1 {
			depth[target] = d
		}
		return depth[target] == d // merge points must agree
	}

	for pc := 0; pc < n; pc++ {
		d := depth[pc]
		if d == -1 {
			continue // unreachable
		}
		in := ch.Code[pc]
		jumpOK := func(t int32) bool { return int(t) > pc } // forward only
		need, delta, target, targetDelta, falls := 0, 0, -1, 0, true

		switch in.Op {
		case compiler.OpPushConst:
			if !isConst(in.A) {
				return env.ErrBadBytecode
			}
			delta = 1
		case compiler.OpLoadField:
			if !isField(in.A) {
				return env.ErrBadBytecode
			}
			delta = 1
		case compiler.OpCallBuiltin:
			if in.A < 0 || int(in.A) >= len(env.BuiltinArity) {
				return env.ErrBadBytecode
			}
			need = env.BuiltinArity[in.A]
			delta = 1 - need
		case compiler.OpEq, compiler.OpNe, compiler.OpLt, compiler.OpLe, compiler.OpGt, compiler.OpGe,
			compiler.OpAdd, compiler.OpSub, compiler.OpMul, compiler.OpDiv:
			need, delta = 2, -1
		case compiler.OpNeg, compiler.OpNot:
			need = 1
		case compiler.OpAnd, compiler.OpOr:
			if !jumpOK(in.A) {
				return env.ErrBadBytecode
			}
			need, delta, target, targetDelta = 1, -1, int(in.A), 0
		case compiler.OpJump:
			if !jumpOK(in.A) {
				return env.ErrBadBytecode
			}
			target, falls = int(in.A), false
		case compiler.OpJumpIfFalse, compiler.OpJumpIfTrue:
			if !jumpOK(in.A) {
				return env.ErrBadBytecode
			}
			need, delta, target, targetDelta = 1, -1, int(in.A), -1
		case compiler.OpFieldEqConst, compiler.OpFieldNeConst:
			if !isField(in.A) || !isStr(in.B) {
				return env.ErrBadBytecode
			}
			delta = 1
		case compiler.OpHeaderEqConst, compiler.OpHeaderNeConst:
			if !isStr(in.A) || !isStr(in.B) {
				return env.ErrBadBytecode
			}
			delta = 1
		case compiler.OpCIDRField:
			if !isField(in.A) || !isPrefix(in.B) {
				return env.ErrBadBytecode
			}
			delta = 1
		case compiler.OpJumpIfFieldEq, compiler.OpJumpIfFieldNe:
			if !isField(in.A) || !isStr(in.B) || !jumpOK(in.C) {
				return env.ErrBadBytecode
			}
			target = int(in.C)
		case compiler.OpJumpIfHeaderEq, compiler.OpJumpIfHeaderNe:
			if !isStr(in.A) || !isStr(in.B) || !jumpOK(in.C) {
				return env.ErrBadBytecode
			}
			target = int(in.C)
		case compiler.OpJumpIfCIDR, compiler.OpJumpIfNotCIDR:
			if !isField(in.A) || !isPrefix(in.B) || !jumpOK(in.C) {
				return env.ErrBadBytecode
			}
			target = int(in.C)
		case compiler.OpRequireAuth:
		case compiler.OpLimit:
			if in.A < 0 || int(in.A) >= len(ch.Limits) {
				return env.ErrBadBytecode
			}
			need, delta = 1, -1
		case compiler.OpSetHeader:
			if !isStr(in.A) {
				return env.ErrBadBytecode
			}
			need, delta = 1, -1
		case compiler.OpDeny, compiler.OpReturn:
			falls = false
		default:
			return env.ErrBadBytecode
		}
		if d < need {
			return env.ErrBadBytecode
		}
		if target >= 0 && !flow(target, d+targetDelta) {
			return env.ErrBadBytecode
		}
		if falls && !flow(pc+1, d+delta) {
			return env.ErrBadBytecode
		}
	}
	return nil
}

// Run evaluates a validated chunk against r and writes the outcome into d
// (reset first). budget <= 0 means DefaultBudget.
func Run(ch *compiler.Chunk, r *env.Request, budget int, d *env.Decision) {
	*d = env.Decision{}
	if budget <= 0 {
		budget = DefaultBudget
	}
	defer func() {
		if recover() != nil { // only reachable with unverified bytecode
			d.Err = env.ErrBadBytecode
		}
	}()

	var stack [compiler.MaxStack]env.Value
	sp := 0
	code, consts := ch.Code, ch.Consts
	steps := 0

	for pc := 0; ; {
		if steps == budget {
			d.Err, d.Steps = env.ErrBudget, steps
			return
		}
		steps++
		in := &code[pc]
		pc++
		switch in.Op {
		case compiler.OpJumpIfFieldEq:
			if r.Field(env.FieldID(in.A)) == consts[in.B].S {
				pc = int(in.C)
			}
		case compiler.OpJumpIfFieldNe:
			if r.Field(env.FieldID(in.A)) != consts[in.B].S {
				pc = int(in.C)
			}
		case compiler.OpJumpIfHeaderEq:
			if env.Header(r.Header, consts[in.A].S) == consts[in.B].S {
				pc = int(in.C)
			}
		case compiler.OpJumpIfHeaderNe:
			if env.Header(r.Header, consts[in.A].S) != consts[in.B].S {
				pc = int(in.C)
			}
		case compiler.OpJumpIfCIDR, compiler.OpJumpIfNotCIDR:
			p := &ch.Prefixes[in.B]
			if r.CIDRMatchPrefix(r.Field(env.FieldID(in.A)), p.P, p.OK) == (in.Op == compiler.OpJumpIfCIDR) {
				pc = int(in.C)
			}
		case compiler.OpFieldEqConst:
			stack[sp] = env.BoolVal(r.Field(env.FieldID(in.A)) == consts[in.B].S)
			sp++
		case compiler.OpFieldNeConst:
			stack[sp] = env.BoolVal(r.Field(env.FieldID(in.A)) != consts[in.B].S)
			sp++
		case compiler.OpHeaderEqConst:
			stack[sp] = env.BoolVal(env.Header(r.Header, consts[in.A].S) == consts[in.B].S)
			sp++
		case compiler.OpHeaderNeConst:
			stack[sp] = env.BoolVal(env.Header(r.Header, consts[in.A].S) != consts[in.B].S)
			sp++
		case compiler.OpCIDRField:
			p := &ch.Prefixes[in.B]
			stack[sp] = env.BoolVal(r.CIDRMatchPrefix(r.Field(env.FieldID(in.A)), p.P, p.OK))
			sp++
		case compiler.OpPushConst:
			stack[sp] = consts[in.A]
			sp++
		case compiler.OpLoadField:
			stack[sp] = env.StringVal(r.Field(env.FieldID(in.A)))
			sp++
		case compiler.OpCallBuiltin:
			n := env.BuiltinArity[in.A]
			res := env.Call(env.BuiltinID(in.A), stack[sp-n:sp], r)
			sp -= n
			stack[sp] = res
			sp++
		case compiler.OpEq:
			sp--
			stack[sp-1] = env.BoolVal(stack[sp-1].Equal(stack[sp]))
		case compiler.OpNe:
			sp--
			stack[sp-1] = env.BoolVal(!stack[sp-1].Equal(stack[sp]))
		case compiler.OpLt:
			sp--
			stack[sp-1] = env.BoolVal(stack[sp-1].I < stack[sp].I)
		case compiler.OpLe:
			sp--
			stack[sp-1] = env.BoolVal(stack[sp-1].I <= stack[sp].I)
		case compiler.OpGt:
			sp--
			stack[sp-1] = env.BoolVal(stack[sp-1].I > stack[sp].I)
		case compiler.OpGe:
			sp--
			stack[sp-1] = env.BoolVal(stack[sp-1].I >= stack[sp].I)
		case compiler.OpAdd:
			sp--
			stack[sp-1] = env.IntVal(stack[sp-1].I + stack[sp].I)
		case compiler.OpSub:
			sp--
			stack[sp-1] = env.IntVal(stack[sp-1].I - stack[sp].I)
		case compiler.OpMul:
			sp--
			stack[sp-1] = env.IntVal(stack[sp-1].I * stack[sp].I)
		case compiler.OpDiv:
			sp--
			if stack[sp].I == 0 {
				d.Err, d.Steps = env.ErrDivZero, steps
				return
			}
			stack[sp-1] = env.IntVal(stack[sp-1].I / stack[sp].I)
		case compiler.OpNeg:
			stack[sp-1].I = -stack[sp-1].I
		case compiler.OpNot:
			stack[sp-1] = env.BoolVal(!stack[sp-1].Bool())
		case compiler.OpAnd:
			if !stack[sp-1].Bool() {
				pc = int(in.A)
			} else {
				sp--
			}
		case compiler.OpOr:
			if stack[sp-1].Bool() {
				pc = int(in.A)
			} else {
				sp--
			}
		case compiler.OpJump:
			pc = int(in.A)
		case compiler.OpJumpIfFalse:
			sp--
			if !stack[sp].Bool() {
				pc = int(in.A)
			}
		case compiler.OpJumpIfTrue:
			sp--
			if stack[sp].Bool() {
				pc = int(in.A)
			}
		case compiler.OpRequireAuth:
			if !r.Authenticated {
				d.Deny, d.Steps = 401, steps
				return
			}
		case compiler.OpLimit:
			sp--
			l := &ch.Limits[in.A]
			if !d.AddLimit(env.LimitReq{ID: l.ID, Count: l.Count, Per: l.Unit.Duration(), Key: stack[sp].S}) {
				d.Err, d.Steps = env.ErrTooMany, steps
				return
			}
		case compiler.OpSetHeader:
			sp--
			if !d.AddHeader(env.HeaderSet{Name: consts[in.A].S, Value: stack[sp].S}) {
				d.Err, d.Steps = env.ErrTooMany, steps
				return
			}
		case compiler.OpDeny:
			d.Deny, d.Steps = int(in.A), steps
			return
		case compiler.OpReturn:
			d.Steps = steps
			return
		default:
			d.Err, d.Steps = env.ErrBadBytecode, steps
			return
		}
	}
}
