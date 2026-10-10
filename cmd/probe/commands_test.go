package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestAuth(t *testing.T) {
	srv := newFake(t).start()

	r := runProbe(t, testConfig(srv), "auth")
	if r.err != nil {
		t.Fatalf("auth: %v", r.err)
	}
	if r.stdout != "PASS auth: the bot key logs in as copilot-bot@example.com\n" {
		t.Errorf("stdout = %q", r.stdout)
	}

	// The seeder's key in the bot's place logs in as Administrator.
	cfg := testConfig(srv)
	cfg.ERPAPIKey, cfg.ERPAPISecret = config.NewSecret(testSeedKey), config.NewSecret(testSeedSecret)
	r = runProbe(t, cfg, "auth")
	if r.err == nil || !strings.Contains(r.err.Error(), `"Administrator"`) {
		t.Errorf("want an error naming Administrator, got %v", r.err)
	}
	if !strings.HasPrefix(r.stdout, "FAIL auth:") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestAuthBadSecretNeverLeaks(t *testing.T) {
	f := newFake(t)
	f.echoAuthInErrors = true
	srv := f.start()
	cfg := testConfig(srv)
	cfg.ERPAPISecret = config.NewSecret(testBotSecret + "-wrong")

	r := runProbe(t, cfg, "auth") // runProbe asserts no secret in stdout, logs or error
	if r.err == nil || !strings.Contains(r.err.Error(), "401") || !strings.Contains(r.err.Error(), "AuthenticationError") {
		t.Errorf("want a 401 AuthenticationError, got %v", r.err)
	}
	if strings.Contains(r.err.Error(), "-wrong") {
		t.Errorf("error leaks the wrong secret: %v", r.err)
	}
}

func TestPermsPass(t *testing.T) {
	f := newFake(t)
	srv := f.start()

	r := runProbe(t, testConfig(srv), "perms")
	if r.err != nil {
		t.Fatalf("perms: %v\n%s", r.err, r.stdout)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	wantPrefixes := []string{
		"PASS auth:",
		"PASS (a) read GL Entry:",
		"PASS (a) read Purchase Invoice:",
		"PASS (a) read Payment Entry:",
		"PASS (b) create Journal Entry:",
		"PASS (b) submit Journal Entry:",
		"PASS (c) refused read of System Settings:",
		"PASS (d) refused write to System Settings:",
	}
	if len(lines) != len(wantPrefixes) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(wantPrefixes), r.stdout)
	}
	for i, p := range wantPrefixes {
		if !strings.HasPrefix(lines[i], p) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], p)
		}
	}

	// Nothing is created: no POST at all, and the only PUT is the refused
	// System Settings write. Journal Entry rights come from has_permission.
	var perms []string
	for _, req := range f.seen() {
		switch {
		case req.Method == http.MethodPost:
			t.Errorf("perms sent POST %s", req.Path)
		case req.Method == http.MethodPut && req.Path != "/api/resource/System Settings/System Settings":
			t.Errorf("perms sent PUT %s", req.Path)
		case req.Path == "/api/method/frappe.client.has_permission":
			perms = append(perms, req.Query)
		}
	}
	if len(perms) != 2 || !strings.Contains(perms[0], "perm_type=create") || !strings.Contains(perms[1], "perm_type=submit") ||
		!strings.Contains(perms[0], "doctype=Journal+Entry") || !strings.Contains(perms[0], "docname=&") {
		t.Errorf("has_permission queries = %q", perms)
	}
}

func TestPermsFailures(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*fakeERP)
		wantFail string
	}{
		{"no submit right", func(f *fakeERP) { f.jeDenied["submit"] = true }, "FAIL (b) submit Journal Entry"},
		{"no create right", func(f *fakeERP) { f.jeDenied["create"] = true }, "FAIL (b) create Journal Entry"},
		{"System Settings readable", func(f *fakeERP) { f.settingsReadable = true }, "FAIL (c) refused read of System Settings"},
		{"System Settings writable", func(f *fakeERP) { f.settingsWritable = true }, "FAIL (d) refused write to System Settings"},
		{"401 is not a refusal", func(f *fakeERP) { f.settingsAuthFails = true }, "FAIL (c) refused read of System Settings: GET /api/resource/System%20Settings/System%20Settings failed but was not a permission refusal"},
		{"bare 403 is not a refusal", func(f *fakeERP) { f.settingsBare403 = true }, "FAIL (c) refused read of System Settings: GET /api/resource/System%20Settings/System%20Settings failed but was not a permission refusal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.echoAuthInErrors = true
			tt.setup(f)
			r := runProbe(t, testConfig(f.start()), "perms")
			if r.err == nil {
				t.Fatalf("want an error; stdout:\n%s", r.stdout)
			}
			if !strings.Contains(r.stdout, tt.wantFail) {
				t.Errorf("stdout lacks %q:\n%s", tt.wantFail, r.stdout)
			}
			if got := strings.Count(r.stdout, "\n"); got != 8 {
				t.Errorf("want all 8 lines even after a failure, got %d", got)
			}
			// Check (d), the System Settings write, runs only when (c) was
			// refused.
			cFailed := strings.Contains(r.stdout, "FAIL (c)")
			if skipped := strings.Contains(r.stdout, "SKIP (d) refused write to System Settings: not run because check (c) did not pass"); skipped != cFailed {
				t.Errorf("(d) skipped = %v, want %v:\n%s", skipped, cFailed, r.stdout)
			}
			put := false
			for _, req := range f.seen() {
				put = put || req.Method == http.MethodPut
			}
			if put == cFailed {
				t.Errorf("PUT sent = %v with check (c) failed = %v; (d) must run exactly when (c) passed", put, cFailed)
			}
		})
	}
}

func TestPermsRefusesOtherUsersKey(t *testing.T) {
	f := newFake(t)
	cfg := testConfig(f.start())
	cfg.ERPAPIKey, cfg.ERPAPISecret = config.NewSecret(testSeedKey), config.NewSecret(testSeedSecret)
	r := runProbe(t, cfg, "perms")
	if r.err == nil || !strings.Contains(r.err.Error(), "not running the checks") {
		t.Fatalf("want a refusal to run, got %v", r.err)
	}
	for _, req := range f.seen() {
		if req.Method != http.MethodGet || req.Path != "/api/method/frappe.auth.get_logged_user" {
			t.Errorf("unexpected request %s %s with a non-bot key", req.Method, req.Path)
		}
	}
}

func TestSchemaUsesSeederWhenBotRefused(t *testing.T) {
	old := customFieldPageSize
	customFieldPageSize = 2
	t.Cleanup(func() { customFieldPageSize = old })

	f := newFake(t)
	f.journalCustomCount = 5 // three pages of 2
	cfg := testConfig(f.start())

	dir1 := filepath.Join(t.TempDir(), "run1")
	r := runProbe(t, cfg, "schema", "--out", dir1)
	if r.err != nil {
		t.Fatalf("schema: %v", r.err)
	}
	if !strings.Contains(r.logs, "using the seeder key for schema only") {
		t.Errorf("logs do not say the seeder key was used: %s", r.logs)
	}

	// Every meta request after the bot's two probes used the seeder key.
	reqs := f.seen()
	if len(reqs) < 3 {
		t.Fatalf("too few requests: %d", len(reqs))
	}
	for i, req := range reqs {
		bot := strings.HasPrefix(req.Auth, "token "+testBotKey+":")
		if (i < 2) != bot {
			t.Errorf("request %d %s used the wrong key (bot=%v)", i, req.Path, bot)
		}
	}

	// One file per DocType, kebab-named.
	entries, err := os.ReadDir(dir1)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != len(schemaDocTypes) || !slices.Contains(names, "purchase-taxes-and-charges.json") || !slices.Contains(names, "gl-entry.json") {
		t.Errorf("files = %v", names)
	}

	// The recorded Purchase Invoice fixture renders to the golden file:
	// sorted by fieldname, custom fields marked, nulls as "".
	got, err := os.ReadFile(filepath.Join(dir1, "purchase-invoice.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readFixture(t, "purchase-invoice.golden.json"); !bytes.Equal(got, want) {
		t.Errorf("purchase-invoice.json differs from the golden file:\n%s", got)
	}

	// Paging collected all five Journal Entry custom fields.
	je, err := os.ReadFile(filepath.Join(dir1, "journal-entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(je), `"custom": true`); n != 5 {
		t.Errorf("journal-entry.json has %d custom fields, want 5", n)
	}

	// A second run gives identical bytes.
	dir2 := filepath.Join(t.TempDir(), "run2")
	if r := runProbe(t, cfg, "schema", "--out", dir2); r.err != nil {
		t.Fatal(r.err)
	}
	for _, n := range names {
		a, _ := os.ReadFile(filepath.Join(dir1, n))
		b, _ := os.ReadFile(filepath.Join(dir2, n))
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs between runs", n)
		}
		if !bytes.HasSuffix(a, []byte("}\n")) {
			t.Errorf("%s has no trailing newline", n)
		}
	}
}

func TestSchemaWithoutSeederKey(t *testing.T) {
	f := newFake(t)
	cfg := testConfig(f.start())
	cfg.ERPSeedAPIKey, cfg.ERPSeedAPISecret = config.Secret{}, config.Secret{}
	out := filepath.Join(t.TempDir(), "out")

	r := runProbe(t, cfg, "schema", "--out", out)
	if r.err == nil || !strings.Contains(r.err.Error(), config.EnvERPSeedAPIKey) {
		t.Fatalf("want an error naming %s, got %v", config.EnvERPSeedAPIKey, r.err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("out dir was created: %v", err)
	}
}

func TestSchemaUsesBotWhenAllowed(t *testing.T) {
	f := newFake(t)
	f.botCanReadMeta = true
	cfg := testConfig(f.start())
	if r := runProbe(t, cfg, "schema", "--out", t.TempDir()); r.err != nil {
		t.Fatal(r.err)
	}
	for _, req := range f.seen() {
		if !strings.HasPrefix(req.Auth, "token "+testBotKey+":") {
			t.Fatalf("%s used a key other than the bot's", req.Path)
		}
	}
}

func TestSchemaEchoedErrorsNeverLeak(t *testing.T) {
	f := newFake(t)
	f.echoAuthInErrors = true
	cfg := testConfig(f.start())
	cfg.ERPSeedAPISecret = config.NewSecret(testSeedSecret + "-wrong") // the seeder gets 401s quoting its header
	r := runProbe(t, cfg, "schema", "--out", t.TempDir())
	if r.err == nil || !strings.Contains(r.err.Error(), "401") {
		t.Fatalf("want a 401, got %v", r.err)
	}
}

func TestSchemaArgs(t *testing.T) {
	cfg := testConfig(newFake(t).start())
	for _, args := range [][]string{{"schema"}, {"schema", "--out"}, {"schema", "--out", "x", "extra"}, {"schema", "--bogus"}} {
		if r := runProbe(t, cfg, args...); r.err == nil {
			t.Errorf("args %q: want an error", args)
		}
	}
}

func TestKebab(t *testing.T) {
	for in, want := range map[string]string{
		"GL Entry":                   "gl-entry",
		"Purchase Taxes and Charges": "purchase-taxes-and-charges",
		"Company":                    "company",
	} {
		if got := kebab(in); got != want {
			t.Errorf("kebab(%q) = %q, want %q", in, got, want)
		}
	}
}
