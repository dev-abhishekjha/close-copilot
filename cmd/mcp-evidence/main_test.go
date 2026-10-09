package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func testEvidenceConfig() config.Config {
	return config.Config{
		DatabaseURL:   config.NewSecret("postgres://localhost:5432/copilot"),
		MCPTokenAgent: config.NewSecret("agent-token"),
	}
}

func TestMCPEvidenceRun_UnknownTransport(t *testing.T) {
	cfg := testEvidenceConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, log, []string{"--transport=invalid"})
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Errorf("want unknown transport error, got: %v", err)
	}
}

func TestMCPEvidenceRun_StdioCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testEvidenceConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, cfg, log, []string{"--transport=stdio"})
	}()

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stdio run timed out on cancelled context")
	}
}
