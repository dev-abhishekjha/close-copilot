// Package admintoken defines an analyzer that confines the MCP admin token
// to the code that loads, verifies and uses it.
package admintoken

import (
	"go/ast"
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/abhishekjha/close-copilot/gates/analyzers/scope"
)

// envName is the variable's name. It is built at run time so that this
// analyzer's own source holds no constant equal to it.
var envName = strings.Join([]string{"MCP", "TOKEN", "ADMIN"}, "_")

// Analyzer reports references to the MCP admin token outside its allowed
// places.
var Analyzer = &analysis.Analyzer{
	Name: "admintoken",
	Doc: `confine the MCP admin token to internal/config, internal/mcpkit/auth*.go and internal/approvals

The admin token unlocks the write tools on /mcp-admin, so only three places
may name it: internal/config (and subpackages), which loads it; files named
auth*.go in package internal/mcpkit, which verify it; and internal/approvals
(and subpackages), which uses it. These are matched by exact import path: a
directory such as internal/approvals_test is not internal/approvals.
Everywhere else admintoken reports:

  - any reference to the field config.Config.MCPTokenAdmin (selector or
    composite-literal key);
  - any constant string expression that contains the variable's name
    (the words MCP, TOKEN and ADMIN joined by underscores) anywhere in
    its value: the constant config.EnvMCPTokenAdmin, any other constant
    declared equal to it or built from it in any package (for example a
    re-export in an allowed package), a string literal, a constant
    concatenation or conversion, or a template such as "$NAME" or
    "${NAME}" passed to os.ExpandEnv.

One narrower exception: package cmd/mcp-books (exactly, not its
subpackages) hosts /mcp-admin, so it may list the variable's name to
require it at startup. There the name is allowed only as a direct element
of a []string (or [N]string) composite literal, the list passed to
cli.Main, and only when the element's value is exactly the name; it is
reported in every other position, including as an argument to os.Getenv
and inside a longer string. In cmd/mcp-books admintoken also reports any
reference to os.Getenv, os.LookupEnv, os.Environ, os.ExpandEnv,
syscall.Getenv and syscall.Environ: the package gets its configuration
from config.Config and must not read the environment itself. It must never
touch the value: config.Config.MCPTokenAdmin is still reported there.

It uses type information, so a field named MCPTokenAdmin on another struct
is not reported.

Test files (*_test.go) are exempt: tests set fake tokens and never reach a
real server.`,
	Run: run,
}

func run(pass *analysis.Pass) (any, error) {
	path := pass.Pkg.Path()
	if scope.In(path, "internal/config", "internal/approvals") {
		return nil, nil
	}
	mcpkit := scope.Is(path, "internal/mcpkit")
	// cmd/mcp-books may list the variable's name (to require it at
	// startup) but never read its value.
	mcpBooks := scope.Is(path, "cmd/mcp-books")
	for _, f := range pass.Files {
		name := pass.Fset.File(f.Pos()).Name()
		if scope.IsTestFile(name) {
			continue
		}
		if mcpkit && isAuthFile(scope.Base(name)) {
			continue
		}
		checkFile(pass, f, mcpBooks)
	}
	return nil, nil
}

func isAuthFile(base string) bool {
	return strings.HasPrefix(base, "auth") && strings.HasSuffix(base, ".go")
}

const (
	allowed      = "only internal/config, internal/mcpkit/auth*.go and internal/approvals may use it"
	allowedBooks = "cmd/mcp-books may name it only as an element of a []string literal (the variables required at startup)"
)

// envReaders are the functions that read the process environment, by
// package path.
var envReaders = map[string]map[string]bool{
	"os":      {"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true},
	"syscall": {"Getenv": true, "Environ": true},
}

// checkFile reports admin-token references in f. When mcpBooks is set, the
// variable's name is allowed as a direct element of a string-list composite
// literal whose value is exactly the name, and environment readers are
// reported. Any other constant string containing the name, and the
// Config.MCPTokenAdmin field, are always reported.
func checkFile(pass *analysis.Pass, f *ast.File, mcpBooks bool) {
	listed := map[ast.Expr]bool{}
	nameRule := allowed
	if mcpBooks {
		nameRule = allowedBooks
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if !isCLIMain(pass, call.Fun) {
				return true
			}
			cl, ok := ast.Unparen(call.Args[1]).(*ast.CompositeLit)
			if !ok || !isStringList(pass.TypesInfo.TypeOf(cl)) {
				return true
			}
			for _, e := range cl.Elts {
				if _, keyed := e.(*ast.KeyValueExpr); !keyed {
					listed[e] = true
				}
			}
			return true
		})
	}
	where := scope.Rel(pass.Pkg.Path())

	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if s, ok := constString(pass, e); ok && strings.Contains(s, envName) {
			exact := s == envName
			if !exact || !listed[e] {
				relation := "is"
				if !exact {
					relation = "contains"
				}
				if c, ok := constName(pass, e); ok {
					pass.Reportf(e.Pos(), "reference to %s in %s: its value %s the admin token's variable name %q; %s", c, where, relation, envName, nameRule)
				} else if exact {
					pass.Reportf(e.Pos(), "string %q in %s: %s", s, where, nameRule)
				} else {
					pass.Reportf(e.Pos(), "string %q in %s contains the admin token's variable name %q; %s", s, where, envName, nameRule)
				}
			}
			return false // one report for the outermost constant expression
		}
		id, ok := e.(*ast.Ident)
		if !ok {
			return true
		}
		switch obj := pass.TypesInfo.Uses[id].(type) {
		case *types.Var:
			if isConfigAdminField(obj) {
				pass.Reportf(id.Pos(), "reference to config.Config.MCPTokenAdmin in %s: %s", where, allowed)
			}
		case *types.Func:
			if mcpBooks && isEnvReader(obj) {
				pass.Reportf(id.Pos(), "%s.%s in %s: this package hosts /mcp-admin and must not read the environment; take values from config.Config", obj.Pkg().Name(), obj.Name(), where)
			}
		}
		return true
	})
}

// constString returns the value of e when e is a constant string
// expression.
func constString(pass *analysis.Pass, e ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// constName returns pkg.Name when e is a (possibly qualified) reference to
// a named constant.
func constName(pass *analysis.Pass, e ast.Expr) (string, bool) {
	var id *ast.Ident
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return "", false
	}
	c, ok := pass.TypesInfo.Uses[id].(*types.Const)
	if !ok || c.Pkg() == nil {
		return "", false
	}
	return c.Pkg().Name() + "." + c.Name(), true
}

// isStringList reports whether t is a slice or array of strings.
func isStringList(t types.Type) bool {
	if t == nil {
		return false
	}
	var elem types.Type
	switch u := t.Underlying().(type) {
	case *types.Slice:
		elem = u.Elem()
	case *types.Array:
		elem = u.Elem()
	default:
		return false
	}
	b, ok := elem.Underlying().(*types.Basic)
	return ok && b.Kind() == types.String
}

// isEnvReader reports whether fn is one of the package-level functions
// that read the process environment.
func isEnvReader(fn *types.Func) bool {
	if fn.Pkg() == nil {
		return false
	}
	if sig, ok := fn.Type().(*types.Signature); !ok || sig.Recv() != nil {
		return false
	}
	return envReaders[fn.Pkg().Path()][fn.Name()]
}

// isConfigAdminField reports whether v is the MCPTokenAdmin field of the
// Config struct declared in internal/config.
func isConfigAdminField(v *types.Var) bool {
	if !v.IsField() || v.Name() != "MCPTokenAdmin" || v.Pkg() == nil || !scope.In(v.Pkg().Path(), "internal/config") {
		return false
	}
	tn, ok := v.Pkg().Scope().Lookup("Config").(*types.TypeName)
	if !ok {
		return false
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for fld := range st.Fields() {
		if fld == v.Origin() {
			return true
		}
	}
	return false
}

// isCLIMain reports whether fun resolves to internal/cli.Main.
func isCLIMain(pass *analysis.Pass, fun ast.Expr) bool {
	var id *ast.Ident
	switch e := ast.Unparen(fun).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return false
	}
	fn, ok := pass.TypesInfo.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return scope.Is(fn.Pkg().Path(), "internal/cli") && fn.Name() == "Main"
}
