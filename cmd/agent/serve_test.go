package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/agent"
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
func (s *serveStore) InsertAuditLog(context.Context, store.AuditLogEntry) (int64, error) {
	return 1, nil
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
