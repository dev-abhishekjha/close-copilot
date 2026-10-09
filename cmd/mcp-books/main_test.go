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

func testBooksConfig() config.Config {
	return config.Config{
		ERPBaseURL:    "http://localhost:8080",
		ERPSite:       "erp.localhost",
		ERPAPIKey:     config.NewSecret("key"),
		ERPAPISecret:  config.NewSecret("secret"),
		MCPTokenAgent: config.NewSecret("agent-token"),
		MCPTokenAdmin: config.NewSecret("admin-token"),
	}
}

func TestMCPBooksRun_UnknownTransport(t *testing.T) {
	cfg := testBooksConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, log, []string{"--transport=invalid"})
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Errorf("want unknown transport error, got: %v", err)
	}
}

func TestMCPBooksRun_StdioCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testBooksConfig()
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
