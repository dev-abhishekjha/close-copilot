package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
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
		MCPScopeKey:   config.NewSecret("scope-key-for-tests"),
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
	srv, err := newBooksServer(testBooksConfig(), testCompanies, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
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

// TestMCPBooksHTTPRequiresScopeKey: the HTTP server refuses to start
// without MCP_SCOPE_KEY; stdio, a local process, does not need it.
func TestMCPBooksHTTPRequiresScopeKey(t *testing.T) {
	cfg := testBooksConfig()
	cfg.MCPScopeKey = config.Secret{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := run(t.Context(), cfg, log, []string{"--transport=http", "--companies=" + testCompanies, "--addr=127.0.0.1:0"})
	if !errors.Is(err, errNoScopeKey) || !strings.Contains(err.Error(), config.EnvMCPScopeKey) {
		t.Errorf("http without a scope key: %v, want an error naming %s", err, config.EnvMCPScopeKey)
	}
	if _, _, err := httpRoutes(cfg, testCompanies, log, nil); !errors.Is(err, errNoScopeKey) {
		t.Errorf("httpRoutes without a scope key (and no store): %v, want the scope key error first", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, cfg, log, []string{"--transport=stdio", "--companies=" + testCompanies}); err != nil && strings.Contains(err.Error(), config.EnvMCPScopeKey) {
		t.Errorf("stdio refused for want of a scope key: %v", err)
	}
}

// scopeTransport sends a bearer token and, when set, a run scope.
type scopeTransport struct {
	token, scope string
}

func (s scopeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+s.token)
	if s.scope != "" {
		r.Header.Set(mcpkit.ScopeHeader, s.scope)
	}
	return http.DefaultTransport.RoundTrip(r)
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

// TestMCPBooksHTTPScoped drives the handler the binary builds for
// --transport http: /mcp needs a run scope, a scoped call for another
// company is refused as a tool error, and /mcp-admin is not scoped.
func TestMCPBooksHTTPScoped(t *testing.T) {
	cfg := testBooksConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httpTestServer(t, cfg, log)

	if code := postInitialize(t, ts.URL+"/mcp", scopeTransport{token: "agent-token"}); code != http.StatusUnauthorized {
		t.Errorf("/mcp without X-Run-Scope: %d, want 401", code)
	}
	if code := postInitialize(t, ts.URL+"/mcp-admin", scopeTransport{token: "admin-token"}); code == http.StatusUnauthorized {
		t.Errorf("/mcp-admin without X-Run-Scope: 401, want it unscoped")
	}

	sc, err := mcpkit.Window("sharma", "run-bin-test", "2026-09", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	header, err := mcpkit.Mint([]byte(cfg.MCPScopeKey.Reveal()), sc)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: scopeTransport{token: "agent-token", scope: header}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect with a scope: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: books.ToolGetTrialBalance,
		Arguments: map[string]any{"company": "mehta", "from_date": "2026-09-01", "to_date": "2026-09-30"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := ""
	if len(res.Content) > 0 {
		text = res.Content[0].(*mcp.TextContent).Text
	}
	if !res.IsError || !strings.HasPrefix(text, "out_of_scope") {
		t.Errorf("sharma's scope reading mehta: IsError=%v %q, want an out_of_scope tool error", res.IsError, text)
	}
}

// fakeAdminStore is the HTTP server's store without Postgres: it records
// audit rows and has no proposals.
type fakeAdminStore struct {
	mu   sync.Mutex
	rows []store.AuditLogEntry
}

func (f *fakeAdminStore) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, e)
	return int64(len(f.rows)), nil
}

func (f *fakeAdminStore) WithProposalLock(_ context.Context, id uuid.UUID, _ func(context.Context, store.JournalProposal, func(context.Context, store.JournalProposal) error) error) error {
	return fmt.Errorf("%w: journal proposal %s", store.ErrNotFound, id)
}

func httpTestServer(t *testing.T, cfg config.Config, log *slog.Logger) *httptest.Server {
	t.Helper()
	routes, opts, err := httpRoutes(cfg, testCompanies, log, &fakeAdminStore{})
	if err != nil {
		t.Fatalf("httpRoutes: %v", err)
	}
	ts := httptest.NewServer(mcpkit.BuildHandler(routes, log, opts...))
	t.Cleanup(ts.Close)
	return ts
}

func TestMCPBooksHTTPNeedsStore(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := httpRoutes(testBooksConfig(), testCompanies, log, nil); !errors.Is(err, errNoAdminStore) {
		t.Errorf("httpRoutes without a store: %v, want errNoAdminStore", err)
	}
}

// TestMCPBooksTokensDontCross: the agent token gets 401 on /mcp-admin, and
// the admin token gets 401 on /mcp even with a validly minted run scope.
func TestMCPBooksTokensDontCross(t *testing.T) {
	cfg := testBooksConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httpTestServer(t, cfg, log)

	if code := postInitialize(t, ts.URL+mcpkit.AdminPath, scopeTransport{token: "agent-token"}); code != http.StatusUnauthorized {
		t.Errorf("agent token on /mcp-admin: %d, want 401", code)
	}
	sc, err := mcpkit.Window("sharma", "run-bin-test", "2026-09", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	header, err := mcpkit.Mint([]byte(cfg.MCPScopeKey.Reveal()), sc)
	if err != nil {
		t.Fatal(err)
	}
	if code := postInitialize(t, ts.URL+"/mcp", scopeTransport{token: "admin-token", scope: header}); code != http.StatusUnauthorized {
		t.Errorf("admin token with a valid scope on /mcp: %d, want 401", code)
	}
	if code := postInitialize(t, ts.URL+"/mcp", scopeTransport{token: "agent-token", scope: header}); code == http.StatusUnauthorized {
		t.Errorf("agent token with a valid scope on /mcp: 401, want it accepted")
	}
}

// TestMCPBooksAdminRouteTools: /mcp-admin lists exactly the admin write
// tool, /mcp never does, and an admin call is audited as books-admin.
func TestMCPBooksAdminRouteTools(t *testing.T) {
	cfg := testBooksConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &fakeAdminStore{}
	routes, opts, err := httpRoutes(cfg, testCompanies, log, st)
	if err != nil {
		t.Fatalf("httpRoutes: %v", err)
	}
	ts := httptest.NewServer(mcpkit.BuildHandler(routes, log, opts...))
	t.Cleanup(ts.Close)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + mcpkit.AdminPath,
		HTTPClient: &http.Client{Transport: scopeTransport{token: "admin-token"}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect /mcp-admin: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != books.ToolPostApprovedJournalEntry {
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("/mcp-admin tools %v, want only %s", names, books.ToolPostApprovedJournalEntry)
	}
	if slices.Contains(books.ToolNames, books.ToolPostApprovedJournalEntry) {
		t.Error("the admin tool is in the read-only tool list")
	}

	id := uuid.New().String()
	call, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: books.ToolPostApprovedJournalEntry,
		Arguments: map[string]any{"proposal_id": id}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !call.IsError || !strings.HasPrefix(call.Content[0].(*mcp.TextContent).Text, books.CodeNotFound) {
		t.Errorf("unknown proposal: IsError=%v %v, want not_found", call.IsError, call.Content)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.rows) != 1 {
		t.Fatalf("audit rows %d, want 1", len(st.rows))
	}
	r := st.rows[0]
	if *r.Server != "books-admin" || *r.Tool != books.ToolPostApprovedJournalEntry || r.Actor != "admin" || !strings.Contains(string(r.Args), id) {
		t.Errorf("audit row: actor %q server %q tool %q args %s", r.Actor, *r.Server, *r.Tool, r.Args)
	}
}
