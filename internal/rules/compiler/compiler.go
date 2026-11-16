// Package compiler turns an optimized route body into bytecode for the VM:
// a flat instruction slice plus constant and prefix pools. Every jump is
// forward (the language has no loops), so the number of instructions
// executed per evaluation is at most len(Code), which the analyzer uses as a
// proven bound.
//
// Two techniques keep dispatch counts low:
//   - Conditions compile in jump mode: `when a && b` becomes conditional
//     jumps, with no booleans pushed on the stack.
//   - Superinstructions cover the dominant shapes: field == "const",
//     header("X") != "const" and cidr_match(field, "const"), each as one
//     instruction (or one fused compare-and-jump).
package compiler

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// Op is a VM opcode.
type Op uint8

const (
	OpPushConst   Op = iota // push Consts[A]
	OpLoadField             // push field A
	OpCallBuiltin           // pop args, push builtin A's result
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpNeg
	OpNot
	OpAnd         // value mode &&: if top is false jump to A keeping it, else pop
	OpOr          // value mode ||: if top is true jump to A keeping it, else pop
	OpJump        // jump to A
	OpJumpIfFalse // pop; jump to A if false
	OpJumpIfTrue  // pop; jump to A if true

	// superinstructions (value mode): push a bool
	OpFieldEqConst  // push field A == Consts[B]
	OpFieldNeConst  // push field A != Consts[B]
	OpHeaderEqConst // push header(Consts[A]) == Consts[B]
	OpHeaderNeConst // push header(Consts[A]) != Consts[B]
	OpCIDRField     // push cidr_match(field A, Prefixes[B])

	// fused compare-and-jump (jump mode): jump to C when the test holds
	OpJumpIfFieldEq  // field A == Consts[B]
	OpJumpIfFieldNe  // field A != Consts[B]
	OpJumpIfHeaderEq // header(Consts[A]) == Consts[B]
	OpJumpIfHeaderNe // header(Consts[A]) != Consts[B]
	OpJumpIfCIDR     // cidr_match(field A, Prefixes[B])
	OpJumpIfNotCIDR  // !cidr_match(field A, Prefixes[B])

	OpRequireAuth // deny 401 and return if unauthenticated
	OpLimit       // pop key; record Limits[A]
	OpDeny        // deny with status A and return
	OpSetHeader   // pop value; record header named Consts[A]
	OpBudget      // pop key; record Budgets[A]
	OpReturn
	NumOps
)

var opNames = [...]string{
	"PUSH_CONST", "LOAD_FIELD", "CALL_BUILTIN", "EQ", "NE", "LT", "LE", "GT", "GE",
	"ADD", "SUB", "MUL", "DIV", "NEG", "NOT", "AND", "OR", "JUMP", "JUMP_IF_FALSE", "JUMP_IF_TRUE",
	"FIELD_EQ_CONST", "FIELD_NE_CONST", "HEADER_EQ_CONST", "HEADER_NE_CONST", "CIDR_FIELD",
	"JUMP_IF_FIELD_EQ", "JUMP_IF_FIELD_NE", "JUMP_IF_HEADER_EQ", "JUMP_IF_HEADER_NE", "JUMP_IF_CIDR", "JUMP_IF_NOT_CIDR",
	"REQUIRE_AUTH", "LIMIT", "DENY", "SET_HEADER", "BUDGET", "RETURN",
}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return fmt.Sprintf("OP(%d)", o)
}

// IsFusedJump reports whether the op is a compare-and-jump with target C.
func (o Op) IsFusedJump() bool { return o >= OpJumpIfFieldEq && o <= OpJumpIfNotCIDR }

// Instr is one instruction with up to three operands.
type Instr struct {
	Op      Op
	A, B, C int32
}

// LimitInfo is the static part of a limit statement.
type LimitInfo struct {
	ID    int
	Count int64
	Unit  ast.Unit
	Algo  uint8
}

// BudgetInfo is the static part of a budget statement.
type BudgetInfo struct {
	ID     int
	Amount int64
	Unit   ast.Unit
}

// Prefix is a pre-parsed constant CIDR (OK=false: invalid, never matches).
type Prefix struct {
	P  netip.Prefix
	OK bool
}

// Chunk is a compiled route body.
type Chunk struct {
	Code     []Instr
	Consts   []env.Value
	Limits   []LimitInfo
	Budgets  []BudgetInfo
	Prefixes []Prefix
	MaxStack int // deepest stack the code can reach
}

// MaxStack is the VM's fixed stack size; deeper code is rejected at compile time.
const MaxStack = 64

type compiler struct {
	ch     *Chunk
	consts map[env.Value]int32
	depth  int
	diags  []ast.Diagnostic
}

// Compile compiles one optimized, type-checked route.
func Compile(r *ast.Route) (*Chunk, []ast.Diagnostic) {
	c := &compiler{ch: &Chunk{}, consts: map[env.Value]int32{}}
	c.stmts(r.Body)
	c.emit(OpReturn, 0, 0, 0)
	if c.ch.MaxStack > MaxStack {
		c.diags = append(c.diags, ast.Errorf("compiler", r.Pos,
			"route %q needs a stack of %d values; the VM allows %d", r.Pattern, c.ch.MaxStack, MaxStack))
	}
	return c.ch, c.diags
}

func (c *compiler) emit(op Op, a, b, cc int32) int {
	c.ch.Code = append(c.ch.Code, Instr{Op: op, A: a, B: b, C: cc})
	return len(c.ch.Code) - 1
}

// patch points a jump at the current position.
func (c *compiler) patch(at int) {
	in := &c.ch.Code[at]
	if in.Op.IsFusedJump() {
		in.C = int32(len(c.ch.Code))
	} else {
		in.A = int32(len(c.ch.Code))
	}
}

func (c *compiler) patchAll(at []int) {
	for _, j := range at {
		c.patch(j)
	}
}

func (c *compiler) push(n int) {
	c.depth += n
	if c.depth > c.ch.MaxStack {
		c.ch.MaxStack = c.depth
	}
}

func (c *compiler) constant(v env.Value) int32 {
	if i, ok := c.consts[v]; ok {
		return i
	}
	i := int32(len(c.ch.Consts))
	c.ch.Consts = append(c.ch.Consts, v)
	c.consts[v] = i
	return i
}

func (c *compiler) str(s string) int32 { return c.constant(env.StringVal(s)) }

func (c *compiler) prefix(call *ast.Call) int32 {
	c.ch.Prefixes = append(c.ch.Prefixes, Prefix{P: call.Prefix, OK: call.PrefixOK})
	return int32(len(c.ch.Prefixes) - 1)
}

func (c *compiler) stmts(ss []ast.Stmt) {
	for _, s := range ss {
		switch s := s.(type) {
		case *ast.RequireAuth:
			c.emit(OpRequireAuth, 0, 0, 0)
		case *ast.When:
			falseJumps := c.jump(s.Cond, false)
			c.stmts(s.Body)
			c.patchAll(falseJumps)
		case *ast.Limit:
			c.expr(s.Key)
			c.ch.Limits = append(c.ch.Limits, LimitInfo{ID: s.ID, Count: s.Count, Unit: s.Unit, Algo: s.Algo})
			c.emit(OpLimit, int32(len(c.ch.Limits)-1), 0, 0)
			c.push(-1)
		case *ast.Deny:
			c.emit(OpDeny, int32(s.Status), 0, 0)
		case *ast.SetHeader:
			c.expr(s.Value)
			c.emit(OpSetHeader, c.str(s.Name), 0, 0)
			c.push(-1)
		case *ast.Budget:
			c.expr(s.Key)
			c.ch.Budgets = append(c.ch.Budgets, BudgetInfo{ID: s.ID, Amount: s.Amount, Unit: s.Unit})
			c.emit(OpBudget, int32(len(c.ch.Budgets)-1), 0, 0)
			c.push(-1)
		}
	}
}

// ---- pattern matching for superinstructions ----

// fieldConst matches `field op "const"` in either operand order.
func fieldConst(b *ast.Binary) (*ast.Field, *ast.StringLit, bool) {
	if f, ok := b.X.(*ast.Field); ok {
		if s, ok := b.Y.(*ast.StringLit); ok {
			return f, s, true
		}
	}
	if f, ok := b.Y.(*ast.Field); ok {
		if s, ok := b.X.(*ast.StringLit); ok {
			return f, s, true
		}
	}
	return nil, nil, false
}

// headerConst matches `header("Name") op "const"` in either operand order.
func headerConst(b *ast.Binary) (name, val *ast.StringLit, ok bool) {
	match := func(x, y ast.Expr) bool {
		call, ok := x.(*ast.Call)
		if !ok || call.Builtin != int(env.BuiltinHeader) || len(call.Args) != 1 {
			return false
		}
		n, ok1 := call.Args[0].(*ast.StringLit)
		v, ok2 := y.(*ast.StringLit)
		if ok1 && ok2 {
			name, val = n, v
			return true
		}
		return false
	}
	ok = match(b.X, b.Y) || match(b.Y, b.X)
	return
}

// cidrField matches cidr_match(field, "const prefix").
func cidrField(e ast.Expr) (*ast.Call, *ast.Field, bool) {
	call, ok := e.(*ast.Call)
	if !ok || call.Builtin != int(env.BuiltinCIDRMatch) || !call.ConstPrefix {
		return nil, nil, false
	}
	f, ok := call.Args[0].(*ast.Field)
	return call, f, ok
}

// ---- jump mode ----

// jump compiles cond for control flow only. The returned jumps (patched by
// the caller) are taken when cond == when; otherwise execution falls
// through. Nothing is left on the stack.
func (c *compiler) jump(e ast.Expr, when bool) []int {
	switch e := e.(type) {
	case *ast.Binary:
		switch e.Op {
		case ast.OpAnd, ast.OpOr:
			// Jumping when the whole && is false (or the whole || is true)
			// means jumping as soon as either side decides it.
			decidesEarly := (e.Op == ast.OpAnd) == !when
			if decidesEarly {
				return append(c.jump(e.X, when), c.jump(e.Y, when)...)
			}
			// Otherwise the left side short-circuits past the right side.
			skip := c.jump(e.X, !when)
			out := c.jump(e.Y, when)
			c.patchAll(skip)
			return out
		case ast.OpEq, ast.OpNe:
			// jump when (x == y) == want
			want := (e.Op == ast.OpEq) == when
			if f, s, ok := fieldConst(e); ok {
				op := OpJumpIfFieldNe
				if want {
					op = OpJumpIfFieldEq
				}
				return []int{c.emit(op, int32(f.ID), c.str(s.Value), 0)}
			}
			if n, v, ok := headerConst(e); ok {
				op := OpJumpIfHeaderNe
				if want {
					op = OpJumpIfHeaderEq
				}
				return []int{c.emit(op, c.str(n.Value), c.str(v.Value), 0)}
			}
		}
	case *ast.Unary:
		if e.Op == ast.OpNot {
			return c.jump(e.X, !when)
		}
	case *ast.Call:
		if call, f, ok := cidrField(e); ok {
			op := OpJumpIfNotCIDR
			if when {
				op = OpJumpIfCIDR
			}
			return []int{c.emit(op, int32(f.ID), c.prefix(call), 0)}
		}
	}
	c.expr(e)
	op := OpJumpIfFalse
	if when {
		op = OpJumpIfTrue
	}
	j := c.emit(op, 0, 0, 0)
	c.push(-1)
	return []int{j}
}

// ---- value mode ----

var binOps = map[ast.Op]Op{
	ast.OpEq: OpEq, ast.OpNe: OpNe, ast.OpLt: OpLt, ast.OpLe: OpLe, ast.OpGt: OpGt, ast.OpGe: OpGe,
	ast.OpAdd: OpAdd, ast.OpSub: OpSub, ast.OpMul: OpMul, ast.OpDiv: OpDiv,
}

// expr compiles e so that it leaves exactly one value on the stack.
func (c *compiler) expr(e ast.Expr) {
	switch e := e.(type) {
	case *ast.IntLit:
		c.emit(OpPushConst, c.constant(env.IntVal(e.Value)), 0, 0)
		c.push(1)
	case *ast.StringLit:
		c.emit(OpPushConst, c.str(e.Value), 0, 0)
		c.push(1)
	case *ast.BoolLit:
		c.emit(OpPushConst, c.constant(env.BoolVal(e.Value)), 0, 0)
		c.push(1)
	case *ast.Field:
		if e.ID < 0 {
			c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: field %s.%s not resolved (type checker not run)", e.Obj, e.Name))
		}
		c.emit(OpLoadField, int32(e.ID), 0, 0)
		c.push(1)
	case *ast.Call:
		if e.Builtin < 0 {
			c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: call %s not resolved (type checker not run)", e.Fn))
		}
		if call, f, ok := cidrField(e); ok {
			c.emit(OpCIDRField, int32(f.ID), c.prefix(call), 0)
			c.push(1)
			return
		}
		for _, a := range e.Args {
			c.expr(a)
		}
		c.emit(OpCallBuiltin, int32(e.Builtin), 0, 0)
		c.push(1 - len(e.Args))
	case *ast.Unary:
		c.expr(e.X)
		if e.Op == ast.OpNot {
			c.emit(OpNot, 0, 0, 0)
		} else {
			c.emit(OpNeg, 0, 0, 0)
		}
	case *ast.Binary:
		switch e.Op {
		case ast.OpAnd, ast.OpOr:
			c.expr(e.X)
			op := OpAnd
			if e.Op == ast.OpOr {
				op = OpOr
			}
			j := c.emit(op, 0, 0, 0)
			c.push(-1) // on fall-through the left value is popped
			c.expr(e.Y)
			c.patch(j)
			return
		case ast.OpEq, ast.OpNe:
			eq := e.Op == ast.OpEq
			if f, s, ok := fieldConst(e); ok {
				op := OpFieldNeConst
				if eq {
					op = OpFieldEqConst
				}
				c.emit(op, int32(f.ID), c.str(s.Value), 0)
				c.push(1)
				return
			}
			if n, v, ok := headerConst(e); ok {
				op := OpHeaderNeConst
				if eq {
					op = OpHeaderEqConst
				}
				c.emit(op, c.str(n.Value), c.str(v.Value), 0)
				c.push(1)
				return
			}
		}
		c.expr(e.X)
		c.expr(e.Y)
		c.emit(binOps[e.Op], 0, 0, 0)
		c.push(-1)
	case *ast.Ident:
		// Lets are inlined by the optimizer; reaching here is a bug.
		c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: un-inlined name %q", e.Name))
	}
}

// Disassemble prints a chunk, used in golden tests and debugging.
func (ch *Chunk) Disassemble() string {
	var b strings.Builder
	str := func(i int32) string {
		if i < 0 || int(i) >= len(ch.Consts) {
			return "?"
		}
		v := ch.Consts[i]
		switch v.T {
		case env.String:
			return fmt.Sprintf("%q", v.S)
		case env.Bool:
			return fmt.Sprint(v.Bool())
		}
		return fmt.Sprint(v.I)
	}
	pfx := func(i int32) string {
		if i < 0 || int(i) >= len(ch.Prefixes) {
			return "?"
		}
		if !ch.Prefixes[i].OK {
			return "<invalid>"
		}
		return ch.Prefixes[i].P.String()
	}
	for i, in := range ch.Code {
		line := fmt.Sprintf("%04d %s", i, in.Op)
		switch in.Op {
		case OpPushConst, OpSetHeader:
			line += " " + str(in.A)
		case OpBudget:
			bi := ch.Budgets[in.A]
			line += fmt.Sprintf(" #%d %d/%s", bi.ID, bi.Amount, bi.Unit)
		case OpLimit:
			l := ch.Limits[in.A]
			line += fmt.Sprintf(" #%d %d/%s", l.ID, l.Count, l.Unit)
			if l.Algo != 0 {
				line += " " + ast.AlgoNames[l.Algo]
			}
		case OpLoadField, OpCallBuiltin, OpDeny, OpAnd, OpOr, OpJump, OpJumpIfFalse, OpJumpIfTrue:
			line += fmt.Sprintf(" %d", in.A)
		case OpFieldEqConst, OpFieldNeConst:
			line += fmt.Sprintf(" field%d %s", in.A, str(in.B))
		case OpHeaderEqConst, OpHeaderNeConst:
			line += fmt.Sprintf(" %s %s", str(in.A), str(in.B))
		case OpCIDRField:
			line += fmt.Sprintf(" field%d %s", in.A, pfx(in.B))
		case OpJumpIfFieldEq, OpJumpIfFieldNe:
			line += fmt.Sprintf(" field%d %s -> %d", in.A, str(in.B), in.C)
		case OpJumpIfHeaderEq, OpJumpIfHeaderNe:
			line += fmt.Sprintf(" %s %s -> %d", str(in.A), str(in.B), in.C)
		case OpJumpIfCIDR, OpJumpIfNotCIDR:
			line += fmt.Sprintf(" field%d %s -> %d", in.A, pfx(in.B), in.C)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
