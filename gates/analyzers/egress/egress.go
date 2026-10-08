// Package egress defines an analyzer that sends all outbound HTTP through
// internal/httpx, whose client refuses hosts outside the configured
// services.
package egress

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/abhishekjha/close-copilot/gates/analyzers/scope"
)

// Analyzer reports HTTP clients built outside internal/httpx.
var Analyzer = &analysis.Analyzer{
	Name: "egress",
	Doc: `route outbound HTTP through internal/httpx

internal/httpx.New returns a client whose transport refuses any host that
isn't one of the configured services (ERPNext, the MCP servers, TEI,
docling, the OTLP endpoint, Anthropic). Outside package internal/httpx,
egress reports:

  - references to net/http's Get, Head, Post and PostForm;
  - any use of http.DefaultClient or http.DefaultTransport;
  - composite literals http.Client{...} and &http.Client{...}, and
    new(http.Client).

Test files (*_test.go) are exempt: tests use httptest servers on loopback.`,
	Run: run,
}

var forbiddenFuncs = map[string]bool{"Get": true, "Head": true, "Post": true, "PostForm": true}

var forbiddenVars = map[string]bool{"DefaultClient": true, "DefaultTransport": true}

const fix = "build clients with internal/httpx.New"

func run(pass *analysis.Pass) (any, error) {
	if scope.Is(pass.Pkg.Path(), "internal/httpx") {
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

func checkFile(pass *analysis.Pass, f *ast.File) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			obj := pass.TypesInfo.Uses[n]
			if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != "net/http" || obj.Parent() != obj.Pkg().Scope() {
				return true
			}
			switch obj.(type) {
			case *types.Func:
				if forbiddenFuncs[obj.Name()] {
					pass.Reportf(n.Pos(), "http.%s uses the default client, which reaches any host: %s", obj.Name(), fix)
				}
			case *types.Var:
				if forbiddenVars[obj.Name()] {
					pass.Reportf(n.Pos(), "http.%s reaches any host: %s", obj.Name(), fix)
				}
			}
		case *ast.CompositeLit:
			if isHTTPClient(pass.TypesInfo.TypeOf(n)) {
				pass.Reportf(n.Pos(), "http.Client literal has no host allowlist: %s", fix)
			}
		case *ast.CallExpr:
			if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && len(n.Args) == 1 {
				if b, ok := pass.TypesInfo.Uses[id].(*types.Builtin); ok && b.Name() == "new" && isHTTPClient(pass.TypesInfo.TypeOf(n.Args[0])) {
					pass.Reportf(n.Pos(), "new(http.Client) has no host allowlist: %s", fix)
				}
			}
		}
		return true
	})
}

func isHTTPClient(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem() // an elided &T{} element in []*http.Client{{...}}
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == "Client"
}
