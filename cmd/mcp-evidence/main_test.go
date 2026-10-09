package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func testEvidenceConfig(dbURL string) config.Config {
	return config.Config{
		DatabaseURL:   config.NewSecret(dbURL),
		MCPTokenAgent: config.NewSecret("agent-secret-token"),
	}
}

func setupTestDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	var databaseURL string
	if envURL := os.Getenv("TEST_DATABASE_URL"); envURL != "" {
		databaseURL = envURL
	} else {
		pgContainer, err := postgres.Run(ctx,
			"pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot_mcp_evidence"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start testcontainers postgres: %v", err)
		}
		t.Cleanup(func() {
			_ = testcontainers.TerminateContainer(pgContainer)
		})

		connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("get postgres connection string: %v", err)
		}
		databaseURL = connStr
	}

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := st.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("migration: %v", err)
	}

	return databaseURL
}

func TestMCPEvidenceRun_UnknownTransport(t *testing.T) {
	cfg := testEvidenceConfig("postgres://localhost:5432/copilot")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, log, []string{"--transport=invalid"})
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Errorf("want unknown transport error, got: %v", err)
	}
}

func TestMCPEvidenceRun_StdioCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testEvidenceConfig("postgres://localhost:5432/copilot")
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

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func TestMCPEvidenceRun_HTTP_StreamableHandshake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dbURL := setupTestDB(t)
	cfg := testEvidenceConfig(dbURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, cfg, log, []string{"--transport=http", "--addr=" + addr})
	}()

	// Wait for server to start responding on /healthz
	baseURL := "http://" + addr
	ready := false
	for range 60 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("mcp-evidence server failed to start within timeout")
	}

	// 1. Verify unauthorized access to /mcp returns 401
	unauthReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/mcp", strings.NewReader("{}"))
	unauthReq.Header.Set("Content-Type", "application/json")
	unauthReq.Header.Set("Accept", "application/json, text/event-stream")
	unauthResp, err := http.DefaultClient.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauth request: %v", err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth status: got %d, want 401", unauthResp.StatusCode)
	}

	// 2. Connect MCP client over Streamable HTTP with agent bearer token
	clientTransport := &mcp.StreamableClientTransport{
		Endpoint: baseURL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &authTransport{
				token: cfg.MCPTokenAgent.Reveal(),
			},
		},
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer sess.Close()

	// 3. List tools: expect exactly list_bank_lines and list_gstr2b_entries
	toolsList, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("sess.ListTools: %v", err)
	}
	if len(toolsList.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(toolsList.Tools))
	}
	found := make(map[string]bool)
	for _, tool := range toolsList.Tools {
		found[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s missing ReadOnlyHint", tool.Name)
		}
	}
	if !found["list_bank_lines"] || !found["list_gstr2b_entries"] {
		t.Errorf("expected list_bank_lines and list_gstr2b_entries, got %v", found)
	}

	// Graceful shutdown
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
