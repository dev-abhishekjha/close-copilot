package evidence_test

// Run-scope tests for the evidence tools (CC-506), over the whole HTTP
// stack with WithScoper. The store is nil: a call that got past the scope
// check and reached Postgres would panic, so a clean out_of_scope refusal
// proves no Postgres read. Audit is on with an in-memory sink, which turns
// such a panic into a tool error the test can see instead of a crash.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const scopeTestKey = "evidence-scope-test-key"

// scopeCompany's 2026-09 run may read 2025-09-01..2026-10-31.
const scopeCompany = "sharma"

type memSink struct {
	mu   sync.Mutex
	rows []store.AuditLogEntry
}

func (m *memSink) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, e)
	return int64(len(m.rows)), nil
}

func (m *memSink) panicked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Error != nil && strings.Contains(*r.Error, "panicked") {
			return true
		}
	}
	return false
}

type scopeTransport struct {
	base         http.RoundTripper
	token, scope string
}

func (s *scopeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+s.token)
	clone.Header.Set(mcpkit.ScopeHeader, s.scope)
	return s.base.RoundTrip(clone)
}

func scopedEvidence(t *testing.T) (*httptest.Server, *memSink) {
	t.Helper()
	sc, err := mcpkit.NewScoper(scopeTestKey)
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-evidence", Version: "scope"}, nil)
	mcpkit.EnableAudit(s, "evidence", sink)
	evidence.RegisterTools(s, nil, evidence.WithScope(sc))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, contractToken)}, log,
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}), mcpkit.WithScoper(sc, "/mcp")))
	t.Cleanup(srv.Close)
	return srv, sink
}

func mintScope(t *testing.T, sc mcpkit.Scope) string {
	t.Helper()
	h, err := mcpkit.Mint([]byte(scopeTestKey), sc)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func runScope(t *testing.T) string {
	t.Helper()
	sc, err := mcpkit.Window(scopeCompany, "run-scope-test", "2026-09", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return mintScope(t, sc)
}

func scopedSession(t *testing.T, srv *httptest.Server, header string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "scope-test", Version: "v0"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{
			Endpoint:   srv.URL + "/mcp",
			HTTPClient: &http.Client{Transport: &scopeTransport{base: srv.Client().Transport, token: contractToken, scope: header}},
			MaxRetries: -1,
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return cs
}

func toolResult(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (bool, string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport or protocol error %v, want a tool result", tool, err)
	}
	return res.IsError, resultText(res)
}

func wantOutOfScope(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) {
	t.Helper()
	isErr, msg := toolResult(t, cs, tool, args)
	if !isErr || !strings.HasPrefix(msg, "out_of_scope") {
		t.Fatalf("%s(%v): IsError=%v %q, want an out_of_scope tool error", tool, args, isErr, msg)
	}
	if strings.Contains(msg, "Mehta") || strings.Contains(msg, "panicked") {
		t.Errorf("%s: refusal %q leaks another company or reached the store", tool, msg)
	}
}

type evidenceCase struct {
	name string
	args map[string]any
}

func bank(kv ...string) map[string]any {
	m := map[string]any{"company": scopeCompany, "from_date": "2026-09-01", "to_date": "2026-09-30"}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func gstr2b(kv ...string) map[string]any {
	m := map[string]any{"company": scopeCompany, "period": "2026-09"}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

var evidenceScopeCases = map[string]struct {
	refused []evidenceCase
	ok      []map[string]any
}{
	"list_bank_lines": {
		refused: []evidenceCase{
			{"unconfigured company", bank("company", "nobody")},
			{"another company", bank("company", "mehta")},
			{"from a day before the window", bank("from_date", "2025-08-31")},
			{"to a day after the window", bank("from_date", "2026-10-01", "to_date", "2026-11-01")},
		},
		ok: []map[string]any{bank(), bank("from_date", "2025-09-01", "to_date", "2026-10-31")},
	},
	"list_gstr2b_entries": {
		refused: []evidenceCase{
			{"unconfigured company", gstr2b("company", "nobody")},
			{"another company", gstr2b("company", "mehta")},
			{"period before the window", gstr2b("period", "2025-08")},
			{"period after the window", gstr2b("period", "2026-11")},
		},
		ok: []map[string]any{gstr2b(), gstr2b("period", "2025-09"), gstr2b("period", "2026-10")},
	},
}

func TestScopeEveryTool(t *testing.T) {
	for _, tool := range evidenceTools {
		c := evidenceScopeCases[tool]
		t.Run(tool, func(t *testing.T) {
			for _, r := range c.refused {
				t.Run(r.name, func(t *testing.T) {
					srv, sink := scopedEvidence(t)
					wantOutOfScope(t, scopedSession(t, srv, runScope(t)), tool, r.args)
					if sink.panicked() {
						t.Error("a refused call reached the store")
					}
				})
			}
			// Inside the window the call passes the scope check and reaches
			// the (nil) store, which the audit wrapper reports as a panic.
			t.Run("own company and window", func(t *testing.T) {
				srv, sink := scopedEvidence(t)
				cs := scopedSession(t, srv, runScope(t))
				for _, args := range c.ok {
					if _, msg := toolResult(t, cs, tool, args); strings.HasPrefix(msg, "out_of_scope") {
						t.Errorf("in-scope call %v refused: %s", args, msg)
					}
				}
				if !sink.panicked() {
					t.Error("in-scope calls never reached the store")
				}
			})
		})
	}

	t.Run("discovery", func(t *testing.T) {
		srv, sink := scopedEvidence(t)
		cs := scopedSession(t, srv, mintScope(t, mcpkit.DiscoveryScope(time.Now())))
		for _, tool := range evidenceTools {
			for _, args := range evidenceScopeCases[tool].ok {
				wantOutOfScope(t, cs, tool, args)
			}
		}
		if sink.panicked() {
			t.Error("a call under the discovery scope reached the store")
		}
	})

	t.Run("no scope is 401", func(t *testing.T) {
		srv, _ := scopedEvidence(t)
		if cs, err := contractConnect(t, srv, contractToken); err == nil {
			_ = cs.Close()
			t.Error("connected without a run scope")
		}
	})
}
