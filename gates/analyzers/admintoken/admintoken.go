// Package admintoken defines an analyzer that confines the MCP admin token
// to the code that loads, verifies and uses it.
package admintoken

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
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
(and subpackages), which uses it. Everywhere else admintoken reports:

  - any reference to the field config.Config.MCPTokenAdmin (selector or
    composite-literal key);
  - any reference to the constant config.EnvMCPTokenAdmin;
  - a string literal, or a constant string concatenation, equal to the
    variable's name (MCP_TOKEN_ADMIN).

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
	for _, f := range pass.Files {
		name := pass.Fset.File(f.Pos()).Name()
		if scope.IsTestFile(name) {
			continue
		}
		if mcpkit && isAuthFile(scope.Base(name)) {
			continue
		}
		checkFile(pass, f)
	}
	return nil, nil
}

func isAuthFile(base string) bool {
	return strings.HasPrefix(base, "auth") && strings.HasSuffix(base, ".go")
}

const allowed = "only internal/config, internal/mcpkit/auth*.go and internal/approvals may use it"

func checkFile(pass *analysis.Pass, f *ast.File) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			switch obj := pass.TypesInfo.Uses[n].(type) {
			case *types.Var:
				if isConfigAdminField(obj) {
					pass.Reportf(n.Pos(), "reference to config.Config.MCPTokenAdmin in %s: %s", scope.Rel(pass.Pkg.Path()), allowed)
				}
			case *types.Const:
				if obj.Name() == "EnvMCPTokenAdmin" && obj.Pkg() != nil && scope.In(obj.Pkg().Path(), "internal/config") {
					pass.Reportf(n.Pos(), "reference to config.EnvMCPTokenAdmin in %s: %s", scope.Rel(pass.Pkg.Path()), allowed)
				}
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if s, err := strconv.Unquote(n.Value); err == nil && s == envName {
					pass.Reportf(n.Pos(), "string %q in %s: %s", envName, scope.Rel(pass.Pkg.Path()), allowed)
				}
			}
		case *ast.BinaryExpr:
			tv, ok := pass.TypesInfo.Types[n]
			if ok && tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == envName {
				pass.Reportf(n.Pos(), "string %q in %s: %s", envName, scope.Rel(pass.Pkg.Path()), allowed)
				return false
			}
		}
		return true
	})
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
