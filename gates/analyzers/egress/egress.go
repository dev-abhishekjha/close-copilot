// Package egress defines an analyzer that sends all outbound HTTP through
// internal/httpx, whose client refuses hosts outside the configured
// services.
package egress

import (
	"go/ast"
	"go/token"
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
  - composite literals http.Client{...}, &http.Client{...}, http.Transport{...},
    and &http.Transport{...}, plus new(http.Client) and new(http.Transport);
  - zero-value var declarations of http.Client or http.Transport;
  - struct fields of type http.Client or http.Transport held by value, or
    structs embedding them;
  - any use of net/http/httputil.ReverseProxy or httputil.NewSingleHostReverseProxy.

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
		case *ast.GenDecl:
			if n.Tok == token.VAR {
				for _, spec := range n.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						t := pass.TypesInfo.TypeOf(name)
						if isHTTPTypeValue(t, "Client") {
							pass.Reportf(name.Pos(), "var %s http.Client has no host allowlist: %s", name.Name, fix)
						} else if isHTTPTypeValue(t, "Transport") {
							pass.Reportf(name.Pos(), "var %s http.Transport has no host allowlist: %s", name.Name, fix)
						}
					}
				}
			}
		case *ast.StructType:
			if n.Fields != nil {
				for _, fld := range n.Fields.List {
					t := pass.TypesInfo.TypeOf(fld.Type)
					if len(fld.Names) == 0 {
						if isHTTPType(t, "Client") {
							pass.Reportf(fld.Pos(), "struct embeds http.Client: %s", fix)
						} else if isHTTPType(t, "Transport") {
							pass.Reportf(fld.Pos(), "struct embeds http.Transport: %s", fix)
						}
					} else {
						if isHTTPTypeValue(t, "Client") {
							for _, id := range fld.Names {
								pass.Reportf(id.Pos(), "struct field %s http.Client held by value has no host allowlist: %s", id.Name, fix)
							}
						} else if isHTTPTypeValue(t, "Transport") {
							for _, id := range fld.Names {
								pass.Reportf(id.Pos(), "struct field %s http.Transport held by value has no host allowlist: %s", id.Name, fix)
							}
						}
					}
				}
			}
		case *ast.Ident:
			obj := pass.TypesInfo.Uses[n]
			if obj == nil || obj.Pkg() == nil {
				return true
			}
			if obj.Pkg().Path() == "net/http" && obj.Parent() == obj.Pkg().Scope() {
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
			} else if obj.Pkg().Path() == "net/http/httputil" {
				if obj.Name() == "ReverseProxy" || obj.Name() == "NewSingleHostReverseProxy" {
					pass.Reportf(n.Pos(), "httputil.%s reaches any host: %s", obj.Name(), fix)
				}
			}
		case *ast.CompositeLit:
			t := pass.TypesInfo.TypeOf(n)
			if isHTTPType(t, "Client") {
				pass.Reportf(n.Pos(), "http.Client literal has no host allowlist: %s", fix)
			} else if isHTTPType(t, "Transport") {
				pass.Reportf(n.Pos(), "http.Transport literal has no host allowlist: %s", fix)
			}
		case *ast.CallExpr:
			if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && len(n.Args) == 1 {
				if b, ok := pass.TypesInfo.Uses[id].(*types.Builtin); ok && b.Name() == "new" {
					argType := pass.TypesInfo.TypeOf(n.Args[0])
					if isHTTPType(argType, "Client") {
						pass.Reportf(n.Pos(), "new(http.Client) has no host allowlist: %s", fix)
					} else if isHTTPType(argType, "Transport") {
						pass.Reportf(n.Pos(), "new(http.Transport) has no host allowlist: %s", fix)
					}
				}
			}
		}
		return true
	})
}

func isHTTPType(t types.Type, typeName string) bool {
	if t == nil {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == typeName
}

func isHTTPTypeValue(t types.Type, typeName string) bool {
	if t == nil {
		return false
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == typeName
}
