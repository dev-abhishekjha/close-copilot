package noerpimport

import (
	"slices"
	"strconv"
	"strings"

	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/abhishekjha/close-copilot/gates/analyzers/scope"
)

// Analyzer reports imports of internal/frappe from internal/agent.
var Analyzer = &analysis.Analyzer{
	Name: "noerpimport",
	Doc: `forbid internal/agent from importing internal/frappe

The agent reaches ERPNext only through MCP (the Books MCP server's
read-only tools), never through the ERPNext client. noerpimport reports any
import of internal/frappe, or one of its subpackages, from internal/agent or
one of its subpackages, directly or transitively.

Test files are NOT exempt: an agent test that talks to ERPNext directly
would test a path production never takes.`,
	Run: run,
}

func run(pass *analysis.Pass) (any, error) {
	// InTested: an external test package (internal/agent_test) is in scope too.
	if !scope.InTested(pass.Pkg.Path(), "internal/agent") {
		return nil, nil
	}

	importsByPath := make(map[string]*types.Package)
	for _, imp := range pass.Pkg.Imports() {
		importsByPath[imp.Path()] = imp
		importsByPath[scope.Rel(imp.Path())] = imp
	}

	for _, f := range pass.Files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if scope.In(path, "internal/frappe") {
				pass.Reportf(imp.Pos(), "internal/agent must not import %s: the agent reaches ERPNext only through MCP", path)
				continue
			}
			pkg := importsByPath[path]
			if pkg == nil {
				pkg = importsByPath[scope.Rel(path)]
			}
			if pkg == nil {
				continue
			}
			if chain, target := findFrappeChain(pkg); len(chain) > 0 {
				pass.Reportf(imp.Pos(), "internal/agent imports %s via %s: the agent reaches ERPNext only through MCP", target, strings.Join(chain, " -> "))
			}
		}
	}
	return nil, nil
}

func findFrappeChain(start *types.Package) ([]string, string) {
	type item struct {
		pkg   *types.Package
		chain []string
	}
	visited := map[string]bool{start.Path(): true}
	queue := []item{{pkg: start, chain: []string{scope.Rel(start.Path())}}}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, dep := range curr.pkg.Imports() {
			depRel := scope.Rel(dep.Path())
			if scope.In(dep.Path(), "internal/frappe") {
				return curr.chain, depRel
			}
			if !visited[dep.Path()] {
				visited[dep.Path()] = true
				nextChain := append(slices.Clone(curr.chain), depRel)
				queue = append(queue, item{pkg: dep, chain: nextChain})
			}
		}
	}
	return nil, ""
}
