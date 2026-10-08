// Package noerpimport defines an analyzer that keeps internal/agent off the
// ERPNext client.
package noerpimport

import (
	"strconv"

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
one of its subpackages.

Test files are NOT exempt: an agent test that talks to ERPNext directly
would test a path production never takes.`,
	Run: run,
}

func run(pass *analysis.Pass) (any, error) {
	// InTested: an external test package (internal/agent_test) is in scope too.
	if !scope.InTested(pass.Pkg.Path(), "internal/agent") {
		return nil, nil
	}
	for _, f := range pass.Files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if scope.In(path, "internal/frappe") {
				pass.Reportf(imp.Pos(), "internal/agent must not import %s: the agent reaches ERPNext only through MCP", path)
			}
		}
	}
	return nil, nil
}
