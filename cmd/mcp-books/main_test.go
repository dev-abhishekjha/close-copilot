package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
)

// testCompanies is the repo's company profile directory, relative to this
// package.
const testCompanies = "../../config/companies"

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
		errCh <- run(ctx, cfg, log, []string{"--transport=stdio", "--companies=" + testCompanies})
	}()

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stdio run timed out on cancelled context")
	}
}

func TestMCPBooksRun_BadCompaniesDir(t *testing.T) {
	cfg := testBooksConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, log, []string{"--transport=stdio", "--companies=" + t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "company profiles") {
		t.Errorf("want a company profiles error, got: %v", err)
	}
}

// TestMCPBooksListsReadOnlyTools connects a real MCP client to the /mcp
// server and checks it lists exactly the seven read-only books tools, each
// with a description, input and output schemas and the read-only hint.
func TestMCPBooksListsReadOnlyTools(t *testing.T) {
	srv, err := newBooksServer(testBooksConfig(), testCompanies, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newBooksServer: %v", err)
	}
	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if len(tool.Description) < 80 {
			t.Errorf("%s: description %q is too thin for the model", tool.Name, tool.Description)
		}
		if !strings.Contains(tool.Description, "paise") {
			t.Errorf("%s: description doesn't say amounts are paise", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: missing the read-only hint", tool.Name)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s: missing input or output schema", tool.Name)
		}
	}
	want := slices.Clone(books.ToolNames)
	slices.Sort(want)
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if len(names) != 7 {
		t.Errorf("got %d tools, want 7", len(names))
	}
}
