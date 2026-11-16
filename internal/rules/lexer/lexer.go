// Package lexer turns rule source into tokens. It is hand-written and
// tracks line and column for every token so errors point at the exact spot.
package lexer

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
)

// Kind of token.
type Kind int

const (
	EOF Kind = iota
	Illegal
	Ident
	Int
	String

	// keywords
	Let
	Route
	Require
	When
	Limit
	Per
	By
	Deny
	SetHeader
	True
	False

	// punctuation and operators
	LBrace
	RBrace
	LParen
	RParen
	Comma
	Dot
	Assign
	Eq
	Ne
	Lt
	Le
	Gt
	Ge
	Plus
	Minus
	Star
	Slash
	And
	Or
	Not
)

var kindNames = map[Kind]string{
	EOF: "EOF", Illegal: "ILLEGAL", Ident: "IDENT", Int: "INT", String: "STRING",
	Let: "let", Route: "route", Require: "require", When: "when", Limit: "limit", Per: "per",
	By: "by", Deny: "deny", SetHeader: "set_header", True: "true", False: "false",
	LBrace: "{", RBrace: "}", LParen: "(", RParen: ")", Comma: ",", Dot: ".", Assign: "=",
	Eq: "==", Ne: "!=", Lt: "<", Le: "<=", Gt: ">", Ge: ">=", Plus: "+", Minus: "-",
	Star: "*", Slash: "/", And: "&&", Or: "||", Not: "!",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

var keywords = map[string]Kind{
	"let": Let, "route": Route, "require": Require, "when": When, "limit": Limit,
	"per": Per, "by": By, "deny": Deny, "set_header": SetHeader, "true": True, "false": False,
}

// Token is one lexeme. For String tokens Text is the decoded value; for all
// others it is the source text.
type Token struct {
	Kind Kind
	Text string
	Pos  ast.Pos
}

func (t Token) String() string {
	switch t.Kind {
	case Ident, Int, Illegal:
		return fmt.Sprintf("%s %s(%s)", t.Pos, t.Kind, t.Text)
	case String:
		return fmt.Sprintf("%s STRING(%s)", t.Pos, strconv.Quote(t.Text))
	}
	return fmt.Sprintf("%s %s", t.Pos, t.Kind)
}

// Lexer produces tokens on demand.
type Lexer struct {
	src       string
	off       int
	line, col int
	diags     []ast.Diagnostic
}

// New returns a lexer over src.
func New(src string) *Lexer { return &Lexer{src: src, line: 1, col: 1} }

// Diagnostics returns lexing errors seen so far.
func (l *Lexer) Diagnostics() []ast.Diagnostic { return l.diags }

// All lexes the whole input, ending with an EOF token.
func All(src string) ([]Token, []ast.Diagnostic) {
	l := New(src)
	var toks []Token
	for {
		t := l.Next()
		toks = append(toks, t)
		if t.Kind == EOF {
			return toks, l.diags
		}
	}
}

func (l *Lexer) errorf(pos ast.Pos, format string, a ...any) {
	l.diags = append(l.diags, ast.Errorf("lexer", pos, format, a...))
}

func (l *Lexer) peek() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.off:])
	return r
}

func (l *Lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) skipSpaceAndComments() {
	for l.off < len(l.src) {
		r := l.peek()
		switch {
		case r == '#':
			for l.off < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
		case unicode.IsSpace(r):
			l.advance()
		default:
			return
		}
	}
}

// Next returns the next token. After the input ends it keeps returning EOF.
func (l *Lexer) Next() Token {
	l.skipSpaceAndComments()
	pos := ast.Pos{Line: l.line, Col: l.col}
	if l.off >= len(l.src) {
		return Token{Kind: EOF, Pos: pos}
	}
	start := l.off
	r := l.peek()

	switch {
	case r == '_' || isLetter(r):
		for l.off < len(l.src) {
			c := l.peek()
			if c != '_' && !isLetter(c) && !isDigit(c) {
				break
			}
			l.advance()
		}
		text := l.src[start:l.off]
		if k, ok := keywords[text]; ok {
			return Token{Kind: k, Text: text, Pos: pos}
		}
		return Token{Kind: Ident, Text: text, Pos: pos}

	case isDigit(r):
		for l.off < len(l.src) && isDigit(l.peek()) {
			l.advance()
		}
		// A number running into letters ("10x") is one bad token, not two.
		if l.off < len(l.src) && (isLetter(l.peek()) || l.peek() == '_') {
			for l.off < len(l.src) && (isLetter(l.peek()) || isDigit(l.peek()) || l.peek() == '_') {
				l.advance()
			}
			text := l.src[start:l.off]
			l.errorf(pos, "malformed number %q", text)
			return Token{Kind: Illegal, Text: text, Pos: pos}
		}
		text := l.src[start:l.off]
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			l.errorf(pos, "integer %s out of range", text)
			return Token{Kind: Illegal, Text: text, Pos: pos}
		}
		return Token{Kind: Int, Text: text, Pos: pos}

	case r == '"':
		return l.lexString(pos)
	}

	l.advance()
	two := func(next byte, double, single Kind) Token {
		if l.off < len(l.src) && l.src[l.off] == next {
			l.advance()
			return Token{Kind: double, Text: l.src[start:l.off], Pos: pos}
		}
		return Token{Kind: single, Text: l.src[start:l.off], Pos: pos}
	}
	switch r {
	case '{':
		return Token{Kind: LBrace, Text: "{", Pos: pos}
	case '}':
		return Token{Kind: RBrace, Text: "}", Pos: pos}
	case '(':
		return Token{Kind: LParen, Text: "(", Pos: pos}
	case ')':
		return Token{Kind: RParen, Text: ")", Pos: pos}
	case ',':
		return Token{Kind: Comma, Text: ",", Pos: pos}
	case '.':
		return Token{Kind: Dot, Text: ".", Pos: pos}
	case '+':
		return Token{Kind: Plus, Text: "+", Pos: pos}
	case '-':
		return Token{Kind: Minus, Text: "-", Pos: pos}
	case '*':
		return Token{Kind: Star, Text: "*", Pos: pos}
	case '/':
		return Token{Kind: Slash, Text: "/", Pos: pos}
	case '=':
		return two('=', Eq, Assign)
	case '!':
		return two('=', Ne, Not)
	case '<':
		return two('=', Le, Lt)
	case '>':
		return two('=', Ge, Gt)
	case '&':
		if l.off < len(l.src) && l.src[l.off] == '&' {
			l.advance()
			return Token{Kind: And, Text: "&&", Pos: pos}
		}
	case '|':
		if l.off < len(l.src) && l.src[l.off] == '|' {
			l.advance()
			return Token{Kind: Or, Text: "||", Pos: pos}
		}
	}
	text := l.src[start:l.off]
	if r == utf8.RuneError {
		l.errorf(pos, "invalid UTF-8")
	} else {
		l.errorf(pos, "unexpected character %q", text)
	}
	return Token{Kind: Illegal, Text: text, Pos: pos}
}

func (l *Lexer) lexString(pos ast.Pos) Token {
	l.advance() // opening quote
	var b strings.Builder
	for {
		if l.off >= len(l.src) || l.peek() == '\n' {
			l.errorf(pos, "unterminated string")
			return Token{Kind: Illegal, Text: b.String(), Pos: pos}
		}
		r := l.advance()
		switch r {
		case '"':
			return Token{Kind: String, Text: b.String(), Pos: pos}
		case '\\':
			escPos := ast.Pos{Line: l.line, Col: l.col - 1}
			if l.off >= len(l.src) {
				l.errorf(pos, "unterminated string")
				return Token{Kind: Illegal, Text: b.String(), Pos: pos}
			}
			e := l.advance()
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				l.errorf(escPos, "unknown escape \\%c", e)
			}
		default:
			if r == utf8.RuneError {
				l.errorf(ast.Pos{Line: l.line, Col: l.col - 1}, "invalid UTF-8 in string")
			}
			b.WriteRune(r)
		}
	}
}

func isLetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool  { return r >= '0' && r <= '9' }
