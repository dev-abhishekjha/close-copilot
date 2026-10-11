package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func testEvidenceConfig(dbURL string) config.Config {
	return config.Config{
		DatabaseURL:   config.NewSecret(dbURL),
		MCPTokenAgent: config.NewSecret("agent-secret-token"),
		MCPScopeKey:   config.NewSecret("evidence-scope-key-for-tests"),
	}
}

// testScope mints a run scope for company's 2026-09 close with cfg's key.
func testScope(t *testing.T, cfg config.Config, company string) string {
	t.Helper()
	sc, err := mcpkit.Window(company, "run-bin-test", "2026-09", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcpkit.Mint([]byte(cfg.MCPScopeKey.Reveal()), sc)
	if err != nil {
		t.Fatal(err)
	}
	return h
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
	scope string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.scope != "" {
		clone.Header.Set(mcpkit.ScopeHeader, t.scope)
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
	// 2. Without a run scope, the agent token alone gets 401.
	noScope := &authTransport{token: cfg.MCPTokenAgent.Reveal()}
	if code := postInitialize(t, baseURL+"/mcp", noScope); code != http.StatusUnauthorized {
		t.Errorf("no run scope: got %d, want 401", code)
	}

	httpClient := &http.Client{
		Transport: &authTransport{
			token: cfg.MCPTokenAgent.Reveal(),
			scope: testScope(t, cfg, "sharma"),
		},
	}
	clientTransport := &mcp.StreamableClientTransport{
		Endpoint:             baseURL + "/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}

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

	// Close client session and idle connections so server shutdown doesn't wait
	_ = sess.Close()
	httpClient.CloseIdleConnections()
	http.DefaultClient.CloseIdleConnections()

	// Graceful shutdown
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error on shutdown: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func postInitialize(t *testing.T, url string, tr http.RoundTripper) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestMCPEvidenceHTTPRequiresScopeKey: the HTTP server refuses to start
// without MCP_SCOPE_KEY, before it opens the database; stdio, a local
// process, does not need it.
func TestMCPEvidenceHTTPRequiresScopeKey(t *testing.T) {
	cfg := testEvidenceConfig("postgres://localhost:5432/copilot")
	cfg.MCPScopeKey = config.Secret{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := run(t.Context(), cfg, log, []string{"--transport=http", "--addr=127.0.0.1:0"})
	if !errors.Is(err, errNoScopeKey) || !strings.Contains(err.Error(), config.EnvMCPScopeKey) {
		t.Errorf("http without a scope key: %v, want an error naming %s", err, config.EnvMCPScopeKey)
	}
	if _, _, err := httpRoutes(cfg, nil); !errors.Is(err, errNoScopeKey) {
		t.Errorf("httpRoutes without a scope key: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, cfg, log, []string{"--transport=stdio"}); err != nil && strings.Contains(err.Error(), config.EnvMCPScopeKey) {
		t.Errorf("stdio refused for want of a scope key: %v", err)
	}
}

// TestMCPEvidenceHTTPScoped drives the handler the binary builds for
// --transport http, over a nil store: /mcp needs a run scope, and a scoped
// call for another company is refused as a tool error before any Postgres
// read (a read would panic on the nil store). The evidence server has no
// /mcp-admin route, so that path is a 404, never a 401.
func TestMCPEvidenceHTTPScoped(t *testing.T) {
	cfg := testEvidenceConfig("postgres://unused")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	routes, opts, err := httpRoutes(cfg, nil)
	if err != nil {
		t.Fatalf("httpRoutes: %v", err)
	}
	ts := httptest.NewServer(mcpkit.BuildHandler(routes, log, opts...))
	t.Cleanup(ts.Close)

	agent := cfg.MCPTokenAgent.Reveal()
	if code := postInitialize(t, ts.URL+"/mcp", &authTransport{token: agent}); code != http.StatusUnauthorized {
		t.Errorf("/mcp without X-Run-Scope: %d, want 401", code)
	}
	if code := postInitialize(t, ts.URL+"/mcp-admin", &authTransport{token: agent}); code == http.StatusUnauthorized {
		t.Errorf("/mcp-admin without X-Run-Scope: 401, want it unscoped")
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: &authTransport{token: agent, scope: testScope(t, cfg, "sharma")}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect with a scope: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	for tool, args := range map[string]map[string]any{
		"list_bank_lines":     {"company": "mehta", "from_date": "2026-09-01", "to_date": "2026-09-30"},
		"list_gstr2b_entries": {"company": "mehta", "period": "2026-09"},
	} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		text := ""
		if len(res.Content) > 0 {
			text = res.Content[0].(*mcp.TextContent).Text
		}
		if !res.IsError || !strings.HasPrefix(text, "out_of_scope") {
			t.Errorf("%s: sharma's scope reading mehta: IsError=%v %q, want an out_of_scope tool error", tool, res.IsError, text)
		}
	}
}
