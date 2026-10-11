package books

// Run-scope tests for the books tools (CC-506), over the whole HTTP stack:
// mcpkit.BuildHandler with bearer auth and WithScoper, the SDK's Streamable
// HTTP client, and a fake ERPNext that counts requests. Each refusal is an
// attempt by a steered model, or an injected argument, to read another
// company or a date outside the run's window; it must come back as an
// out_of_scope tool error before any ERPNext request.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

// scopeTestKey is a made-up MCP_SCOPE_KEY.
const scopeTestKey = "books-scope-test-key"

// scopeTransport adds the bearer token and a fixed X-Run-Scope.
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

// scopedBooks serves the books tools over f at /mcp, scoped as the binary
// serves them.
func scopedBooks(t *testing.T, f *fakeERP) *httptest.Server {
	t.Helper()
	sc, err := mcpkit.NewScoper(scopeTestKey)
	if err != nil {
		t.Fatal(err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-books", Version: "scope"}, nil)
	RegisterTools(s, ToolDeps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client: f.client(),
		Companies: ProfileCompanies([]seed.Profile{
			{ID: toolCompanyID, ERPCompany: testCompany},
			{ID: "other", ERPCompany: otherCompany},
		}),
		Now:   func() time.Time { return toolNow },
		Scope: sc,
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, contractToken)}, log,
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}), mcpkit.WithScoper(sc, "/mcp")))
	t.Cleanup(srv.Close)
	return srv
}

// runScope mints the scope of a run of toolCompanyID for month.
func runScope(t *testing.T, month string) string {
	t.Helper()
	sc, err := mcpkit.Window(toolCompanyID, "run-scope-test", month, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return mint(t, sc)
}

func mint(t *testing.T, sc mcpkit.Scope) string {
	t.Helper()
	h, err := mcpkit.Mint([]byte(scopeTestKey), sc)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// scopedSession connects to srv with header as the run scope and lists
// the tools: initialize, tools/list and later tools/call share the scope.
func scopedSession(t *testing.T, srv *httptest.Server, header string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "scope-test", Version: "v0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
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

// scopeCase is one call; the run is toolCompanyID's 2026-09 close, whose
// window is 2025-09-01..2026-10-31.
type scopeCase struct {
	name string
	args map[string]any
}

func rangeRefusals() []scopeCase {
	return []scopeCase{
		{"unconfigured company", with(september(), "company", "nobody")},
		{"configured other company", with(september(), "company", "other")},
		{"from a day before the window", with(september(), "from_date", "2025-08-31")},
		{"to a day after the window", with(september(), "from_date", "2026-10-01", "to_date", "2026-11-01")},
	}
}

func accountHistory(kv ...any) map[string]any {
	return with(map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 2}, kv...)
}

func recurring(kv ...any) map[string]any {
	return with(map[string]any{"company": toolCompanyID, "before_month": "2026-09", "lookback_months": 3, "min_occurrences": 1}, kv...)
}

// refusals lists, per tool, the calls the 2026-09 scope must refuse, and
// the call inside it that must succeed.
func refusals() map[string]struct {
	refused []scopeCase
	ok      map[string]any
} {
	type entry = struct {
		refused []scopeCase
		ok      map[string]any
	}
	out := map[string]entry{}
	for _, tool := range []string{ToolGetTrialBalance, ToolListGLEntries, ToolListPurchaseInvoices, ToolListSalesInvoices, ToolListPayments} {
		out[tool] = entry{rangeRefusals(), september()}
	}
	out[ToolGetAccountHistory] = entry{[]scopeCase{
		{"unconfigured company", accountHistory("company", "nobody")},
		{"configured other company", accountHistory("company", "other")},
		{"through_month after the window", accountHistory("through_month", "2026-11")},
		{"months reaching before the window", accountHistory("through_month", "2026-07", "months", 12)},
	}, accountHistory("through_month", "2026-10", "months", 12)}
	out[ToolListRecurringSuppliers] = entry{[]scopeCase{
		{"unconfigured company", recurring("company", "nobody")},
		{"configured other company", recurring("company", "other")},
		{"before_month far in the future", recurring("before_month", "2030-01")},
		{"before_month past the window", recurring("before_month", "2026-12", "lookback_months", 1)},
		{"lookback reaching before the window", recurring("before_month", "2026-08", "lookback_months", 12)},
	}, recurring("before_month", "2026-09", "lookback_months", 12)}
	return out
}

// wantOutOfScope calls tool and wants an out_of_scope tool error (not an
// HTTP or protocol error) that names no other company beyond the echo.
func wantOutOfScope(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport or protocol error %v, want a tool error", tool, err)
	}
	msg := resultText(res)
	if !res.IsError || !strings.HasPrefix(msg, "out_of_scope") {
		t.Fatalf("%s(%v): IsError=%v %q, want an out_of_scope tool error", tool, args, res.IsError, msg)
	}
	for _, leak := range []string{otherCompany, testCompany, "unknown company", "http"} {
		if strings.Contains(msg, leak) {
			t.Errorf("%s: refusal %q leaks %q", tool, msg, leak)
		}
	}
}

func TestScopeEveryTool(t *testing.T) {
	cases := refusals()
	for _, tool := range ToolNames {
		c := cases[tool]
		t.Run(tool, func(t *testing.T) {
			for _, r := range c.refused {
				t.Run(r.name, func(t *testing.T) {
					f := contractERP(t)
					cs := scopedSession(t, scopedBooks(t, f), runScope(t, "2026-09"))
					wantOutOfScope(t, cs, tool, r.args)
					if n := len(f.requests()); n != 0 {
						t.Errorf("%d ERPNext requests for a refused call, want 0", n)
					}
				})
			}
			t.Run("own company and window", func(t *testing.T) {
				f := contractERP(t)
				cs := scopedSession(t, scopedBooks(t, f), runScope(t, "2026-09"))
				res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: c.ok})
				if err != nil || res.IsError {
					t.Fatalf("in-scope call refused: %v %s", err, resultText(res))
				}
			})
		})
	}

	t.Run("discovery", func(t *testing.T) {
		f := contractERP(t)
		cs := scopedSession(t, scopedBooks(t, f), mint(t, mcpkit.DiscoveryScope(time.Now())))
		for _, tool := range ToolNames {
			wantOutOfScope(t, cs, tool, cases[tool].ok)
		}
		if n := len(f.requests()); n != 0 {
			t.Errorf("%d ERPNext requests under the discovery scope, want 0", n)
		}
	})

	t.Run("account history defaults to the run's month", func(t *testing.T) {
		f := contractERP(t)
		cs := scopedSession(t, scopedBooks(t, f), runScope(t, "2026-08"))
		var got AccountHistoryOutput
		call(t, cs, ToolGetAccountHistory, accountHistory(), &got)
		if got.ThroughMonth != "2026-08" {
			t.Errorf("through_month %q, want the run's month 2026-08 (the clock says 2026-09)", got.ThroughMonth)
		}
	})

	t.Run("no scope is 401", func(t *testing.T) {
		srv := scopedBooks(t, contractERP(t))
		client := mcp.NewClient(&mcp.Implementation{Name: "scope-test", Version: "v0"}, nil)
		if cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint:   srv.URL + "/mcp",
			HTTPClient: &http.Client{Transport: &bearerTransport{base: srv.Client().Transport, token: contractToken}},
			MaxRetries: -1,
		}, nil); err == nil {
			_ = cs.Close()
			t.Error("connected without a run scope")
		}
	})
}

// TestScopeNoERPNextCall makes every refused call of every tool, and every
// tool under the discovery scope, on one server: the fake ERPNext must see
// no request at all.
func TestScopeNoERPNextCall(t *testing.T) {
	f := contractERP(t)
	srv := scopedBooks(t, f)
	run := scopedSession(t, srv, runScope(t, "2026-09"))
	discovery := scopedSession(t, srv, mint(t, mcpkit.DiscoveryScope(time.Now())))
	calls := 0
	for tool, c := range refusals() {
		for _, r := range c.refused {
			wantOutOfScope(t, run, tool, r.args)
			calls++
		}
		wantOutOfScope(t, discovery, tool, c.ok)
		calls++
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Fatalf("%d refused calls made %d ERPNext requests, want 0: first %s %s", calls, len(reqs), reqs[0].method, reqs[0].doctype)
	}
}
