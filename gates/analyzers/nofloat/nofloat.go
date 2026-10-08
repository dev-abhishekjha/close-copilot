// Package nofloat defines an analyzer that keeps floating point out of the
// packages that handle money.
package nofloat

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/abhishekjha/close-copilot/gates/analyzers/scope"
)

// Packages are the money-handling package trees (each with its
// subpackages). Retrieval scores (internal/retrieval) and LLM cost
// (internal/llm) are deliberately absent; internal/money is the only
// package that converts to or from floating point.
var Packages = []string{
	"internal/checks",
	"internal/books",
	"internal/evidence",
	"internal/seed",
	"internal/approvals",
	"internal/agent",
}

// Analyzer reports floating point in the money-handling packages.
var Analyzer = &analysis.Analyzer{
	Name: "nofloat",
	Doc: `forbid float32 and float64 in the money-handling packages

Money is int64 paise (money.Paise); only internal/money converts to or from
floating point, and ERPNext amounts are decoded as json.Number. In
internal/checks, internal/books, internal/evidence, internal/seed,
internal/approvals and internal/agent (and their subpackages) nofloat
reports any declaration or expression whose type is float32 or float64,
including untyped float constants such as 1.5 that default to float64.
It uses type information, so a local type that happens to be named float64
is not reported.

Test files (*_test.go) are exempt: float assertions in tests never touch
the ledger.`,
	Run: run,
}

func run(pass *analysis.Pass) (any, error) {
	// InTested widens the scope (a dir such as internal/checks_test is
	// checked too); it never narrows it.
	if !scope.InTested(pass.Pkg.Path(), Packages...) {
		return nil, nil
	}
	for _, f := range pass.Files {
		if scope.IsTestFile(pass.Fset.File(f.Pos()).Name()) {
			continue
		}
		checkFile(pass, f)
	}
	return nil, nil
}

// checkFile reports at most one diagnostic per line: the outermost float
// expression or the first float-typed definition on it.
func checkFile(pass *analysis.Pass, f *ast.File) {
	reported := map[int]bool{}
	report := func(pos token.Pos, t types.Type) {
		line := pass.Fset.Position(pos).Line
		if reported[line] {
			return
		}
		reported[line] = true
		pass.Reportf(pos, "%s in %s: money is int64 paise (money.Paise); only internal/money converts to or from floating point",
			describe(t), scope.Rel(pass.Pkg.Path()))
	}

	ast.Inspect(f, func(n ast.Node) bool {
		// Definitions carry no type expression when inferred
		// (x := strconv.ParseFloat(...)), so check them directly.
		if id, ok := n.(*ast.Ident); ok {
			if v, ok := pass.TypesInfo.Defs[id].(*types.Var); ok && isFloat(v.Type()) {
				report(id.Pos(), v.Type())
			}
		}
		if e, ok := n.(ast.Expr); ok {
			if tv, ok := pass.TypesInfo.Types[e]; ok && isFloat(tv.Type) {
				report(e.Pos(), tv.Type)
				return false // one report for the outermost float expression
			}
		}
		return true
	})
}

// isFloat reports whether t is, has an underlying type of, or (for a
// result tuple) contains float32, float64 or an untyped float.
func isFloat(t types.Type) bool {
	if t == nil {
		return false
	}
	if tup, ok := t.(*types.Tuple); ok {
		for v := range tup.Variables() {
			if isFloat(v.Type()) {
				return true
			}
		}
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return false
	}
	switch b.Kind() {
	case types.Float32, types.Float64, types.UntypedFloat:
		return true
	}
	return false
}

func describe(t types.Type) string {
	if b, ok := t.Underlying().(*types.Basic); ok && b.Kind() == types.UntypedFloat {
		return "untyped float constant (defaults to float64)"
	}
	if _, ok := t.(*types.Tuple); ok {
		return "float result"
	}
	return "float type " + t.String()
}
