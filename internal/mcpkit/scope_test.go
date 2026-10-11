package mcpkit_test

// Run-scope tests (CC-506), written as the threat model: each case is an
// attempt to read outside a run, or to forge or stretch a scope.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
)

var (
	scopeKey = []byte("test-scope-key-not-a-secret")
	scopeNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
)

func goodScope() mcpkit.Scope {
	return mcpkit.Scope{RunID: "run-1", Company: "sharma", From: "2025-09-01", To: "2026-10-31", Exp: scopeNow.Add(time.Hour).Unix()}
}

func mustMint(t *testing.T, key []byte, s mcpkit.Scope) string {
	t.Helper()
	h, err := mcpkit.Mint(key, s)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return h
}

// signRaw signs an arbitrary payload the way Mint does, so a test can hand
// Verify a correctly signed payload that is not a valid scope.
func signRaw(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(m.Sum(nil))
}

func TestMintVerifyRoundTrip(t *testing.T) {
	want := goodScope()
	got, err := mcpkit.Verify(scopeKey, mustMint(t, scopeKey, want), scopeNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Errorf("Verify = %+v, want %+v", got, want)
	}
}

func TestVerifyRejects(t *testing.T) {
	good := mustMint(t, scopeKey, goodScope())
	payload, mac, _ := strings.Cut(good, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)

	tampered := []byte(string(raw))
	i := strings.Index(string(tampered), "sharma")
	tampered[i] = 'S' // "Sharma": one byte flipped after minting
	tamperedHeader := base64.RawURLEncoding.EncodeToString(tampered) + "." + mac

	expired := goodScope()
	expired.Exp = scopeNow.Unix()

	cases := []struct {
		name   string
		key    []byte
		header string
	}{
		{"empty key", nil, good},
		{"empty header", scopeKey, ""},
		{"no dot", scopeKey, payload + mac},
		{"bad base64 payload", scopeKey, "!!!" + "." + mac},
		{"bad base64 mac", scopeKey, payload + ".***"},
		{"bad json", scopeKey, signRaw(scopeKey, `{"run_id":`)},
		{"trailing json", scopeKey, signRaw(scopeKey, string(raw)+`{}`)},
		{"unknown fields", scopeKey, signRaw(scopeKey, strings.Replace(string(raw), `{`, `{"admin":true,`, 1))},
		{"wrong mac", []byte("another-key"), good},
		{"tampered payload", scopeKey, tamperedHeader},
		{"expired", scopeKey, mustMint(t, scopeKey, expired)},
		{"no exp", scopeKey, signRaw(scopeKey, `{"run_id":"r","company":"sharma","from":"2026-09-01","to":"2026-09-30"}`)},
		{"empty run id", scopeKey, signRaw(scopeKey, `{"run_id":"","company":"sharma","from":"2026-09-01","to":"2026-09-30","exp":9999999999}`)},
		{"empty company", scopeKey, signRaw(scopeKey, `{"run_id":"r","company":"","from":"2026-09-01","to":"2026-09-30","exp":9999999999}`)},
		{"bad from", scopeKey, signRaw(scopeKey, `{"run_id":"r","company":"sharma","from":"2026-9-01","to":"2026-09-30","exp":9999999999}`)},
		{"bad to", scopeKey, signRaw(scopeKey, `{"run_id":"r","company":"sharma","from":"2026-09-01","to":"2026-09-31","exp":9999999999}`)},
		{"company with odd characters", scopeKey, signRaw(scopeKey, `{"run_id":"r","company":"a b","from":"2026-09-01","to":"2026-09-30","exp":9999999999}`)},
		{"over 2 KiB", scopeKey, good + strings.Repeat("A", 2048)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := mcpkit.Verify(c.key, c.header, scopeNow); err == nil {
				t.Fatal("Verify accepted it")
			} else if c.header != "" && strings.Contains(err.Error(), c.header) {
				t.Errorf("error quotes the header: %v", err)
			}
		})
	}
}

func TestMintRejects(t *testing.T) {
	if _, err := mcpkit.Mint(nil, goodScope()); err == nil {
		t.Error("Mint without a key succeeded")
	}
	bad := goodScope()
	bad.Company = ""
	if _, err := mcpkit.Mint(scopeKey, bad); err == nil {
		t.Error("Mint of a scope without a company succeeded")
	}
}

func TestWindow(t *testing.T) {
	exp := scopeNow.Add(time.Hour)
	cases := []struct{ month, from, to string }{
		{"2026-09", "2025-09-01", "2026-10-31"},
		{"2026-12", "2025-12-01", "2027-01-31"}, // To in the next year
		{"2028-01", "2027-01-01", "2028-02-29"}, // leap day
		{"2026-01", "2025-01-01", "2026-02-28"},
	}
	for _, c := range cases {
		s, err := mcpkit.Window("sharma", "run-1", c.month, exp)
		if err != nil {
			t.Fatalf("%s: %v", c.month, err)
		}
		if s.From != c.from || s.To != c.to || s.Exp != exp.Unix() || s.Company != "sharma" || s.RunID != "run-1" {
			t.Errorf("%s: %+v, want %s..%s", c.month, s, c.from, c.to)
		}
		if s.Month() != c.month {
			t.Errorf("%s: Month() = %s", c.month, s.Month())
		}
	}
	for _, bad := range []string{"", "2026-9", "2026-13", "2026-09-01"} {
		if _, err := mcpkit.Window("sharma", "run-1", bad, exp); err == nil {
			t.Errorf("Window(%q) succeeded", bad)
		}
	}
	if _, err := mcpkit.Window("", "run-1", "2026-09", exp); err == nil {
		t.Error("Window without a company succeeded")
	}
}

func TestAllows(t *testing.T) {
	s := goodScope() // sharma, 2025-09-01..2026-10-31
	allowed := [][3]string{
		{"sharma", "2026-09-01", "2026-09-30"},
		{"sharma", "2025-09-01", "2026-10-31"}, // the edges
	}
	for _, a := range allowed {
		if err := s.Allows(a[0], a[1], a[2]); err != nil {
			t.Errorf("Allows%v: %v", a, err)
		}
	}
	refused := [][3]string{
		{"mehta", "2026-09-01", "2026-09-30"},  // another company
		{"Sharma", "2026-09-01", "2026-09-30"}, // case matters
		{"", "2026-09-01", "2026-09-30"},
		{"sharma", "2025-08-31", "2026-09-30"}, // a day before From
		{"sharma", "2026-09-01", "2026-11-01"}, // a day after To
		{"sharma", "2026-9-1", "2026-09-30"},   // not YYYY-MM-DD
		{"sharma", "2026-09-01", "9999"},
	}
	for _, r := range refused {
		err := s.Allows(r[0], r[1], r[2])
		if !errors.Is(err, mcpkit.ErrOutOfScope) || !strings.HasPrefix(err.Error(), "out_of_scope:") {
			t.Errorf("Allows%v = %v, want out_of_scope", r, err)
		}
	}
	long := strings.Repeat("x", 200)
	if err := s.Allows(long, "2026-09-01", "2026-09-30"); err == nil || strings.Contains(err.Error(), long[:41]) {
		t.Errorf("echo of a long company is not capped: %v", err)
	}
}

func TestDiscoveryScope(t *testing.T) {
	d := mcpkit.DiscoveryScope(scopeNow)
	if d.Exp != scopeNow.Add(time.Minute).Unix() || d.Company != "-discovery-" || d.RunID != "discovery" {
		t.Fatalf("DiscoveryScope = %+v", d)
	}
	got, err := mcpkit.Verify(scopeKey, mustMint(t, scopeKey, d), scopeNow)
	if err != nil {
		t.Fatalf("Verify(discovery): %v", err)
	}
	for _, c := range [][3]string{
		{"-discovery-", "2000-01-02", "2000-01-01"}, // its own edges
		{"-discovery-", "2000-01-01", "2000-01-02"},
		{"-discovery-", "2000-01-01", "2000-01-01"},
		{"sharma", "2026-09-01", "2026-09-30"},
		{"mehta", "2000-01-01", "2000-01-01"},
	} {
		if err := got.Allows(c[0], c[1], c[2]); !errors.Is(err, mcpkit.ErrOutOfScope) {
			t.Errorf("discovery Allows%v = %v, want out_of_scope", c, err)
		}
	}
	if err := got.AllowsCompany("-discovery-"); !errors.Is(err, mcpkit.ErrOutOfScope) {
		t.Errorf("discovery AllowsCompany = %v, want out_of_scope", err)
	}
}

func TestScoper(t *testing.T) {
	if _, err := mcpkit.NewScoper(""); err == nil {
		t.Fatal("NewScoper with an empty key succeeded")
	}
	t.Run("nil scoper is unscoped", func(t *testing.T) {
		var s *mcpkit.Scoper
		if err := s.CheckRange(&mcp.CallToolRequest{}, "anyone", "1999-01-01", "2099-01-01"); err != nil {
			t.Errorf("nil Scoper refused: %v", err)
		}
		if err := s.CheckCompany(nil, "anyone"); err != nil {
			t.Errorf("nil Scoper refused: %v", err)
		}
		if _, ok := s.Scope(nil); ok {
			t.Error("nil Scoper returned a scope")
		}
	})
	s, err := mcpkit.NewScoper(string(scopeKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("no Extra (stdio or in-process) is refused", func(t *testing.T) {
		for _, req := range []*mcp.CallToolRequest{nil, {}, {Extra: &mcp.RequestExtra{}}} {
			if err := s.CheckRange(req, "sharma", "2026-09-01", "2026-09-30"); !errors.Is(err, mcpkit.ErrOutOfScope) {
				t.Errorf("CheckRange = %v, want out_of_scope", err)
			}
			if err := s.CheckCompany(req, "sharma"); !errors.Is(err, mcpkit.ErrOutOfScope) {
				t.Errorf("CheckCompany = %v, want out_of_scope", err)
			}
		}
	})
	sc := goodScope()
	sc.Exp = time.Now().Add(time.Hour).Unix()
	req := func(vals ...string) *mcp.CallToolRequest {
		h := http.Header{}
		for _, v := range vals {
			h.Add(mcpkit.ScopeHeader, v)
		}
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
	}
	good := req(mustMint(t, scopeKey, sc))
	t.Run("good header", func(t *testing.T) {
		if err := s.Check(good, "sharma", "2026-09-01", "2026-09-30"); err != nil {
			t.Errorf("Check: %v", err)
		}
		if got, ok := s.Scope(good); !ok || got != sc {
			t.Errorf("Scope = %+v %v", got, ok)
		}
		if id, ok := s.RunID(good); !ok || id != "run-1" {
			t.Errorf("RunID = %q %v", id, ok)
		}
		if err := s.CheckCompany(good, "mehta"); !errors.Is(err, mcpkit.ErrOutOfScope) {
			t.Errorf("CheckCompany(mehta) = %v", err)
		}
	})
	t.Run("missing, forged or doubled header is refused", func(t *testing.T) {
		other := sc
		other.Company = "mehta"
		for name, r := range map[string]*mcp.CallToolRequest{
			"missing": req(),
			"forged":  req(mustMint(t, []byte("not-the-key"), other)),
			"doubled": req(mustMint(t, scopeKey, sc), mustMint(t, scopeKey, other)),
		} {
			if err := s.Check(r, "sharma", "2026-09-01", "2026-09-30"); !errors.Is(err, mcpkit.ErrOutOfScope) {
				t.Errorf("%s: Check = %v, want out_of_scope", name, err)
			}
		}
	})
}

// ---- the HTTP layer ----

// scopeTransport adds a bearer token and, when set, a run scope.
type scopeTransport struct {
	token string
	mu    sync.Mutex
	scope string
}

func (s *scopeTransport) set(h string) {
	s.mu.Lock()
	s.scope = h
	s.mu.Unlock()
}

func (s *scopeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+s.token)
	s.mu.Lock()
	if s.scope != "" {
		r.Header.Set(mcpkit.ScopeHeader, s.scope)
	}
	s.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

type companyIn struct {
	Company string `json:"company"`
	From    string `json:"from"`
	To      string `json:"to"`
	Actor   string `json:"actor,omitempty"`
}

// scopedServer serves one read tool that checks its arguments against the
// scope, as the books and evidence handlers do.
func scopedServer(s *mcpkit.Scoper) *mcp.Server {
	srv := mcpkit.NewServer("scoped", "1")
	mcpkit.AddTool(srv, &mcp.Tool{Name: "read", Description: "d", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest, in companyIn) (*mcp.CallToolResult, auditOut, error) {
			if err := s.CheckRange(req, in.Company, in.From, in.To); err != nil {
				return nil, auditOut{}, err
			}
			return nil, auditOut{OK: true}, nil
		})
	return srv
}

func newTestScoper(t *testing.T) *mcpkit.Scoper {
	t.Helper()
	s, err := mcpkit.NewScoper(string(scopeKey))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// liveScope is goodScope valid for an hour from the real clock.
func liveScope(t *testing.T) string {
	t.Helper()
	sc := goodScope()
	sc.Exp = time.Now().Add(time.Hour).Unix()
	return mustMint(t, scopeKey, sc)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// post sends an initialize request to path and returns the status and body.
func post(t *testing.T, base, path, token string, scopes ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, s := range scopes {
		req.Header.Add(mcpkit.ScopeHeader, s)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestScopedHandler401(t *testing.T) {
	s := newTestScoper(t)
	h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(scopedServer(s), "tok")}, testLogger(),
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}), mcpkit.WithScoper(s, "/mcp"))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	good := liveScope(t)
	expired := goodScope()
	expired.Exp = time.Now().Add(-time.Second).Unix()
	payload, mac, _ := strings.Cut(good, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tampered := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "sharma", "mehta", 1))) + "." + mac

	cases := []struct {
		name   string
		token  string
		scopes []string
	}{
		{"wrong bearer with a good scope", "wrong", []string{good}},
		{"no scope", "tok", nil},
		{"expired scope", "tok", []string{mustMint(t, scopeKey, expired)}},
		{"tampered scope", "tok", []string{tampered}},
		{"forged scope", "tok", []string{mustMint(t, []byte("guessed-key"), goodScope())}},
		{"two scopes", "tok", []string{good, good}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := post(t, ts.URL, "/mcp", c.token, c.scopes...)
			if code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", code)
			}
			for _, sc := range c.scopes {
				if strings.Contains(body, sc) || strings.Contains(body, string(scopeKey)) {
					t.Errorf("401 body leaks the scope or key: %q", body)
				}
			}
		})
	}

	t.Run("healthz stays open", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/healthz %d", resp.StatusCode)
		}
	})

	t.Run("good scope reaches the tool, across initialize, list and call", func(t *testing.T) {
		tr := &scopeTransport{token: "tok"}
		tr.set(good)
		sess, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(t.Context(),
			&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tr}, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		if _, err := sess.ListTools(t.Context(), nil); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		res := call(t, sess, "read", map[string]any{"company": "sharma", "from": "2026-09-01", "to": "2026-09-30"})
		if res.IsError {
			t.Fatal("in-scope read refused")
		}
		res = call(t, sess, "read", map[string]any{"company": "mehta", "from": "2026-09-01", "to": "2026-09-30"})
		if !res.IsError || !strings.HasPrefix(res.Content[0].(*mcp.TextContent).Text, "out_of_scope") {
			t.Errorf("another company: want an out_of_scope tool error, got %+v", res.Content)
		}
	})

	t.Run("an unscoped handler is unchanged", func(t *testing.T) {
		plain := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(mcpkit.NewServer("p", "1"), "tok")}, testLogger()))
		t.Cleanup(plain.Close)
		if code, _ := post(t, plain.URL, "/mcp", "tok"); code != http.StatusOK {
			t.Errorf("unscoped handler without a scope: %d, want 200", code)
		}
		if code, _ := post(t, plain.URL, "/mcp", "wrong"); code != http.StatusUnauthorized {
			t.Errorf("unscoped handler with a wrong bearer: %d, want 401", code)
		}
	})

	t.Run("a nil scoper refuses everything", func(t *testing.T) {
		h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(mcpkit.NewServer("n", "1"), "tok")}, testLogger(),
			mcpkit.WithScoper(nil, "/mcp"))
		ns := httptest.NewServer(h)
		t.Cleanup(ns.Close)
		if code, _ := post(t, ns.URL, "/mcp", "tok", good); code != http.StatusUnauthorized {
			t.Errorf("nil scoper: %d, want 401", code)
		}
	})
}

// TestAdminRouteNoScope: /mcp-admin serves with the right bearer and no
// scope, even when it is listed for scoping, while /mcp next to it is
// scoped.
func TestAdminRouteNoScope(t *testing.T) {
	s := newTestScoper(t)
	h := mcpkit.BuildHandler(map[string]mcpkit.Route{
		"/mcp":       mcpkit.NewRoute(scopedServer(s), "agent-tok"),
		"/mcp-admin": mcpkit.NewRoute(mcpkit.NewServer("admin", "1"), "admin-tok"),
	}, testLogger(),
		mcpkit.WithActors(map[string]string{"/mcp": "agent", "/mcp-admin": "admin"}),
		mcpkit.WithScoper(s, "/mcp", mcpkit.AdminPath))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	if code, body := post(t, ts.URL, "/mcp-admin", "admin-tok"); code != http.StatusOK {
		t.Errorf("/mcp-admin without a scope: %d %s, want 200", code, body)
	}
	if code, _ := post(t, ts.URL, "/mcp-admin", "agent-tok"); code != http.StatusUnauthorized {
		t.Errorf("/mcp-admin with the agent token: %d, want 401", code)
	}
	if code, _ := post(t, ts.URL, "/mcp", "agent-tok"); code != http.StatusUnauthorized {
		t.Errorf("/mcp without a scope: %d, want 401", code)
	}
}
