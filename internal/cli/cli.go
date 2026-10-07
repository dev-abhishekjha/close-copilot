// Package cli is the shared entry point for every binary under cmd/: version
// flag, JSON logging, config loading and signal handling, so each main.go
// stays a few lines long.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/config"
)

// RunFunc is a binary's body. It receives a context cancelled on SIGINT or
// SIGTERM, the loaded config, a JSON logger and the arguments left after
// flag parsing.
type RunFunc func(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error

// Main runs a binary named name and exits the process with its status.
// required lists the environment variables the binary can't run without.
func Main(name string, required []string, run RunFunc) {
	os.Exit(Run(name, os.Args[1:], os.Stdout, os.Stderr, config.FromEnv, required, run))
}

// Run is Main without os.Exit, for tests. loadCfg is usually config.FromEnv.
func Run(name string, args []string, stdout, stderr io.Writer,
	loadCfg func(required ...string) (config.Config, error),
	required []string, run RunFunc) int {

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", name, buildinfo.String())
		return 0
	}

	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("service", name)

	cfg, err := loadCfg(required...)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "version", buildinfo.String())
	if err := run(ctx, cfg, log, fs.Args()); err != nil {
		log.Error("exited with error", "err", err)
		return 1
	}
	return 0
}
