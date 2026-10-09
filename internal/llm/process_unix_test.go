//go:build unix

package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run /bin/sh, never a model.

func TestOSProcessRunner_EnvIsExplicit(t *testing.T) {
	t.Setenv("CC_TEST_PARENT_SECRET", "leak")
	r := &OSProcessRunner{}
	out, _, err := r.Run(context.Background(), Command{
		Path: "/bin/sh",
		Args: []string{"-c", `echo "secret=${CC_TEST_PARENT_SECRET:-unset} only=${ONLY:-unset}"`},
		Env:  []string{"ONLY=yes"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "secret=unset only=yes" {
		t.Errorf("child saw %q", got)
	}

	// A nil Env is an empty environment, not the parent's.
	out, _, err = r.Run(context.Background(), Command{
		Path: "/bin/sh",
		Args: []string{"-c", `echo "${CC_TEST_PARENT_SECRET:-unset}"`},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "unset" {
		t.Errorf("nil Env inherited the parent environment: %q", got)
	}
}

func TestOSProcessRunner_Dir(t *testing.T) {
	dir := t.TempDir()
	out, _, err := (&OSProcessRunner{}).Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", "pwd -P"}, Dir: dir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestOSProcessRunner_OutputCaps(t *testing.T) {
	r := &OSProcessRunner{maxStdout: 64, maxStderr: 32}

	out, _, err := r.Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", "i=0; while [ $i -lt 100 ]; do echo 0123456789; i=$((i+1)); done"},
	})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("expected ErrOutputTooLarge for stdout, got %v", err)
	}
	if len(out) > 64 {
		t.Errorf("kept %d stdout bytes, cap is 64", len(out))
	}

	_, errOut, err := r.Run(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"-c", "i=0; while [ $i -lt 100 ]; do echo 0123456789 >&2; i=$((i+1)); done"},
	})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("expected ErrOutputTooLarge for stderr, got %v", err)
	}
	if len(errOut) > 32 {
		t.Errorf("kept %d stderr bytes, cap is 32", len(errOut))
	}

	if _, _, err := r.Run(context.Background(), Command{Path: "/bin/sh", Args: []string{"-c", "echo ok"}}); err != nil {
		t.Errorf("output under the cap should pass: %v", err)
	}
}

func TestOSProcessRunner_KillsProcessGroupOnTimeout(t *testing.T) {
	// The grandchild inherits stdout and would hold the pipe open for 30s if
	// only the direct child were killed.
	r := &OSProcessRunner{waitDelay: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := r.Run(ctx, Command{
		Path: "/bin/sh",
		Args: []string{"-c", "sleep 30 & sleep 30"},
		Env:  []string{"PATH=" + os.Getenv("PATH")},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a killed process")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run returned after %s; the process group was not killed", elapsed)
	}
}
