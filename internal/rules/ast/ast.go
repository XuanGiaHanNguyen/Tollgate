// Package ast defines the rule language syntax tree, source positions and
// diagnostics shared by every compiler stage.
package ast

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pos is a 1-based source position.
type Pos struct{ Line, Col int }

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Severity of a diagnostic.
type Severity int

const (
	Error Severity = iota
	Warning
)

// Diagnostic is a compiler message tied to a position.
type Diagnostic struct {
	Pos      Pos
	Severity Severity
	Stage    string // lexer, parser, types, analysis, opt, compiler
	Msg      string
}

func (d Diagnostic) String() string {
	sev := "error"
	if d.Severity == Warning {
		sev = "warning"
	}
	return fmt.Sprintf("%s: %s: %s: %s", d.Pos, sev, d.Stage, d.Msg)
}

func (d Diagnostic) Error() string { return d.String() }

// Errorf builds an error diagnostic.
func Errorf(stage string, pos Pos, format string, a ...any) Diagnostic {
	return Diagnostic{Pos: pos, Severity: Error, Stage: stage, Msg: fmt.Sprintf(format, a...)}
}

// Warnf builds a warning diagnostic.
func Warnf(stage string, pos Pos, format string, a ...any) Diagnostic {
	return Diagnostic{Pos: pos, Severity: Warning, Stage: stage, Msg: fmt.Sprintf(format, a...)}
}

// HasErrors reports whether any diagnostic is an error.
func HasErrors(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// Program is a whole rule file. Decls keeps source order, which matters
// because a let must be declared before it is used.
type Program struct {
	Decls []Decl
}

// Decl is a top-level declaration: *Let or *Route.
type Decl interface{ declNode() }

// Let binds a name to an expression.
type Let struct {
	Pos   Pos
	Name  string
	Value Expr
}

// Route is a policy block for a path pattern.
type Route struct {
	Pos     Pos
	Pattern string
	Body    []Stmt
}

func (*Let) declNode()   {}
func (*Route) declNode() {}

// Routes returns the program's routes in source order.
func (p *Program) Routes() []*Route {
	var rs []*Route
	for _, d := range p.Decls {
		if r, ok := d.(*Route); ok {
			rs = append(rs, r)
		}
	}
	return rs
}

// Stmt is a statement inside a route.
type Stmt interface {
	stmtNode()
	At() Pos
}

// RequireAuth denies 401 when the request is unauthenticated.
type RequireAuth struct{ Pos Pos }

// When runs Body if Cond is true.
type When struct {
	Pos  Pos
	Cond Expr
	Body []Stmt
}

// Unit of a rate limit window.
type Unit int

const (
	Second Unit = iota
	Minute
	Hour
	Day
)

var unitNames = [...]string{"second", "minute", "hour", "day"}

func (u Unit) String() string { return unitNames[u] }

// Duration of one unit.
func (u Unit) Duration() time.Duration {
	return [...]time.Duration{time.Second, time.Minute, time.Hour, 24 * time.Hour}[u]
}

// UnitByName maps "minute" to Minute.
func UnitByName(s string) (Unit, bool) {
	for i, n := range unitNames {
		if n == s {
			return Unit(i), true
		}
	}
	return 0, false
}

// Limit admits Count requests per Unit for each distinct Key value.
// ID is unique within a program, so engines agree on bucket identity.
type Limit struct {
	Pos   Pos
	ID    int
	Count int64
	Unit  Unit
	Key   Expr
}

// Deny ends evaluation with an HTTP status.
type Deny struct {
	Pos    Pos
	Status int64
}

// SetHeader adds a request header before proxying.
type SetHeader struct {
	Pos   Pos
	Name  string
	Value Expr
}

func (*RequireAuth) stmtNode() {}
func (*When) stmtNode()        {}
func (*Limit) stmtNode()       {}
func (*Deny) stmtNode()        {}
func (*SetHeader) stmtNode()   {}

func (s *RequireAuth) At() Pos { return s.Pos }
func (s *When) At() Pos        { return s.Pos }
func (s *Limit) At() Pos       { return s.Pos }
func (s *Deny) At() Pos        { return s.Pos }
func (s *SetHeader) At() Pos   { return s.Pos }

// Expr is an expression.
type Expr interface {
	exprNode()
	At() Pos
}

// Op is a unary or binary operator.
type Op int

const (
	OpOr Op = iota
	OpAnd
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
	OpNot
	OpNeg
)

var opNames = [...]string{"||", "&&", "==", "!=", "<", "<=", ">", ">=", "+", "-", "*", "/", "!", "-"}

func (o Op) String() string { return opNames[o] }

type (
	IntLit struct {
		Pos   Pos
		Value int64
	}
	StringLit struct {
		Pos   Pos
		Value string
	}
	BoolLit struct {
		Pos   Pos
		Value bool
	}
	Ident struct {
		Pos  Pos
		Name string
	}
	// Field and Call carry IDs resolved once by the type checker, so
	// neither engine resolves names at evaluation time.
	Field struct {
		Pos       Pos
		Obj, Name string
		ID        int
	}
	Call struct {
		Pos     Pos
		Fn      string
		Args    []Expr
		Builtin int
	}
	Unary struct {
		Pos Pos
		Op  Op
		X   Expr
	}
	Binary struct {
		Pos  Pos
		Op   Op
		X, Y Expr
	}
)

func (*IntLit) exprNode()    {}
func (*StringLit) exprNode() {}
func (*BoolLit) exprNode()   {}
func (*Ident) exprNode()     {}
func (*Field) exprNode()     {}
func (*Call) exprNode()      {}
func (*Unary) exprNode()     {}
func (*Binary) exprNode()    {}

func (e *IntLit) At() Pos    { return e.Pos }
func (e *StringLit) At() Pos { return e.Pos }
func (e *BoolLit) At() Pos   { return e.Pos }
func (e *Ident) At() Pos     { return e.Pos }
func (e *Field) At() Pos     { return e.Pos }
func (e *Call) At() Pos      { return e.Pos }
func (e *Unary) At() Pos     { return e.Pos }
func (e *Binary) At() Pos    { return e.Pos }

// Format prints a program as an indented S-expression tree, used by golden
// tests and debugging. Positions are omitted so goldens survive reformatting.
func Format(p *Program) string {
	var b strings.Builder
	for _, d := range p.Decls {
		switch d := d.(type) {
		case *Let:
			fmt.Fprintf(&b, "(let %s %s)\n", d.Name, FormatExpr(d.Value))
		case *Route:
			fmt.Fprintf(&b, "(route %q\n", d.Pattern)
			formatStmts(&b, d.Body, 1)
			b.WriteString(")\n")
		}
	}
	return b.String()
}

func formatStmts(b *strings.Builder, ss []Stmt, depth int) {
	ind := strings.Repeat("  ", depth)
	for _, s := range ss {
		switch s := s.(type) {
		case *RequireAuth:
			fmt.Fprintf(b, "%s(require auth)\n", ind)
		case *When:
			fmt.Fprintf(b, "%s(when %s\n", ind, FormatExpr(s.Cond))
			formatStmts(b, s.Body, depth+1)
			fmt.Fprintf(b, "%s)\n", ind)
		case *Limit:
			fmt.Fprintf(b, "%s(limit#%d %d per %s by %s)\n", ind, s.ID, s.Count, s.Unit, FormatExpr(s.Key))
		case *Deny:
			fmt.Fprintf(b, "%s(deny %d)\n", ind, s.Status)
		case *SetHeader:
			fmt.Fprintf(b, "%s(set_header %q %s)\n", ind, s.Name, FormatExpr(s.Value))
		}
	}
}

// FormatExpr prints one expression as an S-expression.
func FormatExpr(e Expr) string {
	switch e := e.(type) {
	case *IntLit:
		return strconv.FormatInt(e.Value, 10)
	case *StringLit:
		return strconv.Quote(e.Value)
	case *BoolLit:
		return strconv.FormatBool(e.Value)
	case *Ident:
		return e.Name
	case *Field:
		return e.Obj + "." + e.Name
	case *Call:
		parts := []string{e.Fn}
		for _, a := range e.Args {
			parts = append(parts, FormatExpr(a))
		}
		return "(call " + strings.Join(parts, " ") + ")"
	case *Unary:
		return "(" + e.Op.String() + " " + FormatExpr(e.X) + ")"
	case *Binary:
		return "(" + e.Op.String() + " " + FormatExpr(e.X) + " " + FormatExpr(e.Y) + ")"
	case nil:
		return "<nil>"
	}
	return fmt.Sprintf("<%T>", e)
}
