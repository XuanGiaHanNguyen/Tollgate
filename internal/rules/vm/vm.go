// Package vm runs compiled rule bytecode on a stack machine. The value
// stack is a fixed-size array on the Go stack and values are plain structs,
// so a run allocates nothing. Every run has an instruction budget so a bad
// rule cannot hang a gateway.
package vm

import (
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/compiler"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// DefaultBudget is the per-evaluation instruction budget.
const DefaultBudget = 10_000

// Validate checks that bytecode is well-formed: operands in range and jumps
// forward and inside the code. The compiler only emits valid code; Validate
// guards the VM against anything else (e.g. corrupted config from a peer).
func Validate(ch *compiler.Chunk) error {
	n := int32(len(ch.Code))
	if n == 0 || ch.Code[n-1].Op != compiler.OpReturn || ch.MaxStack > compiler.MaxStack {
		return env.ErrBadBytecode
	}
	for i, in := range ch.Code {
		switch in.Op {
		case compiler.OpPushConst, compiler.OpSetHeader:
			if in.Arg < 0 || int(in.Arg) >= len(ch.Consts) {
				return env.ErrBadBytecode
			}
			if in.Op == compiler.OpSetHeader && ch.Consts[in.Arg].T != env.String {
				return env.ErrBadBytecode
			}
		case compiler.OpLimit:
			if in.Arg < 0 || int(in.Arg) >= len(ch.Limits) {
				return env.ErrBadBytecode
			}
		case compiler.OpAnd, compiler.OpOr, compiler.OpJump, compiler.OpJumpIfFalse:
			if in.Arg <= int32(i) || in.Arg >= n {
				return env.ErrBadBytecode
			}
		case compiler.OpLoadField, compiler.OpCallBuiltin:
			if in.Arg < 0 || in.Arg > 255 {
				return env.ErrBadBytecode
			}
		default:
			if in.Op >= compiler.Op(len(opValid)) || !opValid[in.Op] {
				return env.ErrBadBytecode
			}
		}
	}
	return nil
}

var opValid = func() []bool {
	v := make([]bool, 64)
	for op := compiler.OpPushConst; op <= compiler.OpReturn; op++ {
		v[op] = true
	}
	return v
}()

// builtinArity must match env's built-in table.
var builtinArity = [...]int{env.BuiltinHeader: 1, env.BuiltinQuery: 1, env.BuiltinCIDRMatch: 2}

// Run evaluates ch against r and writes the outcome into d (reset first).
// budget <= 0 means DefaultBudget.
func Run(ch *compiler.Chunk, r *env.Request, budget int, d *env.Decision) {
	*d = env.Decision{}
	if budget <= 0 {
		budget = DefaultBudget
	}
	var stack [compiler.MaxStack]env.Value
	sp := 0 // next free slot
	code := ch.Code
	steps := 0

	for pc := 0; pc < len(code); {
		if steps >= budget {
			d.Err = env.ErrBudget
			d.Steps = steps
			return
		}
		steps++
		in := code[pc]
		pc++
		switch in.Op {
		case compiler.OpPushConst:
			if sp == len(stack) {
				d.Err = env.ErrStack
				break
			}
			stack[sp] = ch.Consts[in.Arg]
			sp++
		case compiler.OpLoadField:
			if sp == len(stack) {
				d.Err = env.ErrStack
				break
			}
			stack[sp] = env.StringVal(r.Field(env.FieldID(in.Arg)))
			sp++
		case compiler.OpCallBuiltin:
			if int(in.Arg) >= len(builtinArity) {
				d.Err = env.ErrBadBytecode
				break
			}
			n := builtinArity[in.Arg]
			if sp < n {
				d.Err = env.ErrBadBytecode
				break
			}
			res := env.Call(env.BuiltinID(in.Arg), stack[sp-n:sp], r)
			sp -= n
			stack[sp] = res
			sp++
		case compiler.OpEq, compiler.OpNe, compiler.OpLt, compiler.OpLe, compiler.OpGt, compiler.OpGe,
			compiler.OpAdd, compiler.OpSub, compiler.OpMul, compiler.OpDiv:
			if sp < 2 {
				d.Err = env.ErrBadBytecode
				break
			}
			a, b := stack[sp-2], stack[sp-1]
			sp--
			var v env.Value
			switch in.Op {
			case compiler.OpEq:
				v = env.BoolVal(a.Equal(b))
			case compiler.OpNe:
				v = env.BoolVal(!a.Equal(b))
			case compiler.OpLt:
				v = env.BoolVal(a.I < b.I)
			case compiler.OpLe:
				v = env.BoolVal(a.I <= b.I)
			case compiler.OpGt:
				v = env.BoolVal(a.I > b.I)
			case compiler.OpGe:
				v = env.BoolVal(a.I >= b.I)
			case compiler.OpAdd:
				v = env.IntVal(a.I + b.I)
			case compiler.OpSub:
				v = env.IntVal(a.I - b.I)
			case compiler.OpMul:
				v = env.IntVal(a.I * b.I)
			case compiler.OpDiv:
				if b.I == 0 {
					d.Err = env.ErrDivZero
					break
				}
				v = env.IntVal(a.I / b.I)
			}
			stack[sp-1] = v
		case compiler.OpNeg:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			stack[sp-1].I = -stack[sp-1].I
		case compiler.OpNot:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			stack[sp-1] = env.BoolVal(!stack[sp-1].Bool())
		case compiler.OpAnd, compiler.OpOr:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			if stack[sp-1].Bool() == (in.Op == compiler.OpOr) {
				pc = int(in.Arg) // keep the deciding value
			} else {
				sp--
			}
		case compiler.OpJump:
			pc = int(in.Arg)
		case compiler.OpJumpIfFalse:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			sp--
			if !stack[sp].Bool() {
				pc = int(in.Arg)
			}
		case compiler.OpRequireAuth:
			if !r.Authenticated {
				d.Deny = 401
				d.Steps = steps
				return
			}
		case compiler.OpLimit:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			sp--
			l := ch.Limits[in.Arg]
			if !d.AddLimit(env.LimitReq{ID: l.ID, Count: l.Count, Per: l.Unit.Duration(), Key: stack[sp].S}) {
				d.Err = env.ErrTooMany
			}
		case compiler.OpDeny:
			d.Deny = int(in.Arg)
			d.Steps = steps
			return
		case compiler.OpSetHeader:
			if sp < 1 {
				d.Err = env.ErrBadBytecode
				break
			}
			sp--
			if !d.AddHeader(env.HeaderSet{Name: ch.Consts[in.Arg].S, Value: stack[sp].S}) {
				d.Err = env.ErrTooMany
			}
		case compiler.OpReturn:
			d.Steps = steps
			return
		default:
			d.Err = env.ErrBadBytecode
		}
		if d.Err != nil {
			d.Steps = steps
			return
		}
	}
	d.Steps = steps
}
