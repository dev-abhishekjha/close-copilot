package mcpkit_test

import (
	"context"
	"encoding/json"
	"errors"
	goast "go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
)

type fakeSink struct {
	mu   sync.Mutex
	rows []store.AuditLogEntry
	err  error
}

func (f *fakeSink) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.rows = append(f.rows, e)
	return int64(len(f.rows)), nil
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

type auditIn struct {
	Company string `json:"company"`
	Actor   string `json:"actor,omitempty"`
	Token   string `json:"api_key,omitempty"`
	Nested  *struct {
		Password string `json:"password,omitempty"`
		Keep     string `json:"keep,omitempty"`
	} `json:"nested,omitempty"`
	Fail bool `json:"fail,omitempty"`
}

type auditOut struct {
	OK bool `json:"ok"`
}

// startAudited serves one books-style tool (struct output), one evidence-style
// tool (slice output) and a panicking tool behind a real HTTP handler, and
// returns a connected client session.
func startAudited(t *testing.T, sink mcpkit.AuditSink) *mcp.ClientSession {
	t.Helper()
	srv := mcpkit.NewServer("test", "1.0.0")
	mcpkit.EnableAudit(srv, "books", sink)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcpkit.AddTool(srv, &mcp.Tool{Name: "struct_tool", Description: "d", Annotations: ro},
		func(_ context.Context, _ *mcp.CallToolRequest, in auditIn) (*mcp.CallToolResult, auditOut, error) {
			if in.Fail {
				return nil, auditOut{}, errors.New("boom")
			}
			return nil, auditOut{OK: true}, nil
		})
	mcpkit.AddTool(srv, &mcp.Tool{Name: "slice_tool", Description: "d", Annotations: ro},
		func(_ context.Context, _ *mcp.CallToolRequest, _ auditIn) (*mcp.CallToolResult, []auditOut, error) {
			return nil, []auditOut{{OK: true}, {OK: true}, {OK: true}}, nil
		})
	mcpkit.AddTool(srv, &mcp.Tool{Name: "panic_tool", Description: "d", Annotations: ro},
		func(_ context.Context, _ *mcp.CallToolRequest, _ auditIn) (*mcp.CallToolResult, auditOut, error) {
			panic("kaboom")
		})

	h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(srv, "tok")}, testLogger(),
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: "tok"}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	t.Logf("%s: isError=%v content=%+v", name, res.IsError, res.Content[0].(*mcp.TextContent).Text)
	return res
}

func TestAudit_OneRowPerCall(t *testing.T) {
	sink := &fakeSink{}
	sess := startAudited(t, sink)
	call(t, sess, "struct_tool", map[string]any{"company": "sharma"})
	call(t, sess, "slice_tool", map[string]any{"company": "sharma"})

	if len(sink.rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(sink.rows))
	}
	r0, r1 := sink.rows[0], sink.rows[1]
	if r0.Actor != "agent" || *r0.Server != "books" || *r0.Tool != "struct_tool" || *r0.ResultCount != 1 || r0.Error != nil {
		t.Errorf("struct row: %+v", r0)
	}
	if *r1.Tool != "slice_tool" || *r1.ResultCount != 3 {
		t.Errorf("slice row: tool=%s count=%d", *r1.Tool, *r1.ResultCount)
	}
	if r0.LatencyMS == nil {
		t.Error("latency_ms missing")
	}
}

func TestAudit_ActorIgnoresArguments(t *testing.T) {
	sink := &fakeSink{}
	sess := startAudited(t, sink)
	call(t, sess, "struct_tool", map[string]any{"company": "sharma", "actor": "admin"})
	if len(sink.rows) != 1 || sink.rows[0].Actor != "agent" {
		t.Fatalf("actor: %+v", sink.rows)
	}
}

// TestScopedActor: on a scoped route the audit actor is agent:run/<run_id>
// from the verified scope (which must override the route's actor), and an
// actor argument is ignored.
func TestScopedActor(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScoper(t)
	srv := mcpkit.NewServer("test", "1.0.0")
	mcpkit.EnableAudit(srv, "books", sink)
	mcpkit.AddTool(srv, &mcp.Tool{Name: "struct_tool", Description: "d", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, _ auditIn) (*mcp.CallToolResult, auditOut, error) {
			return nil, auditOut{OK: true}, nil
		})
	h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(srv, "tok")}, testLogger(),
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}), mcpkit.WithScoper(s, "/mcp"))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	tr := &scopeTransport{token: "tok"}
	tr.set(liveScope(t)) // run-1
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tr}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	call(t, sess, "struct_tool", map[string]any{"company": "sharma", "actor": "admin"})

	// The same session, the next request under another run's scope: the
	// actor follows the request's scope, not the session's first one.
	other := goodScope()
	other.RunID, other.Exp = "run-2", time.Now().Add(time.Hour).Unix()
	tr.set(mustMint(t, scopeKey, other))
	call(t, sess, "struct_tool", map[string]any{"company": "sharma"})

	if len(sink.rows) != 2 {
		t.Fatalf("rows: %d, want 2", len(sink.rows))
	}
	if a := sink.rows[0].Actor; a != "agent:run/run-1" {
		t.Errorf("actor %q, want agent:run/run-1", a)
	}
	if a := sink.rows[1].Actor; a != "agent:run/run-2" {
		t.Errorf("actor %q, want agent:run/run-2", a)
	}
}

func TestAudit_RedactsSecrets(t *testing.T) {
	sink := &fakeSink{}
	sess := startAudited(t, sink)
	call(t, sess, "struct_tool", map[string]any{
		"company": "sharma", "api_key": "sk-123",
		"nested": map[string]any{"password": "hunter2", "keep": "visible"},
	})
	args := string(sink.rows[0].Args)
	if strings.Contains(args, "sk-123") || strings.Contains(args, "hunter2") {
		t.Errorf("secret leaked: %s", args)
	}
	var m map[string]any
	if err := json.Unmarshal(sink.rows[0].Args, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_key"] != "[REDACTED]" || m["nested"].(map[string]any)["keep"] != "visible" {
		t.Errorf("args: %s", args)
	}
}

func TestAudit_ErrorsAndPanicsAudited(t *testing.T) {
	sink := &fakeSink{}
	sess := startAudited(t, sink)
	res := call(t, sess, "struct_tool", map[string]any{"company": "x", "fail": true})
	if !res.IsError {
		t.Error("want tool error")
	}
	call(t, sess, "panic_tool", map[string]any{"company": "x"})
	if len(sink.rows) != 2 {
		t.Fatalf("rows: %d", len(sink.rows))
	}
	for _, r := range sink.rows {
		if r.Error == nil || *r.Error == "" || *r.ResultCount != 0 {
			t.Errorf("error row: %+v", r)
		}
	}
}

func TestAudit_FailsClosedOnSinkError(t *testing.T) {
	sink := &fakeSink{err: errors.New("db down")}
	sess := startAudited(t, sink)
	res := call(t, sess, "struct_tool", map[string]any{"company": "x"})
	if !res.IsError {
		t.Error("want tool error when the audit insert fails")
	}
}

func TestAudit_TruncatesLargeArgs(t *testing.T) {
	sink := &fakeSink{}
	sess := startAudited(t, sink)
	call(t, sess, "struct_tool", map[string]any{"company": strings.Repeat("a", 20000)})
	if n := len(sink.rows[0].Args); n > 8192 || !json.Valid(sink.rows[0].Args) {
		t.Errorf("args len %d valid=%v", n, json.Valid(sink.rows[0].Args))
	}
}

func TestAudit_NoSinkPassesThrough(t *testing.T) {
	srv := mcpkit.NewServer("t", "1")
	mcpkit.AddTool(srv, &mcp.Tool{Name: "x", Description: "d"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ auditIn) (*mcp.CallToolResult, auditOut, error) {
			return nil, auditOut{OK: true}, nil
		})
}

// TestNoDirectMCPAddTool fails on any mcp.AddTool call outside audit.go.
func TestNoDirectMCPAddTool(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			if filepath.Base(p) == "audit.go" && filepath.Base(filepath.Dir(p)) == "mcpkit" {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, src, 0)
			if err != nil {
				return err
			}
			mcpName := ""
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "github.com/modelcontextprotocol/go-sdk/mcp" {
					mcpName = "mcp"
					if imp.Name != nil {
						mcpName = imp.Name.Name
					}
				}
			}
			if mcpName == "" {
				return nil
			}
			inspectAddTool(f, mcpName, func() { bad = append(bad, p) })
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Errorf("direct mcp.AddTool calls (use mcpkit.AddTool): %v", bad)
	}
}

func inspectAddTool(f *goast.File, pkg string, hit func()) {
	goast.Inspect(f, func(n goast.Node) bool {
		call, ok := n.(*goast.CallExpr)
		if !ok {
			return true
		}
		var sel *goast.SelectorExpr
		switch fn := call.Fun.(type) {
		case *goast.SelectorExpr:
			sel = fn
		case *goast.IndexExpr:
			sel, _ = fn.X.(*goast.SelectorExpr)
		case *goast.IndexListExpr:
			sel, _ = fn.X.(*goast.SelectorExpr)
		}
		if sel != nil && sel.Sel.Name == "AddTool" {
			if id, ok := sel.X.(*goast.Ident); ok && id.Name == pkg {
				hit()
			}
		}
		return true
	})
}
