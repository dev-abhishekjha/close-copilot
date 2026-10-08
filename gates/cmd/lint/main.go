// Command lint runs the repo's custom analyzers (CC-002): nofloat,
// admintoken, noerpimport and egress. Usage: go run ./gates/cmd/lint ./...
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/abhishekjha/close-copilot/gates/analyzers/admintoken"
	"github.com/abhishekjha/close-copilot/gates/analyzers/egress"
	"github.com/abhishekjha/close-copilot/gates/analyzers/noerpimport"
	"github.com/abhishekjha/close-copilot/gates/analyzers/nofloat"
)

func main() {
	multichecker.Main(
		nofloat.Analyzer,
		admintoken.Analyzer,
		noerpimport.Analyzer,
		egress.Analyzer,
	)
}
