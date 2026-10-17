package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/web"
)

type fakeSystemStore struct {
	pingErr, spendErr error
	spend             string
	gotNow            time.Time
}

func (f *fakeSystemStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeSystemStore) LLMSpendToday(_ context.Context, now time.Time) (string, error) {
	f.gotNow = now
	return f.spend, f.spendErr
}

// erpStandIn answers /api/method/ping with status and records what it saw.
type erpStandIn struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	header []http.Header
}

func newERPStandIn(t *testing.T, status int) *erpStandIn {
	t.Helper()
	e := &erpStandIn{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.paths = append(e.paths, r.Method+" "+r.URL.RequestURI())
		e.header = append(e.header, r.Header.Clone())
		e.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(e.Close)
	return e
}

func probeFor(t *testing.T, base string) func(context.Context) error {
	t.Helper()
	opts, err := newSystemOptions(config.Config{ERPBaseURL: base, LLMProvider: "claude-cli"}, &fakeSystemStore{})
	if err != nil {
		t.Fatal(err)
	}
	return opts.Backend.ERPNext
}

func TestSystemERPProbe(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		ok     bool
	}{
		{"200", http.StatusOK, true},
		{"204", http.StatusNoContent, true},
		{"401", http.StatusUnauthorized, false},
		{"404", http.StatusNotFound, false},
		{"503", http.StatusServiceUnavailable, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newERPStandIn(t, tt.status)
			err := probeFor(t, e.URL)(context.Background())
			if (err == nil) != tt.ok {
				t.Errorf("status %d: err %v, want ok=%v", tt.status, err, tt.ok)
			}
			if len(e.paths) != 1 || e.paths[0] != "GET /api/method/ping" {
				t.Errorf("requests %v, want one GET /api/method/ping", e.paths)
			}
		})
	}
}

func TestSystemERPProbeSendsNoCredentials(t *testing.T) {
	e := newERPStandIn(t, http.StatusOK)
	u, _ := url.Parse(e.URL)
	u.User = url.UserPassword("apikey", "s3cret")
	u.Path = "/site"
	u.RawQuery = "token=abc"
	cfg := config.Config{ERPBaseURL: u.String(), ERPAPIKey: config.NewSecret("key"), LLMProvider: "claude-cli"}
	opts, err := newSystemOptions(cfg, &fakeSystemStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.Backend.ERPNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.paths) != 1 || e.paths[0] != "GET /site/api/method/ping" {
		t.Errorf("requests %v", e.paths)
	}
	for _, h := range e.header[0] {
		for _, v := range h {
			if strings.Contains(v, "s3cret") || strings.Contains(v, "abc") {
				t.Errorf("a header carries a secret: %q", v)
			}
		}
	}
	if e.header[0].Get("Authorization") != "" || e.header[0].Get("Cookie") != "" {
		t.Errorf("credentials sent: %v", e.header[0])
	}
	if opts.Info.ERPHost != u.Host || strings.ContainsAny(opts.Info.ERPHost, "@/?") {
		t.Errorf("ERPHost %q, want %q", opts.Info.ERPHost, u.Host)
	}
}

func TestSystemERPProbeDownAndUnset(t *testing.T) {
	e := newERPStandIn(t, http.StatusOK)
	base := e.URL
	e.Close()
	if err := probeFor(t, base)(context.Background()); err == nil {
		t.Error("a closed port reported up")
	} else if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error carries the address: %v", err)
	}
	opts, err := newSystemOptions(config.Config{LLMProvider: "claude-cli"}, &fakeSystemStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.Backend.ERPNext(context.Background()); !errors.Is(err, web.ErrSystemNotConfigured) {
		t.Errorf("unset ERP_BASE_URL: %v, want ErrSystemNotConfigured", err)
	}
	if opts.Info.ERPHost != "" {
		t.Errorf("ERPHost %q for an unset URL", opts.Info.ERPHost)
	}
}

func TestSystemAdapterSpend(t *testing.T) {
	for _, tt := range []struct {
		name   string
		spend  string
		budget float64
		want   float64
		err    error
	}{
		{"quarter", "1.250000", 5, 0.25, nil},
		{"no budget", "1.25", 0, 0, nil},
		{"over", "10", 5, 2, nil},
		{"store error", "", 5, 0, errors.New("boom")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeSystemStore{spend: tt.spend, spendErr: tt.err}
			a := systemAdapter{st: st, budget: tt.budget, now: func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) }}
			got, err := a.SpendToday(context.Background())
			if !errors.Is(err, tt.err) {
				t.Fatalf("err %v, want %v", err, tt.err)
			}
			if err == nil && (got.USD != tt.spend || got.Fraction != tt.want) {
				t.Errorf("got %+v, want USD %q fraction %v", got, tt.spend, tt.want)
			}
			if err == nil && st.gotNow.Day() != 1 {
				t.Error("the adapter did not pass its clock to the store")
			}
		})
	}
}

func TestSystemInfoHasNoSecrets(t *testing.T) {
	cfg := config.Config{
		LLMProvider: "anthropic", AnthropicAPIKey: config.NewSecret("sk-ant-PLANTED"),
		LLMDailyBudgetUSD: 3, ERPBaseURL: "https://u:p@erp.example.test/x?y=z",
	}
	opts, err := newSystemOptions(cfg, &fakeSystemStore{})
	if err != nil {
		t.Fatal(err)
	}
	want := web.SystemInfo{Provider: "anthropic", KeyPresent: true, BudgetUSD: 3, ERPHost: "erp.example.test"}
	if opts.Info != want {
		t.Errorf("info %+v, want %+v", opts.Info, want)
	}
}

// TestServeSystemPage wires the adapter through newServeServer and reads
// /system as a signed-in viewer.
func TestServeSystemPage(t *testing.T) {
	e := newERPStandIn(t, http.StatusOK)
	cfg := serveConfig()
	cfg.ERPBaseURL, cfg.LLMProvider, cfg.LLMDailyBudgetUSD = e.URL, "claude-cli", 5
	sys, err := newSystemOptions(cfg, &fakeSystemStore{spend: "5.100000"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newServeServer(cfg, serveUsers(t), serveBackends{system: sys}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j := jar{}
	w := j.do(t, srv, http.MethodGet, "/login", nil)
	m := serveCSRF.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("no csrf field on /login")
	}
	j.do(t, srv, http.MethodPost, "/login", url.Values{"csrf_token": {m[1]}, "username": {"demo"}, "password": {servePassword}})
	w = j.do(t, srv, http.MethodGet, "/system", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /system = %d", w.Code)
	}
	for _, want := range []string{"Postgres", "ERPNext is reachable", "$5.10", "Budget reached"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if w := (jar{}).do(t, srv, http.MethodGet, "/system", nil); w.Code != http.StatusSeeOther {
		t.Errorf("anonymous GET /system = %d", w.Code)
	}
}
