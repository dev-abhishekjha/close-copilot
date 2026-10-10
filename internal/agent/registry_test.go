package agent

// Registry tests (CC-702). The agent may not import internal/books, so the
// servers here are small in-test MCP servers built with the SDK that carry
// the real servers' tool names, served at /mcp through mcpkit.BuildHandler
// (bearer auth included) behind httptest. No Docker, no network beyond
// loopback.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// testToken is a made-up agent token for the in-process servers.
const testToken = "agent-test-token-7f3c"

// resultMarker is free text a tool returns; it must never reach a log.
const resultMarker = "REMARK-MARKER-do-not-log"

func ro() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

// testServer is one in-process MCP server at /mcp with switches the tests
// flip: reject strips the Authorization header (so mcpkit answers 401),
// failNext answers the next requests with 503, and requests counts what
// reached it.
type testServer struct {
	*httptest.Server
	reject   atomic.Bool
	failNext atomic.Int32
	requests atomic.Int32
	paths    sync.Map // path -> true
	// failSession answers 503 to every request on that Mcp-Session-Id.
	failSession atomic.Value // string
	// initializes counts initialize requests: one per session opened.
	initializes atomic.Int32
}

func startServer(t *testing.T, s *mcp.Server) *testServer {
	t.Helper()
	ts := &testServer{}
	h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, testToken)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requests.Add(1)
		ts.paths.Store(r.URL.Path, true)
		if r.Method == http.MethodPost && r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			if bytes.Contains(b, []byte(`"method":"initialize"`)) {
				ts.initializes.Add(1)
			}
		}
		if id, _ := ts.failSession.Load().(string); id != "" && r.Header.Get("Mcp-Session-Id") == id {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if ts.failNext.Load() > 0 {
			ts.failNext.Add(-1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if ts.reject.Load() {
			r.Header.Del("Authorization")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// glPage is the in-test stand-in for list_gl_entries: three pages.
func glPageHandler(_ context.Context, _ *mcp.CallToolRequest, in rangeArgs) (*mcp.CallToolResult, wireGLEntries, error) {
	mk := func(name string, debit int64) wireGLEntry {
		return wireGLEntry{Name: name, Account: "HDFC Current 0001 - STPL", Debit: money.Paise(debit), PostingDate: "2026-09-10",
			VoucherType: "Journal Entry", VoucherNo: "JV-" + name, Remarks: resultMarker}
	}
	switch in.Cursor {
	case "":
		return nil, wireGLEntries{Entries: []wireGLEntry{mk("GLE-1", 100), mk("GLE-2", 200)}, NextCursor: "c1"}, nil
	case "c1":
		return nil, wireGLEntries{Entries: []wireGLEntry{mk("GLE-3", 300)}, NextCursor: "c2"}, nil
	case "c2":
		return nil, wireGLEntries{Entries: []wireGLEntry{mk("GLE-4", 900719925474099)}}, nil
	}
	return nil, wireGLEntries{}, errors.New("bad cursor")
}

type emptyIn struct{}

type emptyOut struct {
	OK bool `json:"ok"`
}

// fakeBooks mimics the books server's tool names. Only list_gl_entries
// returns data; the rest are stubs for discovery.
func fakeBooks(extra ...func(*mcp.Server)) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-books", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_gl_entries", Description: "GL entries", Annotations: ro()}, glPageHandler)
	for _, n := range []string{"get_trial_balance", "list_purchase_invoices", "list_sales_invoices", "list_payments", "get_account_history", "list_recurring_suppliers"} {
		mcp.AddTool(s, &mcp.Tool{Name: n, Description: n, Annotations: ro()}, func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, emptyOut, error) {
			return nil, emptyOut{OK: true}, nil
		})
	}
	for _, f := range extra {
		f(s)
	}
	return s
}

// fakeEvidence mimics the evidence server's tool names.
func fakeEvidence(extra ...func(*mcp.Server)) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-evidence", Version: "test"}, nil)
	ref := "UTR123456"
	mcp.AddTool(s, &mcp.Tool{Name: "list_bank_lines", Description: "bank lines", Annotations: ro()},
		func(_ context.Context, _ *mcp.CallToolRequest, in rangeArgs) (*mcp.CallToolResult, []wireBankLine, error) {
			return nil, []wireBankLine{
				{TxnID: "T1", Date: "2026-09-03", Narration: "NEFT " + resultMarker, Ref: &ref, Amount: -5900000},
				{TxnID: "T2", Date: "2026-09-30", Narration: "SMS CHGS", Amount: -1770},
			}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_gstr2b_entries", Description: "gstr2b", Annotations: ro()},
		func(_ context.Context, _ *mcp.CallToolRequest, in periodArgs) (*mcp.CallToolResult, []wireGSTR2BEntry, error) {
			return nil, nil, nil
		})
	for _, f := range extra {
		f(s)
	}
	return s
}

type env struct {
	books, evidence *testServer
	cfg             config.Config
	logs            *bytes.Buffer
	log             *slog.Logger
}

func newEnv(t *testing.T, books, evidence *mcp.Server) *env {
	t.Helper()
	e := &env{books: startServer(t, books), evidence: startServer(t, evidence), logs: &bytes.Buffer{}}
	e.log = slog.New(slog.NewJSONHandler(&lockedWriter{w: e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.cfg = config.Config{
		BooksMCPURL:    e.books.URL + "/mcp",
		EvidenceMCPURL: e.evidence.URL + "/mcp",
		MCPTokenAgent:  config.NewSecret(testToken),
	}
	return e
}

func (e *env) registry(t *testing.T, opts ...Option) *Registry {
	t.Helper()
	r, err := NewRegistry(t.Context(), e.cfg, e.log, opts...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// lockedWriter serialises log writes from concurrent goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) logText() string {
	// The buffer is only read after the writes the test waits for.
	return e.logs.String()
}

func call(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: "call-1", Name: name, Args: json.RawMessage(args)}
}

func TestRegistrySpecsPrefixedAndSorted(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r := e.registry(t)

	specs := r.Specs()
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	want := []string{
		"books__get_account_history", "books__get_trial_balance", "books__list_gl_entries",
		"books__list_payments", "books__list_purchase_invoices", "books__list_recurring_suppliers",
		"books__list_sales_invoices", "evidence__list_bank_lines", "evidence__list_gstr2b_entries",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("Specs names = %v\nwant %v", names, want)
	}

	// The input schema is the server's, passed through.
	cs := directSession(t, e.books.URL+"/mcp")
	var serverSchema any
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "list_gl_entries" {
			serverSchema = tool.InputSchema
		}
	}
	var got any
	idx := slices.IndexFunc(specs, func(s llm.ToolSpec) bool { return s.Name == "books__list_gl_entries" })
	if err := json.Unmarshal(specs[idx].InputSchema, &got); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(serverSchema)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("input schema = %s\nwant %s", gotJSON, wantJSON)
	}
	if specs[idx].Description != "GL entries" {
		t.Errorf("description = %q", specs[idx].Description)
	}

	// Specs returns a copy.
	specs[0].Name = "changed"
	if r.Specs()[0].Name == "changed" {
		t.Error("Specs shares its backing array")
	}
}

// directSession connects straight to an in-test server with the token.
func directSession(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, token: testToken,
			allowed: map[string]bool{endpointKey(mustURL(t, endpoint)): true}}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestRegistryStartupLog(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r := e.registry(t)
	_ = r.Execute(t.Context(), call("books__list_gl_entries", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))
	_ = r.Execute(t.Context(), call("evidence__list_bank_lines", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))

	text := e.logText()
	var ready []map[string]any
	for line := range strings.Lines(text) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if m["msg"] == "agent tool registry ready" {
			ready = append(ready, m)
		}
	}
	if len(ready) != 1 {
		t.Fatalf("startup log lines = %d, want exactly 1\n%s", len(ready), text)
	}
	tools, _ := ready[0]["tools"].([]any)
	var names []string
	for _, n := range tools {
		names = append(names, n.(string))
	}
	want := make([]string, 0)
	for _, s := range r.Specs() {
		want = append(want, s.Name)
	}
	if !slices.Equal(names, want) {
		t.Errorf("logged tools = %v, want %v", names, want)
	}
	for _, n := range names {
		if strings.Contains(n, "admin") {
			t.Errorf("admin tool %q in the tool list", n)
		}
	}
	if strings.Contains(text, testToken) {
		t.Error("a log line holds the agent token")
	}
	if strings.Contains(text, resultMarker) {
		t.Error("a log line holds tool result content")
	}
}

func TestRegistryRefusesNonReadOnlyTool(t *testing.T) {
	for _, ann := range []*mcp.ToolAnnotations{nil, {ReadOnlyHint: false}} {
		books := fakeBooks(func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "post_journal_entry", Annotations: ann},
				func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, emptyOut, error) {
					return nil, emptyOut{}, nil
				})
		})
		e := newEnv(t, books, fakeEvidence())
		r, err := NewRegistry(t.Context(), e.cfg, e.log)
		if err == nil {
			r.Close()
			t.Fatal("NewRegistry accepted a tool without readOnlyHint: true")
		}
		if !strings.Contains(err.Error(), "post_journal_entry") || !strings.Contains(err.Error(), "readOnlyHint") {
			t.Errorf("error = %v, want it to name the tool and readOnlyHint", err)
		}
	}
}

func TestRegistryRefusesNonMCPPath(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	for _, tc := range []struct {
		name     string
		books    string
		evidence string
	}{
		{"books admin", e.books.URL + "/mcp-admin", e.evidence.URL + "/mcp"},
		{"evidence admin", e.books.URL + "/mcp", e.evidence.URL + "/mcp-admin"},
		{"trailing slash", e.books.URL + "/mcp/", e.evidence.URL + "/mcp"},
		{"upper case", e.books.URL + "/MCP", e.evidence.URL + "/mcp"},
		{"no path", e.books.URL, e.evidence.URL + "/mcp"},
		{"escaped", e.books.URL + "/mc%70", e.evidence.URL + "/mcp"},
		{"query", e.books.URL + "/mcp?x=1", e.evidence.URL + "/mcp"},
		{"empty query", e.books.URL + "/mcp?", e.evidence.URL + "/mcp"},
		{"credentials", strings.Replace(e.books.URL, "http://", "http://u:p@", 1) + "/mcp", e.evidence.URL + "/mcp"},
		{"scheme", strings.Replace(e.books.URL, "http://", "ftp://", 1) + "/mcp", e.evidence.URL + "/mcp"},
		{"unset", "", e.evidence.URL + "/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := e.cfg
			cfg.BooksMCPURL, cfg.EvidenceMCPURL = tc.books, tc.evidence
			r, err := NewRegistry(t.Context(), cfg, e.log)
			if err == nil {
				r.Close()
				t.Fatal("NewRegistry accepted a URL whose path is not exactly /mcp")
			}
		})
	}
	if n := e.books.requests.Load() + e.evidence.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the servers; a refused URL must not be dialled", n)
	}
	if _, ok := e.books.paths.Load("/mcp-admin"); ok {
		t.Error("the registry connected to /mcp-admin")
	}
}

func TestRegistryRefusesEmptyToken(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	cfg := e.cfg
	cfg.MCPTokenAgent = config.Secret{}
	if r, err := NewRegistry(t.Context(), cfg, e.log); err == nil {
		r.Close()
		t.Fatal("NewRegistry started without an agent token")
	}
}

func TestRegistryStartup401(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	cfg := e.cfg
	cfg.MCPTokenAgent = config.NewSecret("wrong-token-91ab")
	r, err := NewRegistry(t.Context(), cfg, e.log)
	if err == nil {
		r.Close()
		t.Fatal("NewRegistry started with a token the server refuses")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "wrong-token-91ab") || strings.Contains(err.Error(), e.books.URL) {
		t.Errorf("error leaks the token or URL: %v", err)
	}
}

func TestRegistryExecute(t *testing.T) {
	failing := func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "always_fails", Annotations: ro()},
			func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, emptyOut, error) {
				return nil, emptyOut{}, errors.New("internal detail http://10.0.0.1/secret")
			})
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "slow", Annotations: ro()},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, emptyOut, error) {
				select {
				case <-ctx.Done():
				case <-release:
				}
				return nil, emptyOut{}, nil
			})
	}
	e := newEnv(t, fakeBooks(failing, slow), fakeEvidence())
	r := e.registry(t, WithCallTimeout(300*time.Millisecond))

	t.Run("success", func(t *testing.T) {
		res := r.Execute(t.Context(), call("evidence__list_bank_lines", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))
		if res.IsError || res.ToolCallID != "call-1" {
			t.Fatalf("result = %+v", res)
		}
		var lines []wireBankLine
		if err := json.Unmarshal([]byte(res.Content), &lines); err != nil || len(lines) != 2 {
			t.Fatalf("content %q: %v", res.Content, err)
		}
	})

	errCases := []struct {
		name string
		call llm.ToolCall
		want string
	}{
		{"unknown tool", call("books__post_journal_entry", `{}`), MsgUnknownTool},
		{"unprefixed name", call("list_gl_entries", `{}`), MsgUnknownTool},
		{"other server prefix", call("evidence__list_gl_entries", `{}`), MsgUnknownTool},
		{"empty name", call("", `{}`), MsgUnknownTool},
		{"args not an object", call("books__list_gl_entries", `[1,2]`), MsgBadArgs},
		{"args not JSON", call("books__list_gl_entries", `{`), MsgBadArgs},
		{"tool error", call("books__always_fails", `{}`), MsgToolError},
		{"schema violation", call("books__list_gl_entries", `{"company":1}`), MsgToolError},
		{"timeout", call("books__slow", `{}`), MsgTimeout},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Execute(t.Context(), tc.call)
			if !res.IsError || res.Content != tc.want {
				t.Errorf("result = %+v, want IsError with %q", res, tc.want)
			}
			if strings.Contains(res.Content, "10.0.0.1") || strings.Contains(res.Content, "http") {
				t.Errorf("content leaks detail: %q", res.Content)
			}
		})
	}

	t.Run("still works after errors", func(t *testing.T) {
		res := r.Execute(t.Context(), call("books__list_gl_entries", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))
		if res.IsError {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestRegistry401AfterStartIsAnErrorResult(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r := e.registry(t)
	e.books.reject.Store(true)

	res := r.Execute(t.Context(), call("books__list_gl_entries", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))
	if !res.IsError || res.Content != MsgUnavailable {
		t.Fatalf("result = %+v, want IsError %q", res, MsgUnavailable)
	}
	if strings.Contains(res.Content, testToken) || strings.Contains(res.Content, "Unauthorized") {
		t.Errorf("content leaks detail: %q", res.Content)
	}
	if strings.Contains(e.logText(), testToken) {
		t.Error("a log line holds the agent token")
	}

	// The evidence session is unaffected.
	if res := r.Execute(t.Context(), call("evidence__list_bank_lines", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`)); res.IsError {
		t.Errorf("evidence call = %+v", res)
	}
}

func TestRegistryReconnectsOnceOnTransportFailure(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r := e.registry(t)
	args := `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`

	// One failed request: the reconnect and retry succeed.
	e.books.failNext.Store(1)
	if res := r.Execute(t.Context(), call("books__list_gl_entries", args)); res.IsError {
		t.Fatalf("after one transport failure: %+v, want success on the reconnect", res)
	}

	// Failures beyond the one reconnect come back as an error result.
	e.books.failNext.Store(100)
	before := e.books.requests.Load()
	res := r.Execute(t.Context(), call("books__list_gl_entries", args))
	if !res.IsError || res.Content != MsgUnavailable {
		t.Fatalf("result = %+v, want IsError %q", res, MsgUnavailable)
	}
	if n := e.books.requests.Load() - before; n > 4 {
		t.Errorf("%d requests for one call; want one reconnect at most", n)
	}
	e.books.failNext.Store(0)

	// The next call reconnects again.
	if res := r.Execute(t.Context(), call("books__list_gl_entries", args)); res.IsError {
		t.Fatalf("after the server recovers: %+v", res)
	}
}

func TestRegistryClose(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r, err := NewRegistry(t.Context(), e.cfg, e.log)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r.Close() // twice is fine
	res := r.Execute(t.Context(), call("books__list_gl_entries", `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`))
	if !res.IsError || res.Content != MsgUnavailable {
		t.Errorf("after Close: %+v", res)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestRegistryNeverFollowsRedirects: a 307 from /mcp to /mcp-admin on the
// same host, or to another allowlisted host, fails the start; /mcp-admin
// is never reached and the other host never sees the token.
func TestRegistryNeverFollowsRedirects(t *testing.T) {
	var adminHits, otherAuth, otherHits atomic.Int32
	admin := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp-admin": mcpkit.NewRoute(fakeBooks(), testToken)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			otherAuth.Add(1)
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)

	for _, target := range []string{"/mcp-admin", other.URL + "/mcp"} {
		t.Run(target, func(t *testing.T) {
			books := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mcp-admin" {
					adminHits.Add(1)
					admin.ServeHTTP(w, r)
					return
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			}))
			t.Cleanup(books.Close)
			e := newEnv(t, fakeBooks(), fakeEvidence())
			cfg := e.cfg
			cfg.BooksMCPURL = books.URL + "/mcp"
			cfg.ERPBaseURL = other.URL // allowlisted by httpx
			r, err := NewRegistry(t.Context(), cfg, e.log)
			if err == nil {
				r.Close()
				t.Fatal("NewRegistry followed a redirect")
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
	if n := adminHits.Load(); n != 0 {
		t.Errorf("/mcp-admin was hit %d times", n)
	}
	if n := otherAuth.Load(); n != 0 {
		t.Errorf("the other host saw the agent token %d times (hits %d)", n, otherHits.Load())
	}
}

// TestBearerTransportOnlyOnEndpoints: the token goes only to the
// configured /mcp endpoints; anything else is refused without reaching
// the network.
func TestBearerTransportOnlyOnEndpoints(t *testing.T) {
	var seen []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.String()+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	bt := &bearerTransport{base: base, token: testToken,
		allowed: map[string]bool{endpointKey(mustURL(t, "http://Books.local:8081/mcp")): true}}
	for _, raw := range []string{
		"http://books.local:8081/mcp-admin",
		"http://books.local:8081/mcp/",
		"http://books.local:8081/mcp?x=1",
		"http://books.local:8082/mcp",
		"https://books.local:8081/mcp",
		"http://erp.local:8081/mcp",
		"http://books.local:8081/MCP",
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, raw, strings.NewReader("{}"))
		resp, err := bt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, errOffEndpoint) {
			t.Errorf("%s: err = %v, want refusal", raw, err)
		}
	}
	if len(seen) != 0 {
		t.Errorf("refused requests reached the transport: %v", seen)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://BOOKS.local:8081/mcp", strings.NewReader("{}"))
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "Bearer "+testToken) {
		t.Errorf("allowed request = %v", seen)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("RoundTrip modified the caller's request")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRegistryConcurrentFailuresReconnectOnce: two calls failing on the
// same session open one new session between them, and both succeed.
func TestRegistryConcurrentFailuresReconnectOnce(t *testing.T) {
	e := newEnv(t, fakeBooks(), fakeEvidence())
	r := e.registry(t)
	args := `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`
	old := r.servers[ServerBooks].current()
	e.books.failSession.Store(old.ID())
	before := e.books.initializes.Load()

	var wg sync.WaitGroup
	results := make([]llm.ToolResultBlock, 4)
	for i := range results {
		wg.Go(func() { results[i] = r.Execute(t.Context(), call("books__list_gl_entries", args)) })
	}
	wg.Wait()
	for i, res := range results {
		if res.IsError {
			t.Errorf("call %d = %+v", i, res)
		}
	}
	if n := e.books.initializes.Load() - before; n != 1 {
		t.Errorf("%d sessions opened for concurrent failures, want 1", n)
	}
	if cur := r.servers[ServerBooks].current(); cur == old {
		t.Error("the failed session is still current")
	}
}

func TestRegistryDiscoverBounds(t *testing.T) {
	t.Run("hanging server", func(t *testing.T) {
		release := make(chan struct{})
		hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(hang.Close)
		t.Cleanup(func() { close(release) })
		e := newEnv(t, fakeBooks(), fakeEvidence())
		cfg := e.cfg
		cfg.BooksMCPURL = hang.URL + "/mcp"
		start := time.Now()
		r, err := NewRegistry(t.Context(), cfg, e.log, WithDiscoverTimeout(300*time.Millisecond))
		if err == nil {
			r.Close()
			t.Fatal("NewRegistry started against a hanging server")
		}
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("err = %v, want ErrTimeout", err)
		}
		// The discover deadline plus the SDK's own 5 s bound on sending
		// the cancellation to a server that never answers.
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("start took %v", d)
		}
	})
	t.Run("too many tools", func(t *testing.T) {
		books := fakeBooks(func(s *mcp.Server) {
			for i := range MaxToolsPerServer {
				mcp.AddTool(s, &mcp.Tool{Name: fmt.Sprintf("extra_%02d", i), Annotations: ro()},
					func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, emptyOut, error) {
						return nil, emptyOut{}, nil
					})
			}
		})
		e := newEnv(t, books, fakeEvidence())
		r, err := NewRegistry(t.Context(), e.cfg, e.log)
		if err == nil {
			r.Close()
			t.Fatal("NewRegistry accepted more than MaxToolsPerServer tools")
		}
		if !strings.Contains(err.Error(), "more than") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("hostile tool name is quoted and cut", func(t *testing.T) {
		long := "x_" + strings.Repeat("IGNOREPREVIOUS", 10)
		books := fakeBooks(func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: long},
				func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, emptyOut, error) {
					return nil, emptyOut{}, nil
				})
		})
		e := newEnv(t, books, fakeEvidence())
		r, err := NewRegistry(t.Context(), e.cfg, e.log)
		if err == nil {
			r.Close()
			t.Fatal("accepted")
		}
		if strings.Contains(err.Error(), long) {
			t.Errorf("error carries the whole server-supplied name: %v", err)
		}
	})
}
