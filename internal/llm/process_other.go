//go:build !unix

package llm

import "os/exec"

// configureProcessGroup is a no-op off Unix; exec's default Cancel kills the
// direct child and WaitDelay bounds the wait on its pipes.
func configureProcessGroup(*exec.Cmd) {}
