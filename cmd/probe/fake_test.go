package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// Test credentials. The secrets are distinctive so a leak is easy to spot.
const (
	testBotKey     = "botkey0000000ab"
	testBotSecret  = "BOTSECRET-must-never-leak"
	testSeedKey    = "seedkey00000000"
	testSeedSecret = "SEEDSECRET-must-never-leak"
	testSite       = "erp.localhost"
)

// request is what the fake saw.
type request struct {
	Method, Path, Query, Auth, Accept, Host, ContentType string
}

// fakeERP is an httptest Frappe v15 with just the endpoints probe calls.
// Its zero behaviour matches the real site: the bot (Accounts User) may
// list the three ledger DocTypes, holds Journal Entry create and submit,
// and is refused on System Settings, DocType and Custom Field.
type fakeERP struct {
	t *testing.T

	botCanReadMeta     bool
	jeDenied           map[string]bool // perm_type the bot lacks on Journal Entry
	settingsReadable   bool
	settingsWritable   bool
	settingsAuthFails  bool // System Settings answers 401, which is not a refusal
	settingsBare403    bool // System Settings answers a proxy's bare 403, which is not a refusal
	echoAuthInErrors   bool // error bodies quote the Authorization header
	journalCustomCount int  // custom fields on Journal Entry, to test paging

	mu       sync.Mutex
	requests []request
}

func newFake(t *testing.T) *fakeERP {
	t.Helper()
	return &fakeERP{t: t, jeDenied: map[string]bool{}}
}

func (f *fakeERP) start() *httptest.Server {
	srv := httptest.NewServer(f)
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakeERP) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func (f *fakeERP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.requests = append(f.requests, request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: auth,
		Accept: r.Header.Get("Accept"), Host: r.Host, ContentType: r.Header.Get("Content-Type"),
	})
	f.mu.Unlock()

	var user string
	switch auth {
	case "token " + testBotKey + ":" + testBotSecret:
		user = botUser
	case "token " + testSeedKey + ":" + testSeedSecret:
		user = "Administrator"
	default:
		f.fail(w, http.StatusUnauthorized, "AuthenticationError", "Invalid credentials", auth)
		return
	}
	admin := user == "Administrator"
	q := r.URL.Query()

	switch p := r.URL.Path; {
	case p == "/api/method/frappe.auth.get_logged_user":
		f.reply(w, map[string]any{"message": user})

	case p == "/api/method/frappe.client.has_permission":
		if _, ok := q["docname"]; !ok {
			f.fail(w, http.StatusExpectationFailed, "TypeError", "has_permission() missing 1 required positional argument: 'docname'", auth)
			return
		}
		allowed := admin || (q.Get("doctype") == "Journal Entry" && !f.jeDenied[q.Get("perm_type")])
		f.reply(w, map[string]any{"message": map[string]any{"has_permission": allowed}})

	case p == "/api/resource/GL Entry", p == "/api/resource/Purchase Invoice", p == "/api/resource/Payment Entry":
		f.reply(w, map[string]any{"data": []any{}})

	case p == "/api/resource/System Settings/System Settings":
		switch {
		case f.settingsAuthFails:
			f.fail(w, http.StatusUnauthorized, "AuthenticationError", "session expired", auth)
		case f.settingsBare403:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><body>403 Forbidden</body></html>"))
		case admin, r.Method == http.MethodGet && f.settingsReadable, r.Method == http.MethodPut && f.settingsWritable:
			f.reply(w, map[string]any{"data": map[string]any{"name": "System Settings"}})
		default:
			f.fail(w, http.StatusForbidden, "PermissionError", "Insufficient Permission for System Settings", auth)
		}

	case p == "/api/resource/DocType", strings.HasPrefix(p, "/api/resource/DocType/"):
		if !admin && !f.botCanReadMeta {
			f.fail(w, http.StatusForbidden, "PermissionError", "Insufficient Permission for DocType", auth)
			return
		}
		if p == "/api/resource/DocType" {
			f.reply(w, map[string]any{"data": []any{map[string]any{"name": "Account"}}})
			return
		}
		f.docType(w, strings.TrimPrefix(p, "/api/resource/DocType/"))

	case p == "/api/resource/Custom Field":
		if !admin && !f.botCanReadMeta {
			f.fail(w, http.StatusForbidden, "PermissionError", "Insufficient Permission for Custom Field", auth)
			return
		}
		f.customFields(w, q)

	default:
		f.fail(w, http.StatusNotFound, "DoesNotExistError", "no route "+p, auth)
	}
}

func (f *fakeERP) docType(w http.ResponseWriter, name string) {
	if name == "Purchase Invoice" {
		f.raw(w, readFixture(f.t, "doctype-purchase-invoice.json"))
		return
	}
	istable := 0
	if strings.HasSuffix(name, "Account") || strings.HasSuffix(name, "Reference") || strings.HasSuffix(name, "Charges") {
		istable = 1
	}
	f.reply(w, map[string]any{"data": map[string]any{
		"name": name, "module": "Accounts", "istable": istable, "is_submittable": 0,
		"fields": []any{
			map[string]any{"fieldname": "zeta", "fieldtype": "Data", "label": "Zeta", "reqd": 0, "read_only": 0, "hidden": 1},
			map[string]any{"fieldname": "alpha", "fieldtype": "Link", "label": "Alpha", "options": "Company", "reqd": 1, "read_only": 0, "hidden": 0},
		},
	}})
}

func (f *fakeERP) customFields(w http.ResponseWriter, q map[string][]string) {
	if first(q["filters"]) == "" { // the read-access probe
		f.reply(w, map[string]any{"data": []any{map[string]any{"name": "Purchase Invoice-supplier_gstin"}}})
		return
	}
	var filters [][]string
	if err := json.Unmarshal([]byte(first(q["filters"])), &filters); err != nil || len(filters) != 1 ||
		len(filters[0]) != 3 || filters[0][0] != "dt" || filters[0][1] != "=" {
		f.fail(w, http.StatusExpectationFailed, "ValidationError", "bad filters", "")
		return
	}
	dt := filters[0][2]
	var rows []any
	switch dt {
	case "Purchase Invoice":
		var fixture struct{ Data []any }
		if err := json.Unmarshal(readFixture(f.t, "custom-field-purchase-invoice.json"), &fixture); err != nil {
			f.t.Errorf("fake: %v", err)
		}
		rows = fixture.Data
	case "Journal Entry":
		for i := range f.journalCustomCount {
			rows = append(rows, map[string]any{
				"fieldname": fmt.Sprintf("custom_%02d", i), "fieldtype": "Data", "label": nil,
				"options": nil, "reqd": 0, "read_only": 0, "hidden": 0,
			})
		}
	}
	start, _ := strconv.Atoi(first(q["limit_start"]))
	size, _ := strconv.Atoi(first(q["limit_page_length"]))
	end := min(start+size, len(rows))
	page := []any{}
	if start < len(rows) {
		page = rows[start:end]
	}
	f.reply(w, map[string]any{"data": page})
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (f *fakeERP) reply(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("fake: %v", err)
	}
	f.raw(w, b)
}

func (f *fakeERP) raw(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// fail writes a Frappe-style error body, quoting the Authorization header
// when echoAuthInErrors is set.
func (f *fakeERP) fail(w http.ResponseWriter, status int, excType, msg, auth string) {
	if f.echoAuthInErrors {
		msg += " (header: " + auth + ")"
	}
	inner, _ := json.Marshal(map[string]string{"message": msg})
	server, _ := json.Marshal([]string{string(inner)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"exception":        "frappe.exceptions." + excType + ": " + msg,
		"exc_type":         excType,
		"exc":              `["Traceback (most recent call last): ... ` + msg + `"]`,
		"_server_messages": string(server),
	})
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Errorf("fixture: %v", err) // not Fatal: the fake calls it from its own goroutine
	}
	return b
}

func testConfig(srv *httptest.Server) config.Config {
	return config.Config{
		ERPBaseURL:       srv.URL,
		ERPSite:          testSite,
		ERPAPIKey:        config.NewSecret(testBotKey),
		ERPAPISecret:     config.NewSecret(testBotSecret),
		ERPSeedAPIKey:    config.NewSecret(testSeedKey),
		ERPSeedAPISecret: config.NewSecret(testSeedSecret),
	}
}

// result is one dispatch run's output.
type result struct {
	stdout, logs string
	err          error
}

func (r result) all() string {
	s := r.stdout + r.logs
	if r.err != nil {
		s += r.err.Error()
	}
	return s
}

func runProbe(t *testing.T, cfg config.Config, args ...string) result {
	t.Helper()
	var stdout, logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	err := dispatch(context.Background(), cfg, log, args, &stdout)
	r := result{stdout: stdout.String(), logs: logs.String(), err: err}
	assertNoSecrets(t, r.all())
	return r
}

// assertNoSecrets fails if s holds either secret.
func assertNoSecrets(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{testBotSecret, testSeedSecret} {
		if strings.Contains(s, secret) {
			t.Errorf("output leaks a secret: %q", s)
		}
	}
}
