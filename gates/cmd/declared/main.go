// Command declared is gate G1's declared-files check: every changed file
// matches a files glob of the spec, no other ticket's spec changes, the
// spec's id matches its file name and the branch name, every commit subject
// on the branch starts with "<ID>:", and the spec's files, risk and
// approved_by are unchanged since they were pinned (on --base, or in the
// branch's "<ID>: spec" commit).
//
//	go run ./gates/cmd/declared --spec specs/CC-xxx.md [--base main] [--report path]
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
	os.Exit(gates.RunDeclared(context.Background(), gates.DefaultEnv(), os.Args[1:]))
}
