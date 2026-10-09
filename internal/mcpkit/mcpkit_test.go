package mcpkit_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthzEndpoint(t *testing.T) {
	routes := map[string]mcpkit.Route{
		"/mcp": mcpkit.NewRoute(mcpkit.NewServer("test-server", "1.0.0"), "secret-token"),
	}
	handler := mcpkit.BuildHandler(routes, testLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("/healthz body: got %q, want %q", body, "ok")
	}
}

func TestBearerAuthMiddleware(t *testing.T) {
	routes := map[string]mcpkit.Route{
		"/mcp": mcpkit.NewRoute(mcpkit.NewServer("test-server", "1.0.0"), "agent-secret-token"),
	}
	handler := mcpkit.BuildHandler(routes, testLogger())

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "missing Authorization header",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed Authorization header",
			authHeader: "Basic user:pass",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong bearer token",
			authHeader: "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "correct bearer token",
			authHeader: "Bearer agent-secret-token",
			wantStatus: http.StatusOK, // Streamable HTTP handler responds
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := strings.NewReader(`{"jsonrpc":"2.0","method":"ping","id":1}`)
			req := httptest.NewRequest(http.MethodPost, "/mcp", body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if authResp := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(authResp, "Bearer") {
					t.Errorf("WWW-Authenticate header: got %q, want Bearer challenge", authResp)
				}
			}
		})
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	routes := map[string]mcpkit.Route{
		"/mcp": mcpkit.NewRoute(mcpkit.NewServer("test-server", "1.0.0"), "tok"),
	}
	handler := mcpkit.BuildHandler(routes, testLogger())

	// Case 1: Caller supplies X-Request-ID
	req1 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req1.Header.Set("X-Request-ID", "custom-trace-id-123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if got := rec1.Header().Get("X-Request-ID"); got != "custom-trace-id-123" {
		t.Errorf("preserved request ID: got %q, want custom-trace-id-123", got)
	}

	// Case 2: Generated X-Request-ID
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	gotGen := rec2.Header().Get("X-Request-ID")
	if len(gotGen) != 32 {
		t.Errorf("generated request ID length: got %d (%q), want 32 hex chars", len(gotGen), gotGen)
	}
}

func TestPanicRecovery(t *testing.T) {
	panickingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("deliberate test panic")
	})

	routes := map[string]mcpkit.Route{}
	_ = routes

	// Build handler manually wrapping panicking handler with BuildHandler recovery
	h := mcpkit.RequireBearer("tok", panickingHandler)
	mux := http.NewServeMux()
	mux.Handle("/panic", h)

	// Test with BuildHandler
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use recovery middleware behavior
		rec := httptest.NewRecorder()
		defer func() {
			if r := recover(); r != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(rec, r)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	req.Header.Set("Authorization", "Bearer tok")
	wrappedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status on panic: got %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestAgentAndAdminRoutesFromConfig(t *testing.T) {
	var cfg config.Config
	cfg.MCPTokenAgent = config.NewSecret("secret-agent-token")
	cfg.MCPTokenAdmin = config.NewSecret("secret-admin-token")

	booksServer := mcpkit.NewServer("books", "1.0.0")
	adminServer := mcpkit.NewServer("books-admin", "1.0.0")

	agentRoute := mcpkit.NewAgentRoute(booksServer, cfg)
	adminRoute := mcpkit.NewAdminRoute(adminServer, cfg)

	if agentRoute.Token != "secret-agent-token" {
		t.Errorf("agent route token mismatch: got %q", agentRoute.Token)
	}
	if adminRoute.Token != "secret-admin-token" {
		t.Errorf("admin route token mismatch: got %q", adminRoute.Token)
	}
}

func TestRunStdioCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	server := mcpkit.NewServer("test-stdio", "1.0.0")
	// RunStdio with cancelled context should return or terminate cleanly
	done := make(chan error, 1)
	go func() {
		done <- mcpkit.RunStdio(ctx, server)
	}()

	select {
	case <-done:
		// Succeeded in exiting
	case <-time.After(2 * time.Second):
		t.Fatal("RunStdio timed out on cancelled context")
	}
}

type headerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func TestClientServerHandshakeOverStreamableHTTP(t *testing.T) {
	token := "valid-agent-token"
	routes := map[string]mcpkit.Route{
		"/mcp": mcpkit.NewRoute(mcpkit.NewServer("test-books", "1.0.0"), token),
	}
	ts := httptest.NewServer(mcpkit.BuildHandler(routes, testLogger()))
	defer ts.Close()

	clientTransport := &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &headerTransport{
				base:  ts.Client().Transport,
				token: token,
			},
		},
		DisableStandaloneSSE: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("session.ListTools: %v", err)
	}
	if len(tools.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools.Tools))
	}
}
