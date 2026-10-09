package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const (
	// maxCLIStdout bounds what is read from a subprocess's stdout.
	maxCLIStdout = 4 << 20
	// maxCLIStderr bounds what is read from a subprocess's stderr.
	maxCLIStderr = 64 << 10
	// cliWaitDelay bounds how long Wait lingers on pipes after the process is
	// killed (a grandchild may still hold them).
	cliWaitDelay = 5 * time.Second
)

// ErrOutputTooLarge reports a subprocess that wrote more than the runner's cap
// to stdout or stderr. The output is discarded past the cap and the call fails.
var ErrOutputTooLarge = errors.New("llm: subprocess output exceeds limit")

// Command describes one subprocess invocation.
type Command struct {
	Path  string
	Args  []string
	Stdin []byte
	// Dir is the working directory.
	Dir string
	// Env is the complete environment as KEY=value pairs. It is never merged
	// with the parent's environment; nil means an empty environment.
	Env []string
}

// ProcessRunner runs a process and returns its outputs.
type ProcessRunner interface {
	Run(ctx context.Context, cmd Command) (stdout, stderr []byte, err error)
}

// OSProcessRunner runs processes with os/exec. Output is capped (4 MiB stdout,
// 64 KiB stderr); on Unix the child gets its own process group and
// cancellation kills the whole group, so a grandchild holding a pipe cannot
// keep Run blocked past the context deadline.
type OSProcessRunner struct {
	// Zero values use the package defaults; tests set smaller limits.
	maxStdout int
	maxStderr int
	waitDelay time.Duration
}

// Run executes cmd and waits for it to finish or for ctx to end.
func (r *OSProcessRunner) Run(ctx context.Context, c Command) ([]byte, []byte, error) {
	//nolint:gosec // G204: path and arguments are built by the Claude CLI provider, not taken from model output.
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{}, c.Env...) // non-nil: never inherit the parent environment
	if len(c.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	stdout := &cappedBuffer{limit: orDefault(r.maxStdout, maxCLIStdout)}
	stderr := &cappedBuffer{limit: orDefault(r.maxStderr, maxCLIStderr)}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = cliWaitDelay
	if r.waitDelay > 0 {
		cmd.WaitDelay = r.waitDelay
	}
	configureProcessGroup(cmd)

	err := cmd.Run()
	switch {
	case stdout.overflow:
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%w: stdout over %d bytes", ErrOutputTooLarge, stdout.limit)
	case stderr.overflow:
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%w: stderr over %d bytes", ErrOutputTooLarge, stderr.limit)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// cappedBuffer keeps the first limit bytes and drains the rest, so the child
// never blocks on a full pipe; overflow records that bytes were dropped.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
