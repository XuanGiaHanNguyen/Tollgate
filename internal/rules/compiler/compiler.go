// Package compiler turns an optimized route body into bytecode for the VM:
// a flat instruction slice plus a constant pool. Every jump is forward
// (the language has no loops), so the number of instructions executed per
// evaluation is at most len(Code), which the analyzer uses as a proven bound.
package compiler

import (
	"fmt"
	"strings"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
)

// Op is a VM opcode.
type Op uint8

const (
	OpPushConst   Op = iota // push Consts[Arg]
	OpLoadField             // push request field Arg
	OpCallBuiltin           // pop args, push builtin Arg's result
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
	OpAnd         // if top is false jump to Arg keeping it, else pop (short-circuit &&)
	OpOr          // if top is true jump to Arg keeping it, else pop (short-circuit ||)
	OpJump        // jump to Arg
	OpJumpIfFalse // pop; jump to Arg if false
	OpRequireAuth // deny 401 and return if unauthenticated
	OpLimit       // pop key; record Limits[Arg]
	OpDeny        // deny with status Arg and return
	OpSetHeader   // pop value; record header named Consts[Arg]
	OpReturn
	numOps
)

var opNames = [...]string{
	"PUSH_CONST", "LOAD_FIELD", "CALL_BUILTIN", "EQ", "NE", "LT", "LE", "GT", "GE",
	"ADD", "SUB", "MUL", "DIV", "NEG", "NOT", "AND", "OR", "JUMP", "JUMP_IF_FALSE",
	"REQUIRE_AUTH", "LIMIT", "DENY", "SET_HEADER", "RETURN",
}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return fmt.Sprintf("OP(%d)", o)
}

// Instr is one instruction.
type Instr struct {
	Op  Op
	Arg int32
}

// LimitInfo is the static part of a limit statement.
type LimitInfo struct {
	ID    int
	Count int64
	Unit  ast.Unit
}

// Chunk is a compiled route body.
type Chunk struct {
	Code     []Instr
	Consts   []env.Value
	Limits   []LimitInfo
	MaxStack int // deepest stack the code can reach
}

// MaxStack is the VM's fixed stack size; deeper code is rejected at compile time.
const MaxStack = 64

type compiler struct {
	ch     *Chunk
	consts map[env.Value]int32
	depth  int
	diags  []ast.Diagnostic
	pos    ast.Pos
}

// Compile compiles one optimized route.
func Compile(r *ast.Route) (*Chunk, []ast.Diagnostic) {
	c := &compiler{ch: &Chunk{}, consts: map[env.Value]int32{}, pos: r.Pos}
	c.stmts(r.Body)
	c.emit(OpReturn, 0)
	if c.ch.MaxStack > MaxStack {
		c.diags = append(c.diags, ast.Errorf("compiler", r.Pos,
			"route %q needs a stack of %d values; the VM allows %d", r.Pattern, c.ch.MaxStack, MaxStack))
	}
	return c.ch, c.diags
}

func (c *compiler) emit(op Op, arg int32) int {
	c.ch.Code = append(c.ch.Code, Instr{Op: op, Arg: arg})
	return len(c.ch.Code) - 1
}

func (c *compiler) patch(at int) { c.ch.Code[at].Arg = int32(len(c.ch.Code)) }

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

func (c *compiler) stmts(ss []ast.Stmt) {
	for _, s := range ss {
		switch s := s.(type) {
		case *ast.RequireAuth:
			c.emit(OpRequireAuth, 0)
		case *ast.When:
			c.expr(s.Cond)
			j := c.emit(OpJumpIfFalse, 0)
			c.push(-1)
			c.stmts(s.Body)
			c.patch(j)
		case *ast.Limit:
			c.expr(s.Key)
			c.ch.Limits = append(c.ch.Limits, LimitInfo{ID: s.ID, Count: s.Count, Unit: s.Unit})
			c.emit(OpLimit, int32(len(c.ch.Limits)-1))
			c.push(-1)
		case *ast.Deny:
			c.emit(OpDeny, int32(s.Status))
		case *ast.SetHeader:
			c.expr(s.Value)
			c.emit(OpSetHeader, c.constant(env.StringVal(s.Name)))
			c.push(-1)
		}
	}
}

var binOps = map[ast.Op]Op{
	ast.OpEq: OpEq, ast.OpNe: OpNe, ast.OpLt: OpLt, ast.OpLe: OpLe, ast.OpGt: OpGt, ast.OpGe: OpGe,
	ast.OpAdd: OpAdd, ast.OpSub: OpSub, ast.OpMul: OpMul, ast.OpDiv: OpDiv,
}

func (c *compiler) expr(e ast.Expr) {
	switch e := e.(type) {
	case *ast.IntLit:
		c.emit(OpPushConst, c.constant(env.IntVal(e.Value)))
		c.push(1)
	case *ast.StringLit:
		c.emit(OpPushConst, c.constant(env.StringVal(e.Value)))
		c.push(1)
	case *ast.BoolLit:
		c.emit(OpPushConst, c.constant(env.BoolVal(e.Value)))
		c.push(1)
	case *ast.Field:
		if e.ID < 0 {
			c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: field %s.%s not resolved (type checker not run)", e.Obj, e.Name))
		}
		c.emit(OpLoadField, int32(e.ID))
		c.push(1)
	case *ast.Call:
		for _, a := range e.Args {
			c.expr(a)
		}
		if e.Builtin < 0 {
			c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: call %s not resolved (type checker not run)", e.Fn))
		}
		c.emit(OpCallBuiltin, int32(e.Builtin))
		c.push(1 - len(e.Args))
	case *ast.Unary:
		c.expr(e.X)
		if e.Op == ast.OpNot {
			c.emit(OpNot, 0)
		} else {
			c.emit(OpNeg, 0)
		}
	case *ast.Binary:
		if e.Op == ast.OpAnd || e.Op == ast.OpOr {
			c.expr(e.X)
			op := OpAnd
			if e.Op == ast.OpOr {
				op = OpOr
			}
			j := c.emit(op, 0)
			c.push(-1) // on fall-through the left value is popped
			c.expr(e.Y)
			c.patch(j)
			return
		}
		c.expr(e.X)
		c.expr(e.Y)
		c.emit(binOps[e.Op], 0)
		c.push(-1)
	case *ast.Ident:
		// Lets are inlined by the optimizer; reaching here is a bug.
		c.diags = append(c.diags, ast.Errorf("compiler", e.Pos, "internal error: un-inlined name %q", e.Name))
	}
}

// Disassemble prints a chunk, used in golden tests and debugging.
func (ch *Chunk) Disassemble() string {
	var b strings.Builder
	for i, in := range ch.Code {
		fmt.Fprintf(&b, "%04d %-13s", i, in.Op)
		switch in.Op {
		case OpPushConst, OpSetHeader:
			v := ch.Consts[in.Arg]
			if v.T == env.String {
				fmt.Fprintf(&b, " %q", v.S)
			} else if v.T == env.Bool {
				fmt.Fprintf(&b, " %v", v.Bool())
			} else {
				fmt.Fprintf(&b, " %d", v.I)
			}
		case OpLimit:
			l := ch.Limits[in.Arg]
			fmt.Fprintf(&b, " #%d %d/%s", l.ID, l.Count, l.Unit)
		case OpReturn, OpRequireAuth, OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpAdd, OpSub, OpMul, OpDiv, OpNeg, OpNot:
		default:
			fmt.Fprintf(&b, " %d", in.Arg)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
