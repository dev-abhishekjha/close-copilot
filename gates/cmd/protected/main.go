// Command protected is gate G1's protected-path check: a protected path
// changes only with the owner's "approved" label.
//
//	go run ./gates/cmd/protected [--base main] [--labels approved] [--report path]
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
	os.Exit(gates.RunProtected(context.Background(), gates.DefaultEnv(), os.Args[1:]))
}
