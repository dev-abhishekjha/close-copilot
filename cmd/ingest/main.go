// Command ingest converts the document corpus with docling, chunks it, embeds it with TEI and stores it in Postgres.
//
// Usage:
//
//	ingest [--corpus DIR] [--keep-docling]
//
// A document whose PDF has not changed since the last run is skipped before
// docling or TEI is called. After a run that converted something, the docling
// container is stopped to free memory unless --keep-docling is set.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/retrieval"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// stopArgs is `docker compose ... --profile ingest stop`, as in the Makefile.
var stopArgs = []string{"compose", "-f", "deploy/docker-compose.yml", "--profile", "ingest", "stop"}

const stopTimeout = 2 * time.Minute

func main() {
	args := os.Args[1:]
	// cli.Run's flag set only knows --version; everything after "--" reaches run.
	if !slices.Contains(args, "--version") && !slices.Contains(args, "-version") {
		args = append([]string{"--"}, args...)
	}
	os.Exit(cli.Run("ingest", args, os.Stdout, os.Stderr, config.FromEnv, []string{
		config.EnvDatabaseURL,
		config.EnvTEIEmbedURL,
		config.EnvTEIRerankURL,
		config.EnvDoclingURL,
	}, run))
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	corpus := fs.String("corpus", "corpus", "corpus directory")
	keep := fs.Bool("keep-docling", false, "leave the docling container running")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("ingest: %w; usage: ingest [--corpus DIR] [--keep-docling]", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("ingest: unexpected arguments %q", fs.Args())
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()
	tei, err := retrieval.NewTEI(cfg)
	if err != nil {
		return err
	}
	doc, err := retrieval.NewDocling(cfg)
	if err != nil {
		return err
	}

	stats, err := retrieval.Ingest(ctx, retrieval.IngestDeps{
		Converter: doc, Embedder: tei, Store: st, Log: log,
	}, *corpus)
	log.Info("ingest finished",
		"documents", stats.Documents,
		"chunks", stats.Chunks,
		"skipped", stats.Skipped,
		"seconds", stats.Duration.Seconds())

	if stats.Converted > 0 && !*keep {
		stopDocling(ctx, log)
	}
	return err
}

// stopDocling stops the docling container. A failure is a warning.
func stopDocling(ctx context.Context, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", stopArgs...).CombinedOutput(); err != nil {
		log.Warn("could not stop docling", "err", err, "output", string(out))
	}
}
