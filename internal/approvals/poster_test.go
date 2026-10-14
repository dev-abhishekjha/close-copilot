package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
)

const (
	fakeAdminToken = "ADMIN-TOKEN-SENTINEL-7f3c9e"
	fakeAgentToken = "agent-token-for-tests"
)

func TestPinnedToBooks(t *testing.T) {
	if toolPostApproved != books.ToolPostApprovedJournalEntry {
		t.Errorf("toolPostApproved %q, books says %q", toolPostApproved, books.ToolPostApprovedJournalEntry)
	}
	if adminPath != mcpkit.AdminPath {
		t.Errorf("adminPath %q, mcpkit says %q", adminPath, mcpkit.AdminPath)
	}
}

// TestKnownRefusalCodesPinned reads every Code* constant of
// internal/books/admin.go and checks knownRefusalCodes is exactly that set.
func TestKnownRefusalCodesPinned(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(here), "..", "books", "admin.go")
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var want []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Code") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", n.Name)
				}
				v, _ := strconv.Unquote(lit.Value)
				want = append(want, v)
			}
		}
	}
	var got []string
	for c := range knownRefusalCodes {
		got = append(got, c)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("known codes\n got %v\nwant %v", got, want)
	}
	// Spot-check against the exported constants too.
	for _, c := range []string{books.CodeSelfApproved, books.CodeNotApproved, books.CodeUnbalanced, books.CodeInvalidState,
		books.CodeNoChecker, books.CodeNotDecided, books.CodeInvalidDate, books.CodeInvalidLines, books.CodeERPMismatch,
		books.CodeERPConflict, books.CodeNotFound, books.CodeInvalidProposalID, books.CodeUnknownCompany} {
		if !knownRefusalCodes[c] {
			t.Errorf("code %q missing", c)
		}
	}
}

func TestRefusalCode(t *testing.T) {
	cases := map[string]string{
		"self_approved: the checker is the maker":   "self_approved",
		"  not_approved:x":                          "not_approved",
		"erp_unavailable: ERPNext did not answer":   "erp_unavailable",
		"weird_code: something":                     RefusalUnknown,
		"no colon here":                             RefusalUnknown,
		"":                                          RefusalUnknown,
		"Self_Approved: case matters":               RefusalUnknown,
		"tool post_approved_journal_entry panic: x": RefusalUnknown,
	}
	for in, want := range cases {
		if got := refusalCode(in); got != want {
			t.Errorf("refusalCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func posterConfig(booksURL string) config.Config {
	return config.Config{
		BooksMCPURL:   booksURL,
		MCPTokenAdmin: config.NewSecret(fakeAdminToken),
		MCPTokenAgent: config.NewSecret(fakeAgentToken),
	}
}

func TestNewPosterFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"empty token", func(c *config.Config) { c.MCPTokenAdmin = config.Secret{} }},
		{"blank token", func(c *config.Config) { c.MCPTokenAdmin = config.NewSecret("") }},
		{"empty url", func(c *config.Config) { c.BooksMCPURL = "" }},
		{"not http", func(c *config.Config) { c.BooksMCPURL = "ftp://localhost:7001/mcp" }},
		{"no host", func(c *config.Config) { c.BooksMCPURL = "http:///mcp" }},
		{"relative", func(c *config.Config) { c.BooksMCPURL = "/mcp" }},
		{"credentials", func(c *config.Config) { c.BooksMCPURL = "http://u:p@localhost:7001/mcp" }},
		{"token equals agent", func(c *config.Config) { c.MCPTokenAgent = config.NewSecret(fakeAdminToken) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := posterConfig("http://localhost:7001/mcp")
			tc.mutate(&cfg)
			p, err := NewPoster(cfg, nil)
			if err == nil || p != nil {
				t.Fatalf("NewPoster: %v, %v; want no poster and an error", p, err)
			}
			if strings.Contains(err.Error(), fakeAdminToken) {
				t.Errorf("error shows the token: %v", err)
			}
		})
	}
}

func TestAdminEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://localhost:7001/mcp":            "http://localhost:7001/mcp-admin",
		"https://books.example.test/mcp?x=1#f": "https://books.example.test/mcp-admin",
		"http://127.0.0.1:7001":                "http://127.0.0.1:7001/mcp-admin",
		"http://127.0.0.1:7001/a/b/mcp":        "http://127.0.0.1:7001/mcp-admin",
	}
	for in, want := range cases {
		got, err := adminEndpoint(in)
		if err != nil || got != want {
			t.Errorf("adminEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// fakeAdmin is an in-process books server: /mcp-admin requires the admin
// bearer token and serves a post_approved_journal_entry that records what
// it was sent.
type fakeAdmin struct {
	srv *httptest.Server

	mu       sync.Mutex
	paths    []string
	auths    []string
	scopes   []string
	args     []string
	calls    int
	toolErr  string
	result   *PostResult // nil: echo the proposal as posted
	otherHit int
}

func newFakeAdmin(t *testing.T) *fakeAdmin {
	t.Helper()
	fa := &fakeAdmin{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-books-admin", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: books.ToolPostApprovedJournalEntry}, func(_ context.Context, req *mcp.CallToolRequest, in books.PostApprovedInput) (*mcp.CallToolResult, PostResult, error) {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		fa.calls++
		fa.args = append(fa.args, string(req.Params.Arguments))
		if fa.toolErr != "" {
			return nil, PostResult{}, errors.New(fa.toolErr)
		}
		if fa.result != nil {
			return nil, *fa.result, nil
		}
		return nil, PostResult{ProposalID: in.ProposalID, ERPName: "ACC-JV-2026-00042", Status: "posted"}, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux := http.NewServeMux()
	mux.Handle(mcpkit.AdminPath, mcpkit.RequireBearer(fakeAdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.paths = append(fa.paths, r.URL.Path)
		fa.auths = append(fa.auths, r.Header.Get("Authorization"))
		fa.scopes = append(fa.scopes, r.Header.Get(mcpkit.ScopeHeader))
		fa.mu.Unlock()
		stream.ServeHTTP(w, r)
	})))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fa.mu.Lock()
		fa.otherHit++
		fa.mu.Unlock()
		http.Error(w, "no", http.StatusNotFound)
	})
	fa.srv = httptest.NewServer(mux)
	t.Cleanup(fa.srv.Close)
	return fa
}

func newTestPoster(t *testing.T, fa *fakeAdmin, logs *bytes.Buffer) Poster {
	t.Helper()
	p, err := NewPoster(posterConfig(fa.srv.URL+"/mcp"), slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}
	return p
}

func TestPosterSendsOnlyTheProposalID(t *testing.T) {
	fa := newFakeAdmin(t)
	var logs bytes.Buffer
	p := newTestPoster(t, fa, &logs)
	id := uuid.New()
	out, err := p.Post(t.Context(), id)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if out.ProposalID != id.String() || out.ERPName != "ACC-JV-2026-00042" || out.Status != "posted" {
		t.Errorf("result %+v", out)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if fa.calls != 1 || len(fa.args) != 1 {
		t.Fatalf("%d tool calls, want 1", fa.calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fa.args[0]), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args["proposal_id"] != strings.ToLower(id.String()) {
		t.Errorf("tool args %s, want exactly {\"proposal_id\": %q}", fa.args[0], id.String())
	}
	if len(fa.paths) == 0 {
		t.Fatal("no request reached /mcp-admin")
	}
	for i := range fa.paths {
		if fa.paths[i] != "/mcp-admin" || fa.auths[i] != "Bearer "+fakeAdminToken || fa.scopes[i] != "" {
			t.Errorf("request %d: path %q auth ok=%v scope %q", i, fa.paths[i], fa.auths[i] == "Bearer "+fakeAdminToken, fa.scopes[i])
		}
	}
	if fa.otherHit != 0 {
		t.Errorf("%d requests off /mcp-admin", fa.otherHit)
	}
	if strings.Contains(logs.String(), fakeAdminToken) {
		t.Error("logs show the admin token")
	}
}

func TestPosterRefusals(t *testing.T) {
	cases := []struct {
		name, toolErr, code string
	}{
		{"self approved", "self_approved: the checker must differ from the maker", "self_approved"},
		{"not approved", "not_approved: the proposal is not approved", "not_approved"},
		{"unknown code", "made_up: " + fakeAdminToken, RefusalUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAdmin(t)
			fa.toolErr = tc.toolErr
			var logs bytes.Buffer
			_, err := newTestPoster(t, fa, &logs).Post(t.Context(), uuid.New())
			var ref *Refusal
			if !errors.As(err, &ref) || ref.Code != tc.code {
				t.Fatalf("Post: %v, want refusal %s", err, tc.code)
			}
			if strings.Contains(err.Error(), fakeAdminToken) || strings.Contains(err.Error(), "the checker must") {
				t.Errorf("error carries tool text: %q", err.Error())
			}
		})
	}
}

func TestPosterRejectsOddResults(t *testing.T) {
	cases := []struct {
		name string
		res  func(id uuid.UUID) PostResult
	}{
		{"other proposal", func(uuid.UUID) PostResult {
			return PostResult{ProposalID: uuid.NewString(), ERPName: "X", Status: "posted"}
		}},
		{"not posted", func(id uuid.UUID) PostResult {
			return PostResult{ProposalID: id.String(), ERPName: "X", Status: "approved"}
		}},
		{"no name", func(id uuid.UUID) PostResult { return PostResult{ProposalID: id.String(), Status: "posted"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAdmin(t)
			id := uuid.New()
			r := tc.res(id)
			fa.result = &r
			var logs bytes.Buffer
			if _, err := newTestPoster(t, fa, &logs).Post(t.Context(), id); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// A poster with the wrong token gets 401 and its error never shows either
// token.
func TestPosterWrongTokenNeverShown(t *testing.T) {
	fa := newFakeAdmin(t)
	cfg := posterConfig(fa.srv.URL + "/mcp")
	const wrong = "WRONG-ADMIN-TOKEN-0b1d"
	cfg.MCPTokenAdmin = config.NewSecret(wrong)
	var logs bytes.Buffer
	p, err := NewPoster(cfg, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Post(t.Context(), uuid.New())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, s := range []string{err.Error(), logs.String(), fmt.Sprintf("%v %+v %#v %s", p, p, p, p)} {
		if strings.Contains(s, wrong) || strings.Contains(s, fakeAdminToken) {
			t.Errorf("output shows a token: %q", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("poster", "p", p)
	if strings.Contains(buf.String(), wrong) || !strings.Contains(buf.String(), posterLabel) {
		t.Errorf("slog of poster: %s", buf.String())
	}
}

func TestAdminTransportRefusesOtherURLs(t *testing.T) {
	fa := newFakeAdmin(t)
	p := newTestPoster(t, fa, &bytes.Buffer{}).(*mcpPoster)
	for _, u := range []string{fa.srv.URL + "/mcp", fa.srv.URL + "/mcp-admin/x", fa.srv.URL + "/"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.http.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: request went through", u)
		}
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if fa.otherHit != 0 || len(fa.auths) != 0 {
		t.Errorf("off-endpoint requests reached the server: %d, %d", fa.otherHit, len(fa.auths))
	}
}

func TestPosterNilID(t *testing.T) {
	fa := newFakeAdmin(t)
	if _, err := newTestPoster(t, fa, &bytes.Buffer{}).Post(t.Context(), uuid.Nil); err == nil {
		t.Error("want an error")
	}
}

// End to end through the service: a post refused by the tool reaches the
// audit row and the error as the code only, and the token never shows.
func TestServiceWithRealPosterNeverShowsToken(t *testing.T) {
	fa := newFakeAdmin(t)
	fa.toolErr = "erp_unavailable: ERPNext did not answer " + fakeAdminToken
	var logs bytes.Buffer
	poster := newTestPoster(t, fa, &logs)
	fx := newFixture(t)
	fx.svc.deps.Poster = poster
	fx.svc.deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	id := fx.proposal(t)
	_, err := fx.svc.Approve(t.Context(), "ravi", id)
	var e *Error
	if !errors.As(err, &e) || e.Code != "erp_unavailable" {
		t.Fatalf("Approve: %v", err)
	}
	var all strings.Builder
	all.WriteString(err.Error())
	all.WriteString(logs.String())
	for _, a := range fx.st.audits {
		all.Write(a.Args)
	}
	if strings.Contains(all.String(), fakeAdminToken) {
		t.Errorf("token shown in error, logs or audit: %s", all.String())
	}
}
