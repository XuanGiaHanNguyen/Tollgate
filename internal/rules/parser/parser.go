// Package parser builds an AST from rule source: recursive descent for
// declarations and statements, Pratt parsing for expressions. After an error
// it resynchronizes at the next statement or declaration, so one pass
// reports several errors.
package parser

import (
	"strconv"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/lexer"
)

// maxDepth bounds expression and block nesting so hostile input cannot
// overflow the stack.
const maxDepth = 200

type parser struct {
	toks    []lexer.Token
	pos     int
	diags   []ast.Diagnostic
	limitID int
	depth   int
}

// bailout unwinds the parser after a nesting-depth error.
type bailout struct{}

// Parse parses a whole program. Lexer diagnostics are included.
func Parse(src string) (prog *ast.Program, diags []ast.Diagnostic) {
	toks, lexDiags := lexer.All(src)
	p := &parser{toks: toks, diags: lexDiags}
	prog = &ast.Program{}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
			diags = p.diags
		}
	}()
	for p.cur().Kind != lexer.EOF {
		switch p.cur().Kind {
		case lexer.Let:
			if d := p.parseLet(); d != nil {
				prog.Decls = append(prog.Decls, d)
			}
		case lexer.Route:
			if d := p.parseRoute(); d != nil {
				prog.Decls = append(prog.Decls, d)
			}
		default:
			p.errorf(p.cur().Pos, "expected 'let' or 'route', found %s", describe(p.cur()))
			p.advance()
			p.syncTop()
		}
	}
	return prog, p.diags
}

// ---- token helpers ----

func (p *parser) cur() lexer.Token { return p.toks[p.pos] }

func (p *parser) advance() lexer.Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) errorf(pos ast.Pos, format string, a ...any) {
	p.diags = append(p.diags, ast.Errorf("parser", pos, format, a...))
}

// expect consumes a token of kind k or reports an error. Illegal tokens were
// already reported by the lexer, so they don't produce a second message.
func (p *parser) expect(k lexer.Kind, what string) (lexer.Token, bool) {
	t := p.cur()
	if t.Kind == k {
		return p.advance(), true
	}
	if t.Kind != lexer.Illegal {
		p.errorf(t.Pos, "expected %s, found %s", what, describe(t))
	}
	return t, false
}

func describe(t lexer.Token) string {
	switch t.Kind {
	case lexer.EOF:
		return "end of file"
	case lexer.Ident:
		return "identifier " + strconv.Quote(t.Text)
	case lexer.Int:
		return "integer " + t.Text
	case lexer.String:
		return "string " + strconv.Quote(t.Text)
	case lexer.Illegal:
		return "invalid token " + strconv.Quote(t.Text)
	}
	return "'" + t.Kind.String() + "'"
}

func (p *parser) enter(pos ast.Pos) {
	p.depth++
	if p.depth > maxDepth {
		p.errorf(pos, "nesting deeper than %d", maxDepth)
		panic(bailout{})
	}
}

func (p *parser) leave() { p.depth-- }

func isStmtStart(k lexer.Kind) bool {
	switch k {
	case lexer.Require, lexer.When, lexer.Limit, lexer.Deny, lexer.SetHeader:
		return true
	}
	return false
}

// syncTop skips to the next top-level declaration.
func (p *parser) syncTop() {
	for k := p.cur().Kind; k != lexer.EOF && k != lexer.Let && k != lexer.Route; k = p.cur().Kind {
		p.advance()
	}
}

// syncStmt skips to the next statement, closing brace or declaration. A
// brace group met on the way (e.g. the body of a `when` whose condition
// failed to parse) is skipped as a unit, so its closing brace is not
// mistaken for the end of the enclosing block.
func (p *parser) syncStmt() {
	depth := 0
	for {
		k := p.cur().Kind
		switch {
		case k == lexer.EOF:
			return
		case k == lexer.Let || k == lexer.Route:
			return // a new declaration always ends recovery
		case k == lexer.LBrace:
			depth++
		case k == lexer.RBrace:
			if depth == 0 {
				return
			}
			depth--
		case depth == 0 && isStmtStart(k):
			return
		}
		p.advance()
	}
}

// ---- declarations ----

func (p *parser) parseLet() *ast.Let {
	kw := p.advance()
	name, ok := p.expect(lexer.Ident, "identifier after 'let'")
	if !ok {
		p.syncTop()
		return nil
	}
	if _, ok := p.expect(lexer.Assign, "'='"); !ok {
		p.syncTop()
		return nil
	}
	val := p.parseExpr()
	if val == nil {
		p.syncTop()
		return nil
	}
	return &ast.Let{Pos: kw.Pos, Name: name.Text, Value: val}
}

func (p *parser) parseRoute() *ast.Route {
	kw := p.advance()
	pat, ok := p.expect(lexer.String, "route pattern string")
	if !ok {
		p.syncTop()
		return nil
	}
	r := &ast.Route{Pos: kw.Pos, Pattern: pat.Text}
	if _, ok := p.expect(lexer.LBrace, "'{'"); !ok {
		p.syncTop()
		return nil
	}
	r.Body = p.parseBlock(kw.Pos)
	return r
}

// parseBlock parses statements up to and including the closing brace.
func (p *parser) parseBlock(open ast.Pos) []ast.Stmt {
	p.enter(open)
	defer p.leave()
	var body []ast.Stmt
	for {
		t := p.cur()
		switch {
		case t.Kind == lexer.RBrace:
			p.advance()
			return body
		case t.Kind == lexer.EOF:
			p.errorf(t.Pos, "missing '}' for block opened at %s", open)
			return body
		case t.Kind == lexer.Let || t.Kind == lexer.Route:
			p.errorf(t.Pos, "missing '}' for block opened at %s", open)
			return body
		case isStmtStart(t.Kind):
			if s := p.parseStmt(); s != nil {
				body = append(body, s)
			}
		default:
			if t.Kind != lexer.Illegal {
				p.errorf(t.Pos, "expected a statement, found %s", describe(t))
			}
			p.advance()
			p.syncStmt()
		}
	}
}

// ---- statements ----

func (p *parser) parseStmt() ast.Stmt {
	t := p.advance()
	switch t.Kind {
	case lexer.Require:
		a, ok := p.expect(lexer.Ident, "'auth' after 'require'")
		if !ok || a.Text != "auth" {
			if ok {
				p.errorf(a.Pos, "expected 'auth' after 'require', found %s", describe(a))
			}
			p.syncStmt()
			return nil
		}
		return &ast.RequireAuth{Pos: t.Pos}

	case lexer.When:
		cond := p.parseExpr()
		if cond == nil {
			p.syncStmt()
			return nil
		}
		if _, ok := p.expect(lexer.LBrace, "'{' after when condition"); !ok {
			p.syncStmt()
			return nil
		}
		return &ast.When{Pos: t.Pos, Cond: cond, Body: p.parseBlock(t.Pos)}

	case lexer.Limit:
		n, ok := p.expect(lexer.Int, "request count after 'limit'")
		if !ok {
			p.syncStmt()
			return nil
		}
		count, _ := strconv.ParseInt(n.Text, 10, 64)
		if _, ok := p.expect(lexer.Per, "'per'"); !ok {
			p.syncStmt()
			return nil
		}
		u, ok := p.expect(lexer.Ident, "time unit (second, minute, hour, day)")
		if !ok {
			p.syncStmt()
			return nil
		}
		unit, ok := ast.UnitByName(u.Text)
		if !ok {
			p.errorf(u.Pos, "unknown time unit %q (want second, minute, hour or day)", u.Text)
			p.syncStmt()
			return nil
		}
		if _, ok := p.expect(lexer.By, "'by'"); !ok {
			p.syncStmt()
			return nil
		}
		key := p.parseExpr()
		if key == nil {
			p.syncStmt()
			return nil
		}
		var algo uint8
		if c := p.cur(); c.Kind == lexer.Ident && c.Text == "using" {
			p.advance()
			a, ok := p.expect(lexer.Ident, "algorithm after 'using' (token_bucket or sliding_window)")
			if !ok {
				p.syncStmt()
				return nil
			}
			switch a.Text {
			case "token_bucket":
			case "sliding_window":
				algo = 1
			default:
				p.errorf(a.Pos, "unknown limit algorithm %q (want token_bucket or sliding_window)", a.Text)
				p.syncStmt()
				return nil
			}
		}
		id := p.limitID
		p.limitID++
		return &ast.Limit{Pos: t.Pos, ID: id, Count: count, Unit: unit, Key: key, Algo: algo}

	case lexer.Deny:
		n, ok := p.expect(lexer.Int, "HTTP status after 'deny'")
		if !ok {
			p.syncStmt()
			return nil
		}
		status, _ := strconv.ParseInt(n.Text, 10, 64)
		return &ast.Deny{Pos: t.Pos, Status: status}

	case lexer.SetHeader:
		name, ok := p.expect(lexer.String, "header name string")
		if !ok {
			p.syncStmt()
			return nil
		}
		val := p.parseExpr()
		if val == nil {
			p.syncStmt()
			return nil
		}
		return &ast.SetHeader{Pos: t.Pos, Name: name.Text, Value: val}
	}
	return nil
}

// ---- expressions (Pratt) ----

const (
	precLowest = iota
	precOr
	precAnd
	precEquality
	precCompare
	precSum
	precProduct
	precUnary
)

var binOps = map[lexer.Kind]struct {
	prec int
	op   ast.Op
}{
	lexer.Or: {precOr, ast.OpOr}, lexer.And: {precAnd, ast.OpAnd},
	lexer.Eq: {precEquality, ast.OpEq}, lexer.Ne: {precEquality, ast.OpNe},
	lexer.Lt: {precCompare, ast.OpLt}, lexer.Le: {precCompare, ast.OpLe},
	lexer.Gt: {precCompare, ast.OpGt}, lexer.Ge: {precCompare, ast.OpGe},
	lexer.Plus: {precSum, ast.OpAdd}, lexer.Minus: {precSum, ast.OpSub},
	lexer.Star: {precProduct, ast.OpMul}, lexer.Slash: {precProduct, ast.OpDiv},
}

func (p *parser) parseExpr() ast.Expr { return p.parseBinary(precLowest) }

// parseBinary parses operators that bind tighter than minPrec; all binary
// operators are left-associative.
func (p *parser) parseBinary(minPrec int) ast.Expr {
	p.enter(p.cur().Pos)
	defer p.leave()
	left := p.parseUnary()
	if left == nil {
		return nil
	}
	for {
		info, ok := binOps[p.cur().Kind]
		if !ok || info.prec <= minPrec {
			return left
		}
		opTok := p.advance()
		right := p.parseBinary(info.prec)
		if right == nil {
			return nil
		}
		left = &ast.Binary{Pos: opTok.Pos, Op: info.op, X: left, Y: right}
	}
}

func (p *parser) parseUnary() ast.Expr {
	t := p.cur()
	switch t.Kind {
	case lexer.Not, lexer.Minus:
		p.advance()
		p.enter(t.Pos)
		defer p.leave()
		x := p.parseUnary()
		if x == nil {
			return nil
		}
		op := ast.OpNot
		if t.Kind == lexer.Minus {
			op = ast.OpNeg
		}
		return &ast.Unary{Pos: t.Pos, Op: op, X: x}
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() ast.Expr {
	t := p.cur()
	switch t.Kind {
	case lexer.Int:
		p.advance()
		v, _ := strconv.ParseInt(t.Text, 10, 64)
		return &ast.IntLit{Pos: t.Pos, Value: v}
	case lexer.String:
		p.advance()
		return &ast.StringLit{Pos: t.Pos, Value: t.Text}
	case lexer.True, lexer.False:
		p.advance()
		return &ast.BoolLit{Pos: t.Pos, Value: t.Kind == lexer.True}
	case lexer.LParen:
		p.advance()
		e := p.parseExpr()
		if e == nil {
			return nil
		}
		if _, ok := p.expect(lexer.RParen, "')'"); !ok {
			return nil
		}
		return e
	case lexer.Ident:
		p.advance()
		switch p.cur().Kind {
		case lexer.Dot:
			p.advance()
			name, ok := p.expect(lexer.Ident, "field name after '.'")
			if !ok {
				return nil
			}
			return &ast.Field{Pos: t.Pos, Obj: t.Text, Name: name.Text, ID: -1} // resolved by types
		case lexer.LParen:
			p.advance()
			call := &ast.Call{Pos: t.Pos, Fn: t.Text, Builtin: -1} // resolved by types
			if p.cur().Kind == lexer.RParen {
				p.advance()
				return call
			}
			for {
				a := p.parseExpr()
				if a == nil {
					return nil
				}
				call.Args = append(call.Args, a)
				if p.cur().Kind == lexer.Comma {
					p.advance()
					continue
				}
				if _, ok := p.expect(lexer.RParen, "',' or ')' in call"); !ok {
					return nil
				}
				return call
			}
		}
		return &ast.Ident{Pos: t.Pos, Name: t.Text}
	case lexer.Illegal:
		p.advance()
		return nil
	}
	p.errorf(t.Pos, "expected an expression, found %s", describe(t))
	return nil
}
