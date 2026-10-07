// Command graph lists the tickets ready to build and validates tasks/graph.yaml.
//
//	go run ./gates/cmd/graph ready [--base main] [--json]
//	go run ./gates/cmd/graph check tasks/graph.yaml
//
// Exit codes: 0 pass, 1 gate failure (the report JSON is on stdout), 2 bad
// usage or an I/O error.
package main

import (
	"context"
	"os"

	"github.com/abhishekjha/close-copilot/gates"
)

func main() {
	os.Exit(gates.RunGraph(context.Background(), gates.DefaultEnv(), os.Args[1:]))
}
