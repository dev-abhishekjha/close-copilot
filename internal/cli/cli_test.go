package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func noop(context.Context, config.Config, *slog.Logger, []string) error { return nil }

func TestVersionFlagSkipsConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	load := func(...string) (config.Config, error) {
		t.Fatal("config must not load for -version")
		return config.Config{}, nil
	}
	if code := Run("agent", []string{"-version"}, &out, &errOut, load, nil, noop); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "agent ") {
		t.Errorf("unexpected version output %q", out.String())
	}
}

func TestMissingConfigExitsNonZeroWithOneMessage(t *testing.T) {
	var out, errOut bytes.Buffer
	load := func(required ...string) (config.Config, error) {
		return config.Load(func(string) (string, bool) { return "", false }, required...)
	}
	code := Run("agent", nil, &out, &errOut, load,
		[]string{config.EnvDatabaseURL, config.EnvMCPTokenAgent}, noop)
	if code == 0 {
		t.Fatal("want a non-zero exit code")
	}
	msg := errOut.String()
	if !strings.Contains(msg, config.EnvDatabaseURL) || !strings.Contains(msg, config.EnvMCPTokenAgent) {
		t.Errorf("stderr %q should name both missing variables", msg)
	}
}

func TestRunErrorExitsNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	load := func(...string) (config.Config, error) { return config.Config{}, nil }
	fail := func(context.Context, config.Config, *slog.Logger, []string) error { return errors.New("boom") }
	if code := Run("agent", nil, &out, &errOut, load, nil, fail); code != 1 {
		t.Fatalf("want exit code 1, got %d", code)
	}
}

func TestRemainingArgsReachRun(t *testing.T) {
	var out, errOut bytes.Buffer
	load := func(...string) (config.Config, error) { return config.Config{}, nil }
	var got []string
	capture := func(_ context.Context, _ config.Config, _ *slog.Logger, args []string) error {
		got = args
		return nil
	}
	Run("seed", []string{"all", "--suite", "suite-skeleton"}, &out, &errOut, load, nil, capture)
	if strings.Join(got, " ") != "all --suite suite-skeleton" {
		t.Errorf("args = %q", got)
	}
}
