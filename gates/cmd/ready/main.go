// Command ready is gate G0: a spec is ready to hand to a worker.
//
//	go run ./gates/cmd/ready specs/CC-xxx.md [--base main] [--report path]
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
	os.Exit(gates.RunReady(context.Background(), gates.DefaultEnv(), os.Args[1:]))
}
