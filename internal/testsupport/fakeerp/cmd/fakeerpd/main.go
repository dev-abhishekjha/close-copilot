// Command fakeerpd is a test helper: it serves the fakeerp synthetic month
// (a fake ERPNext, the books MCP server over it, and the evidence MCP
// server over DATABASE_URL) on loopback, prints one JSON line with the two
// MCP URLs and the agent token, and serves until stdin closes or it is
// signalled.
//
// It exists so tests in internal/agent, which may not link the ERPNext
// client even in tests (noerpimport), can run against the real MCP
// servers: they start this binary and receive only URLs and a token.
// Synthetic data only; never point it at a real database.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

// Ready is the JSON line fakeerpd prints once it serves.
type Ready struct {
	BooksURL    string `json:"books_url"`
	EvidenceURL string `json:"evidence_url"`
	Token       string `json:"token"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeerpd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	statement := flag.String("statement", "skeleton", "bank lines to seed: skeleton, parity or none")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv(config.EnvDatabaseURL)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()

	var lines []store.BankLine
	switch *statement {
	case "skeleton":
		lines, err = fakeerp.SkeletonStatement()
	case "parity":
		lines, err = fakeerp.ParityStatement()
	case "none":
	default:
		return fmt.Errorf("unknown -statement %q", *statement)
	}
	if err != nil {
		return err
	}
	if err := fakeerp.Seed(ctx, st, lines); err != nil {
		return err
	}

	stack, err := fakeerp.Start(st, "")
	if err != nil {
		return err
	}
	defer stack.Close()

	if err := json.NewEncoder(os.Stdout).Encode(Ready{BooksURL: stack.BooksURL, EvidenceURL: stack.EvidenceURL, Token: stack.Token}); err != nil {
		return err
	}

	// Serve until the parent closes stdin or signals.
	eof := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, os.Stdin)
		eof <- err
	}()
	select {
	case <-ctx.Done():
	case err := <-eof:
		if err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
	}
	return nil
}
