//go:build unix

package llm

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup starts the child in its own process group and makes
// cancellation kill the whole group, grandchildren included.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid signals every process in the group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return cmd.Process.Kill()
		}
		return nil
	}
}
