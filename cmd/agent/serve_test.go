package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/approvals"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web"
)

const (
	serveSessionKey = "0123456789abcdef0123456789abcdef"
	serveHost       = "example.com"
	servePassword   = "viewer-test-password-1"
)

// serveStore is an in-memory web.RunsStore and web.FindingsStore.
type serveStore struct {
	run     store.CloseRun
	finding store.Finding

	mu        sync.Mutex
	proposals map[uuid.UUID]store.JournalProposal
	audits    []store.AuditLogEntry
}

func (s *serveStore) CreateCloseRun(context.Context, store.CloseRun) error { return nil }
func (s *serveStore) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	if id != s.run.ID {
		return store.CloseRun{}, store.ErrNotFound
	}
	return s.run, nil
}
func (s *serveStore) ListCloseRuns(context.Context, int) ([]store.RunSummary, error) {
	return []store.RunSummary{{CloseRun: s.run, Findings: 1}}, nil
}
func (s *serveStore) ListFindingsByRun(context.Context, uuid.UUID) ([]store.Finding, error) {
	return []store.Finding{s.finding}, nil
}
func (s *serveStore) ListCompanies(context.Context) ([]store.Company, error) {
	return []store.Company{{ID: "acme"}}, nil
}
func (s *serveStore) GetFinding(_ context.Context, id uuid.UUID) (store.Finding, error) {
	if id != s.finding.ID {
		return store.Finding{}, store.ErrNotFound
	}
	return s.finding, nil
}
func (s *serveStore) ListSteps(context.Context, uuid.UUID) ([]store.Step, error) { return nil, nil }
func (s *serveStore) GetArtifact(context.Context, string) (store.Artifact, error) {
	return store.Artifact{}, store.ErrNotFound
}
func (s *serveStore) SetFindingStatus(context.Context, uuid.UUID, string) error { return nil }
func (s *serveStore) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, e)
	return int64(len(s.audits)), nil
}

// The journal_proposals side of serveStore, for the approvals wiring.

func (s *serveStore) GetProposalByFinding(_ context.Context, id uuid.UUID) (store.JournalProposal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.proposals {
		if p.FindingID != nil && *p.FindingID == id && p.Status != "rejected" {
			return p, true, nil
		}
	}
	return store.JournalProposal{}, false, nil
}

func (s *serveStore) CreateProposalFromFinding(ctx context.Context, p store.JournalProposal) (store.JournalProposal, error) {
	if live, ok, _ := s.GetProposalByFinding(ctx, *p.FindingID); ok {
		return live, store.ErrProposalExists
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proposals == nil {
		s.proposals = map[uuid.UUID]store.JournalProposal{}
	}
	s.proposals[p.ID] = p
	return p, nil
}

func (s *serveStore) GetJournalProposal(_ context.Context, id uuid.UUID) (store.JournalProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	if !ok {
		return store.JournalProposal{}, store.ErrNotFound
	}
	return p, nil
}

func (s *serveStore) ListProposalsByStatus(_ context.Context, status string, _ int) ([]store.JournalProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.JournalProposal
	for _, p := range s.proposals {
		if p.Status == status {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *serveStore) WithProposalLock(ctx context.Context, id uuid.UUID, fn func(context.Context, store.JournalProposal, func(context.Context, store.JournalProposal) error) error) error {
	p, err := s.GetJournalProposal(ctx, id)
	if err != nil {
		return err
	}
	return fn(ctx, p, func(_ context.Context, np store.JournalProposal) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.proposals[id] = np
		return nil
	})
}

func (s *serveStore) WithProposalFindingLock(ctx context.Context, id uuid.UUID, fn func(context.Context, store.JournalProposal, store.FindingStatusFunc, func(context.Context, store.JournalProposal) error) error) error {
	return s.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		read := func(ctx context.Context) (string, bool, error) {
			if p.FindingID == nil {
				return "", false, nil
			}
			f, err := s.GetFinding(ctx, *p.FindingID)
			if err != nil {
				return "", false, nil
			}
			return f.Status, true, nil
		}
		return fn(ctx, p, read, save)
	})
}

func serveUsers(t *testing.T) web.Users {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(servePassword), web.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("tok"))
	us, err := web.NewUsers(web.User{Name: "demo", Role: web.RoleViewer, PasswordHash: string(h), TokenSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return us
}

func serveConfig() config.Config {
	return config.Config{
		AppSessionKey: config.NewSecret(serveSessionKey),
		MCPScopeKey:   config.NewSecret("scope-key"),
		AppAddr:       "127.0.0.1:0",
	}
}

func newServeStore() *serveStore {
	rid := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	fid := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000001")
	return &serveStore{
		run:     store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone},
		finding: store.Finding{ID: fid, RunID: rid, Type: "bank_charge", Severity: "high", Title: "Unbooked bank charge", Keys: map[string]string{}, Status: "open"},
	}
}

func serveHandler(t *testing.T, st *serveStore) *web.Server {
	t.Helper()
	exec := func(context.Context, uuid.UUID) error { return nil }
	srv, err := newServeServer(serveConfig(), serveUsers(t),
		serveBackends{runs: &web.RunsBackend{Store: st, Execute: exec, Findings: st}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newServeServer: %v", err)
	}
	return srv
}

// jar is a minimal cookie jar over a recorder-driven handler.
type jar map[string]string

func (j jar) do(t *testing.T, h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", "http://"+serveHost)
	}
	for k, v := range j {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, line := range w.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("Set-Cookie %q: %v", line, err)
		}
		if c.MaxAge < 0 {
			delete(j, c.Name)
			continue
		}
		j[c.Name] = c.Value
	}
	return w
}

var serveCSRF = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func TestServeHealthz(t *testing.T) {
	h := serveHandler(t, newServeStore())
	w := jar{}.do(t, h, http.MethodGet, "/healthz", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("/healthz = %d %s", w.Code, w.Body)
	}
}

// TestServeViewerSeesRunAndFinding logs a viewer in and reads the runs
// list and one finding page.
func TestServeViewerSeesRunAndFinding(t *testing.T) {
	st := newServeStore()
	h := serveHandler(t, st)
	j := jar{}
	w := j.do(t, h, http.MethodGet, "/login", nil)
	m := serveCSRF.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		t.Fatalf("GET /login = %d, csrf field %v", w.Code, m != nil)
	}
	w = j.do(t, h, http.MethodPost, "/login", url.Values{"csrf_token": {m[1]}, "username": {"demo"}, "password": {servePassword}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /login = %d: %s", w.Code, w.Body)
	}

	w = j.do(t, h, http.MethodGet, "/runs", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") || !strings.Contains(w.Body.String(), st.run.ID.String()) {
		t.Errorf("/runs = %d, run missing from the list", w.Code)
	}
	w = j.do(t, h, http.MethodGet, "/findings/"+st.finding.ID.String(), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Unbooked bank charge") {
		t.Errorf("/findings/{id} = %d: %.200s", w.Code, w.Body)
	}
}

func TestServeMissingKeysFailBeforeListening(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, tt := range []struct {
		name string
		mod  func(*config.Config)
		want error
	}{
		{"session key", func(c *config.Config) { c.AppSessionKey = config.Secret{} }, errSessionKey},
		{"scope key", func(c *config.Config) { c.MCPScopeKey = config.Secret{} }, errScopeKey},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := serveConfig()
			tt.mod(&cfg)
			// serve checks the keys before it touches the database or the MCP servers.
			err := serve(ctx, cfg, slog.New(slog.DiscardHandler), command{name: cmdServe, configDir: "config", resultsDir: "results", noExplain: true})
			if err != tt.want {
				t.Errorf("serve: %v, want %v", err, tt.want)
			}
			_, err = newServeServer(cfg, serveUsers(t), serveBackends{}, slog.New(slog.DiscardHandler))
			if err != tt.want {
				t.Errorf("newServeServer: %v, want %v", err, tt.want)
			}
			if strings.Contains(tt.want.Error(), serveSessionKey) {
				t.Error("error carries a key")
			}
		})
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServeBindErrorIsReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := serveConfig()
	cfg.AppAddr = ln.Addr().String()
	logs := &syncBuf{}
	srv, err := newServeServer(cfg, serveUsers(t), serveBackends{}, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Fatal("Run on a taken address returned nil")
	}
	if strings.Contains(logs.String(), "listening") {
		t.Errorf("logged listening before the bind failed: %s", logs)
	}
}

func TestServeListensAndStops(t *testing.T) {
	logs := &syncBuf{}
	srv, err := newServeServer(serveConfig(), serveUsers(t), serveBackends{}, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", ln.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if !strings.Contains(logs.String(), "listening") {
		t.Error("no listening log")
	}
}

// TestServeAskAdapterMapsErrors checks that the adapter turns the Asker's
// sentinels into the web package's. A question over the length cap fails
// before any store or model call.
func TestServeAskAdapterMapsErrors(t *testing.T) {
	profiles, err := company.LoadProfiles("../../config/companies")
	if err != nil || len(profiles) == 0 {
		t.Fatalf("profiles: %v", err)
	}
	rules, err := agent.LoadTriageRules("../../config/triage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newRed, err := agent.AskRedactorFunc([]byte("serve-test-key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var (
		ast  = stubAskStore{}
		rec  = stubRecorder{}
		tst  = stubTriage{}
		tool struct{ agent.ToolExecutor }
		ret  struct{ agent.Retriever }
		fake = llm.NewFakeProvider()
	)
	tr, err := agent.NewTriage(rules, profiles, fake, rec, newRed, tst, "fast")
	if err != nil {
		t.Fatalf("NewTriage: %v", err)
	}
	asker, err := agent.NewAsker(agent.AskerConfig{
		Triage: tr, Tools: tool, Retriever: ret, Store: ast, Recorder: rec,
		NewRedactor: newRed, Model: fake, ModelName: "strong", Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewAsker: %v", err)
	}
	_, err = askAdapter{asker: asker}.Ask(t.Context(), web.AskInput{
		Company: profiles[0].ID, Month: "2026-09", Question: strings.Repeat("x ", agent.MaxQuestionRunes),
	})
	if !errors.Is(err, web.ErrQuestionTooLong) {
		t.Errorf("err = %v, want web.ErrQuestionTooLong", err)
	}
}

// Stubs for the Asker's and the router's stores; the test never reaches them.
type (
	stubAskStore struct{ agent.AskStore }
	stubRecorder struct{ agent.LLMCallRecorder }
	stubTriage   struct{ agent.TriageStore }
)

// ---- approvals wiring (CC-1004) ----

const (
	serveAdminToken = "SERVE-ADMIN-TOKEN-SENTINEL-5e1a"
	serveAgentToken = "serve-agent-token"
)

// approvalUsers are asha (maker) and ravi (checker) with bearer tokens.
func approvalUsers(t *testing.T) web.Users {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(servePassword), web.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	tok := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	us, err := web.NewUsers(
		web.User{Name: "asha", Role: web.RoleMaker, PasswordHash: string(h), TokenSHA256: tok("tok-asha")},
		web.User{Name: "ravi", Role: web.RoleChecker, PasswordHash: string(h), TokenSHA256: tok("tok-ravi")},
	)
	if err != nil {
		t.Fatal(err)
	}
	return us
}

// fakeBooksAdmin is an in-process /mcp-admin that answers
// post_approved_journal_entry and records the bearer token it saw.
func fakeBooksAdmin(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		auths []string
	)
	type in struct {
		ProposalID string `json:"proposal_id"`
	}
	type out struct {
		ProposalID    string `json:"proposal_id"`
		ERPName       string `json:"erp_name"`
		Status        string `json:"status"`
		AlreadyPosted bool   `json:"already_posted"`
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-books-admin", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "post_approved_journal_entry"}, func(_ context.Context, _ *mcp.CallToolRequest, x in) (*mcp.CallToolResult, out, error) {
		return nil, out{ProposalID: x.ProposalID, ERPName: "ACC-JV-2026-00321", Status: "posted"}, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp-admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+serveAdminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		stream.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

func TestServeApprovalsDisabledWithoutAdminToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*config.Config)
	}{
		{"no admin token", func(*config.Config) {}},
		{"no books url", func(c *config.Config) { c.MCPTokenAdmin = config.NewSecret(serveAdminToken); c.BooksMCPURL = "" }},
		{"admin equals agent", func(c *config.Config) {
			c.MCPTokenAdmin = config.NewSecret(serveAgentToken)
			c.MCPTokenAgent = config.NewSecret(serveAgentToken)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := serveConfig()
			cfg.BooksMCPURL = "http://127.0.0.1:7001/mcp"
			tc.mod(&cfg)
			var logs syncBuf
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			be := newApprovals(cfg, newServeStore(), log)
			if be != nil {
				t.Fatal("approvals built without a usable admin token")
			}
			if n := strings.Count(logs.String(), "approvals are disabled"); n != 1 || !strings.Contains(logs.String(), `"level":"WARN"`) {
				t.Errorf("warn logged %d times: %s", n, logs.String())
			}
			if strings.Contains(logs.String(), serveAgentToken) || strings.Contains(logs.String(), serveAdminToken) {
				t.Error("a token was logged")
			}
			st := newServeStore()
			srv, err := newServeServer(cfg, approvalUsers(t), serveBackends{
				runs: &web.RunsBackend{Store: st, Execute: func(context.Context, uuid.UUID) error { return nil }, Findings: st}, approvals: be,
			}, log)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/proposals/"+uuid.NewString()+"/approve", nil)
			r.Header.Set("Authorization", "Bearer tok-ravi")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if w.Code != http.StatusNotImplemented {
				t.Errorf("approve without approvals = %d, want 501", w.Code)
			}
		})
	}
}

// TestServeApprovalsWired runs propose and approve through the serve
// wiring against an in-process /mcp-admin: the poster presents the admin
// token there, and the token shows in no response, log or audit row.
func TestServeApprovalsWired(t *testing.T) {
	admin, auths := fakeBooksAdmin(t)
	cfg := serveConfig()
	cfg.BooksMCPURL = admin.URL + "/mcp"
	cfg.ERPBaseURL = "http://erp.test:8080"
	cfg.MCPTokenAdmin = config.NewSecret(serveAdminToken)
	cfg.MCPTokenAgent = config.NewSecret(serveAgentToken)
	var logs syncBuf
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	st := newServeStore()
	st.finding.Verified = true
	st.finding.Proposal = &store.JournalProposal{CompanyID: "acme", Status: "proposed", Payload: store.JournalPayload{
		PostingDate: "2026-09-30", Remark: "bank charge", Lines: []store.JournalLine{
			{Account: "Bank Charges - AC", DebitPaise: 29500}, {Account: "Bank - AC", CreditPaise: 29500},
		},
	}}
	be := newApprovals(cfg, st, log)
	if be == nil {
		t.Fatalf("newApprovals: nil; logs %s", logs.String())
	}
	srv, err := newServeServer(cfg, approvalUsers(t), serveBackends{
		runs: &web.RunsBackend{Store: st, Execute: func(context.Context, uuid.UUID) error { return nil }, Findings: st}, approvals: be,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	var bodies strings.Builder
	call := func(token, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"maker":"ravi","checker":"asha"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		bodies.WriteString(w.Body.String())
		return w
	}

	w := call("tok-asha", http.MethodPost, "/api/findings/"+st.finding.ID.String()+"/proposals")
	if w.Code != http.StatusCreated {
		t.Fatalf("propose = %d %s", w.Code, w.Body)
	}
	var created struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if w := call("tok-asha", http.MethodPost, "/api/findings/"+st.finding.ID.String()+"/proposals"); w.Code != http.StatusConflict {
		t.Errorf("second propose = %d", w.Code)
	}
	if w := call("tok-asha", http.MethodPost, "/api/proposals/"+created.ProposalID+"/approve"); w.Code != http.StatusForbidden {
		t.Errorf("maker approve = %d", w.Code)
	}
	if len(auths()) != 0 {
		t.Fatal("ERPNext reached before a checker approved")
	}
	w = call("tok-ravi", http.MethodPost, "/api/proposals/"+created.ProposalID+"/approve")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ACC-JV-2026-00321") ||
		!strings.Contains(w.Body.String(), "http://erp.test:8080/app/journal-entry/ACC-JV-2026-00321") {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	got := auths()
	if len(got) == 0 {
		t.Fatal("the admin route was not called")
	}
	for _, a := range got {
		if a != "Bearer "+serveAdminToken {
			t.Errorf("admin route saw %q", a)
		}
	}
	id := uuid.MustParse(created.ProposalID)
	row, _ := st.GetJournalProposal(t.Context(), id)
	if row.Maker != "asha" || row.Checker == nil || *row.Checker != "ravi" || row.Status != "approved" || row.ERPName != nil {
		t.Errorf("row %+v (the service must not write posted or erp_name)", row)
	}
	// Reject of an approved proposal is refused.
	if w := call("tok-ravi", http.MethodPost, "/api/proposals/"+created.ProposalID+"/reject"); w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
		t.Errorf("reject approved = %d", w.Code)
	}

	st.mu.Lock()
	audits := append([]store.AuditLogEntry(nil), st.audits...)
	st.mu.Unlock()
	var actors []string
	var auditArgs strings.Builder
	for _, a := range audits {
		if a.Server != nil && *a.Server == "approvals" {
			actors = append(actors, a.Actor+":"+*a.Tool)
		}
		auditArgs.Write(a.Args)
	}
	if !slices.Contains(actors, "asha:proposal.create") || !slices.Contains(actors, "ravi:proposal.approve") {
		t.Errorf("audit rows %v", actors)
	}
	for name, out := range map[string]string{"responses": bodies.String(), "logs": logs.String(), "audit": auditArgs.String()} {
		if strings.Contains(out, serveAdminToken) {
			t.Errorf("%s show the admin token", name)
		}
	}
}

func TestAdaptApprovalErr(t *testing.T) {
	plain := errors.New("db down")
	cases := []struct {
		name string
		err  error
		kind error
	}{
		{"not found", &approvals.Error{Kind: approvals.ErrNotFound, Msg: "not found"}, web.ErrApprovalNotFound},
		{"invalid proposal", &approvals.Error{Kind: approvals.ErrInvalidProposal, Msg: "m"}, web.ErrApprovalInvalid},
		{"invalid reason", &approvals.Error{Kind: approvals.ErrInvalidReason, Msg: "m"}, web.ErrApprovalInvalid},
		{"self", &approvals.Error{Kind: approvals.ErrSelfApproval, Msg: approvals.ErrSelfApproval.Error()}, web.ErrApprovalForbidden},
		{"not proposable", &approvals.Error{Kind: approvals.ErrNotProposable, Msg: "m"}, web.ErrApprovalConflict},
		{"exists", &approvals.Error{Kind: approvals.ErrProposalExists, Msg: "m"}, web.ErrApprovalConflict},
		{"not pending", &approvals.Error{Kind: approvals.ErrNotPending, Msg: "m"}, web.ErrApprovalConflict},
		{"finding not open", &approvals.Error{Kind: approvals.ErrFindingNotOpen, Msg: approvals.ErrFindingNotOpen.Error(), Code: "finding_not_open"}, web.ErrApprovalConflict},
		{"post failed", &approvals.Error{Kind: approvals.ErrPostFailed, Msg: "m", Code: "unbalanced"}, web.ErrApprovalPostFailed},
	}
	for _, tc := range cases {
		got := adaptApprovalErr(tc.err)
		var ae *web.ApprovalError
		if !errors.Is(got, tc.kind) || !errors.As(got, &ae) || ae.Message != tc.err.Error() {
			t.Errorf("%s: %v, want kind %v with the service's message", tc.name, got, tc.kind)
		}
	}
	if got := adaptApprovalErr(plain); got != plain {
		t.Errorf("plain error: %v", got)
	}
	if adaptApprovalErr(nil) != nil {
		t.Error("nil error adapted")
	}
}
