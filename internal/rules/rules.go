// Package rules is the entry point to the rule language: it runs the whole
// pipeline (lexer → parser → type checker → static analysis → optimizer →
// compiler) and exposes route matching and evaluation on either engine.
package rules

import (
	"errors"
	"sort"
	"strings"

	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/analysis"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/ast"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/compiler"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/env"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/interp"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/opt"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/parser"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/types"
	"github.com/XuanGiaHanNguyen/Tollgate/internal/rules/vm"
)

// Options for Compile.
type Options struct {
	Budget int // VM instruction budget per evaluation; 0 = vm.DefaultBudget
}

// RuleSet is a compiled rule file. It is immutable and safe to share.
type RuleSet struct {
	Routes   []*Route // most specific first
	Warnings []ast.Diagnostic
	budget   int
}

// Route is one compiled route block.
type Route struct {
	Pattern   string
	pat       analysis.Pattern
	Source    *ast.Route      // as written (lets not inlined)
	Optimized *ast.Route      // what both engines run
	Chunk     *compiler.Chunk // bytecode for the VM
	lets      interp.Lets     // for evaluating Source
	AI        AISettings      // providers, hedging, safety filter
}

// AISettings are a route's static AI settings (top-level statements).
type AISettings struct {
	Providers     []string // upstream names, in order; empty = the route's upstream
	HedgeQuantile float64  // 0 = no hedging; e.g. 0.95
	HedgeMaxPct   float64
	PatternSet    string // "" = no safety filter
	PatternAction string // redact, block, flag
}

func aiSettings(r *ast.Route) AISettings {
	var s AISettings
	for _, st := range r.Body {
		switch st := st.(type) {
		case *ast.Providers:
			s.Providers = st.Names
		case *ast.Hedge:
			s.HedgeQuantile, s.HedgeMaxPct = st.Quantile, st.MaxPct
		case *ast.BlockPatterns:
			s.PatternSet, s.PatternAction = st.Set, st.Action
		}
	}
	return s
}

// CompileError carries every error diagnostic (and any warnings).
type CompileError struct{ Diags []ast.Diagnostic }

func (e *CompileError) Error() string {
	var b strings.Builder
	for i, d := range e.Diags {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(d.String())
	}
	return b.String()
}

// Compile runs the full pipeline. On failure the error is a *CompileError
// listing every problem found; warnings never fail compilation.
func Compile(src string, o Options) (*RuleSet, error) {
	budget := o.Budget
	if budget <= 0 {
		budget = vm.DefaultBudget
	}
	diags := Check(src)
	if ast.HasErrors(diags) {
		return nil, &CompileError{Diags: diags}
	}

	prog, _ := parser.Parse(src)
	info, _ := types.Check(prog)
	optimized, _ := opt.Optimize(prog)

	lets := interp.Lets{}
	for name, l := range info.Lets {
		lets[name] = l.Value
	}
	rs := &RuleSet{budget: budget}
	srcRoutes := prog.Routes()
	for i, or := range optimized.Routes() {
		ch, cdiags := compiler.Compile(or)
		diags = append(diags, cdiags...)
		diags = append(diags, analysis.CheckBound(or, len(ch.Code), budget)...)
		if err := vm.Validate(ch); err != nil && !ast.HasErrors(cdiags) {
			diags = append(diags, ast.Errorf("compiler", or.Pos, "internal error: %v", err))
		}
		pat, _ := analysis.ParsePattern(or.Pattern)
		rs.Routes = append(rs.Routes, &Route{
			Pattern: or.Pattern, pat: pat, Source: srcRoutes[i], Optimized: or, Chunk: ch, lets: lets,
			AI: aiSettings(srcRoutes[i]),
		})
	}
	if ast.HasErrors(diags) {
		return nil, &CompileError{Diags: diags}
	}
	sort.SliceStable(rs.Routes, func(i, j int) bool { return analysis.MoreSpecific(rs.Routes[i].pat, rs.Routes[j].pat) })
	for _, d := range diags {
		if d.Severity == ast.Warning {
			rs.Warnings = append(rs.Warnings, d)
		}
	}
	return rs, nil
}

// Check runs the front end (lexer, parser, types, analysis, optimizer) and
// returns all diagnostics, errors and warnings, in pipeline order. Later
// stages run only when earlier ones produced no errors, to avoid cascades.
func Check(src string) []ast.Diagnostic {
	prog, diags := parser.Parse(src)
	if ast.HasErrors(diags) {
		return diags
	}
	info, tdiags := types.Check(prog)
	diags = append(diags, tdiags...)
	if ast.HasErrors(diags) {
		return diags
	}
	diags = append(diags, analysis.Analyze(prog, info.Lets)...)
	_, odiags := opt.Optimize(prog)
	return append(diags, odiags...)
}

// Match returns the most specific route for path, or nil.
func (rs *RuleSet) Match(path string) *Route {
	for _, r := range rs.Routes {
		if r.pat.Match(path) {
			return r
		}
	}
	return nil
}

// Engine selects the evaluator.
type Engine int

const (
	VM Engine = iota
	Interp
)

func (e Engine) String() string { return [...]string{"vm", "interp"}[e] }

// Eval evaluates the route on the chosen engine.
func (rs *RuleSet) Eval(r *Route, e Engine, req *env.Request, d *env.Decision) {
	switch e {
	case Interp:
		interp.Eval(r.Optimized.Body, nil, req, rs.budget, d)
	default:
		vm.Run(r.Chunk, req, rs.budget, d)
	}
}

// EvalSource runs the interpreter on the unoptimized source (with lets),
// the oracle used to test the optimizer.
func (rs *RuleSet) EvalSource(r *Route, req *env.Request, d *env.Decision) {
	interp.Eval(r.Source.Body, r.lets, req, 0, d)
}

// ErrNoRoute is returned when no rule route matches.
var ErrNoRoute = errors.New("no rule route matches")
