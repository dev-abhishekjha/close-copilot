package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

var configDir = filepath.Join("..", "..", "config", "companies")

// emptyEnv loads the config from an environment with nothing set.
func emptyEnv(required ...string) (config.Config, error) {
	return config.Load(func(string) (string, bool) { return "", false }, required...)
}

func runSeed(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, emptyEnv, requiredEnv(args), newRun(&out))
	return code, out.String(), errOut.String()
}

func TestWorldRunsWithoutERPEnv(t *testing.T) {
	code, out, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("output doesn't end in a closing brace and a newline")
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var w seed.World
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("decode world: %v", err)
	}
	if w.Company != "sharma" || w.Month != "2026-09" || len(w.Events) == 0 {
		t.Errorf("world %s %s with %d events", w.Company, w.Month, len(w.Events))
	}

	p, err := seed.LoadProfile(filepath.Join(configDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := seed.Generate(p, "2026-09", seed.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, want) {
		t.Error("printed world doesn't decode to Generate's world")
	}
	golden, err := os.ReadFile(filepath.Join("..", "..", "internal", "seed", "testdata", "world-sharma-2026-09.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(golden) {
		t.Error("printed world differs from the golden file")
	}
}

func TestWorldSmall(t *testing.T) {
	_, full, _ := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	code, small, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--small", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if len(small) >= len(full) {
		t.Errorf("--small printed %d bytes, full %d", len(small), len(full))
	}
}

func TestWorldErrors(t *testing.T) {
	cases := [][]string{
		{"world"},
		{"world", "--company", "sharma"},
		{"world", "--company", "sharma", "--month", "2026-13", "--config", configDir},
		{"world", "--company", "nobody", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "../sharma", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "sharma", "--month", "2026-09", "--config", configDir, "extra"},
		{"world", "--bogus"},
	}
	for _, args := range cases {
		code, out, _ := runSeed(t, args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		if out != "" {
			t.Errorf("%v: printed %q on failure", args, out)
		}
	}
}

func TestRequiredEnv(t *testing.T) {
	if got := requiredEnv([]string{"world", "--company", "sharma"}); len(got) != 0 {
		t.Errorf("world requires %v, want nothing", got)
	}
	for _, args := range [][]string{nil, {"books"}, {"all"}, {"--version"}} {
		got := requiredEnv(args)
		if !slices.Equal(got, erpEnv) {
			t.Errorf("%v requires %v, want %v", args, got, erpEnv)
		}
	}
	// Other subcommands still need the ERPNext variables.
	if code, _, errOut := runSeed(t, "books"); code != 1 || !strings.Contains(errOut, config.EnvERPBaseURL) {
		t.Errorf("books with no env: exit %d, stderr %q", code, errOut)
	}
}

// erpTestEnv loads a config with the four ERP seed variables set.
func erpTestEnv(required ...string) (config.Config, error) {
	env := map[string]string{
		config.EnvERPBaseURL:       "http://localhost:8080",
		config.EnvERPSite:          "erp.localhost",
		config.EnvERPSeedAPIKey:    "seed-key",
		config.EnvERPSeedAPISecret: "seed-secret",
	}
	return config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, required...)
}

// fakeBootstrap returns a bootstrap that records the profile it got and
// reports changes created records.
func fakeBootstrap(changes int, got *seed.Profile) bootstrapFunc {
	return func(_ context.Context, c *frappe.Client, p seed.Profile) (seed.BootstrapReport, error) {
		if c == nil {
			return seed.BootstrapReport{}, errors.New("nil client")
		}
		*got = p
		return seed.BootstrapReport{
			Company: p.ERPCompany,
			Abbr:    p.Abbr,
			Counts:  map[string]*seed.Counts{"Customer": {Created: changes, Unchanged: p.Customers - changes}},
		}, nil
	}
}

func runBootstrapCmd(t *testing.T, bootstrap bootstrapFunc, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, erpTestEnv, requiredEnv(args), newRunWith(&out, bootstrap))
	return code, out.String(), errOut.String()
}

func TestBootstrapPrintsReport(t *testing.T) {
	var got seed.Profile
	code, out, errOut := runBootstrapCmd(t, fakeBootstrap(3, &got), "bootstrap", "--company", "sharma", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if got.ID != "sharma" || got.ERPCompany != "Sharma Traders Pvt Ltd" {
		t.Errorf("bootstrap got profile %q (%q)", got.ID, got.ERPCompany)
	}
	var rep seed.BootstrapReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not a report: %v\n%s", err, out)
	}
	if rep.Company != "Sharma Traders Pvt Ltd" || rep.Changes() != 3 {
		t.Errorf("report %+v", rep)
	}
	if strings.Contains(out+errOut, "seed-secret") {
		t.Error("the secret was printed")
	}
}

func TestBootstrapExpectNoChanges(t *testing.T) {
	var got seed.Profile
	code, out, errOut := runBootstrapCmd(t, fakeBootstrap(0, &got), "bootstrap", "--company", "sharma", "--config", configDir, "--expect-no-changes")
	if code != 0 {
		t.Fatalf("no changes: exit %d, stderr:\n%s", code, errOut)
	}
	if out == "" {
		t.Error("no report printed")
	}
	code, out, errOut = runBootstrapCmd(t, fakeBootstrap(2, &got), "bootstrap", "--company", "sharma", "--config", configDir, "--expect-no-changes")
	if code != 1 {
		t.Fatalf("changes: exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "2 records were created or updated") {
		t.Errorf("stderr %q", errOut)
	}
	if out == "" {
		t.Error("the report should still be printed")
	}
}

func TestBootstrapErrors(t *testing.T) {
	failing := func(context.Context, *frappe.Client, seed.Profile) (seed.BootstrapReport, error) {
		return seed.BootstrapReport{}, errors.New("boom")
	}
	var got seed.Profile
	cases := []struct {
		name string
		fn   bootstrapFunc
		args []string
	}{
		{"no company", fakeBootstrap(0, &got), []string{"bootstrap", "--config", configDir}},
		{"unknown company", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "nobody", "--config", configDir}},
		{"path in company", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "../sharma", "--config", configDir}},
		{"extra argument", fakeBootstrap(0, &got), []string{"bootstrap", "--company", "sharma", "--config", configDir, "extra"}},
		{"bad flag", fakeBootstrap(0, &got), []string{"bootstrap", "--bogus"}},
		{"bootstrap fails", failing, []string{"bootstrap", "--company", "sharma", "--config", configDir}},
	}
	for _, tc := range cases {
		code, out, _ := runBootstrapCmd(t, tc.fn, tc.args...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", tc.name, code)
		}
		if out != "" {
			t.Errorf("%s: printed %q on failure", tc.name, out)
		}
	}
}

func TestBootstrapNeedsERPEnv(t *testing.T) {
	if got := requiredEnv([]string{"bootstrap", "--company", "sharma"}); !slices.Equal(got, erpEnv) {
		t.Errorf("bootstrap requires %v, want %v", got, erpEnv)
	}
	code, _, errOut := runSeed(t, "bootstrap", "--company", "sharma", "--config", configDir)
	if code != 1 || !strings.Contains(errOut, config.EnvERPSeedAPIKey) {
		t.Errorf("bootstrap with no env: exit %d, stderr %q", code, errOut)
	}
}
