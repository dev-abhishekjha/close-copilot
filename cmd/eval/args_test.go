package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
)

func TestParseArgs(t *testing.T) {
	var out bytes.Buffer
	c, err := parseArgs([]string{"run", "--suite", "suite-v1", "--only", "sharma:2026-09,mehta:2026-08", "--only", "sharma:2026-06",
		"--model-fast", "claude-haiku-x", "--no-agent", "--results-dir", "/tmp/r", "--scenarios-dir", "s", "--config-dir", "c"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := evals.Flags{Suite: "suite-v1", Only: []string{"sharma:2026-09", "mehta:2026-08", "sharma:2026-06"},
		ModelFast: "claude-haiku-x", NoAgent: true, ResultsDir: "/tmp/r", ScenariosDir: "s", ConfigDir: "c"}
	if c.name != cmdRun || c.flags.Suite != want.Suite || !slices.Equal(c.flags.Only, want.Only) || c.flags.ModelFast != want.ModelFast ||
		!c.flags.NoAgent || c.flags.ResultsDir != want.ResultsDir || c.flags.ScenariosDir != want.ScenariosDir || c.flags.ConfigDir != want.ConfigDir {
		t.Errorf("parsed %+v, want %+v", c.flags, want)
	}

	d, err := parseArgs([]string{"run", "--suite", "suite-skeleton"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if d.flags.ResultsDir != "results" || d.flags.ScenariosDir != "evals/scenarios" || d.flags.ConfigDir != "config" || d.flags.NoAgent {
		t.Errorf("defaults %+v", d.flags)
	}

	for _, args := range [][]string{
		nil,
		{"score"},
		{"run"},
		{"run", "--suite", "s", "extra"},
		{"run", "--suite", "s", "--only", "sharma"},
		{"run", "--suite", "s", "--only", "sharma:2026-9"},
		{"run", "--suite", "s", "--only", "Sharma:2026-09"},
		{"run", "--suite", "s", "--results-dir", ""},
		{"run", "--suite", "s", "--bogus"},
	} {
		if _, err := parseArgs(args, &out); err == nil {
			t.Errorf("parseArgs(%q) accepted it", args)
		}
	}
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	_, err := parseArgs([]string{"run", "-h"}, &out)
	if !errors.Is(err, errHelp) {
		t.Fatalf("-h = %v, want errHelp", err)
	}
	for _, s := range []string{"--suite", "-no-agent", `"agent": false`, "no explainer or verifier", "calls no model"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("help text lacks %q:\n%s", s, out.String())
		}
	}
	old := stderr
	stderr = &bytes.Buffer{}
	defer func() { stderr = old }()
	if err := run(t.Context(), config.Config{}, nil, []string{"run", "-h"}); err != nil {
		t.Errorf("run -h = %v, want nil", err)
	}
}

// TestManifestHasNoSecrets fills every secret variable and checks that
// neither the manifest config nor a whole manifest carries any of them.
func TestManifestHasNoSecrets(t *testing.T) {
	secrets := map[string]string{
		config.EnvERPAPIKey:        "sekret-erp-key",
		config.EnvERPAPISecret:     "sekret-erp-secret",
		config.EnvERPSeedAPIKey:    "sekret-seed-key",
		config.EnvERPSeedAPISecret: "sekret-seed-secret",
		config.EnvDatabaseURL:      "postgres://copilot:sekret-db-pass@db:5432/c",
		config.EnvMCPTokenAgent:    "sekret-agent-token",
		config.EnvMCPTokenAdmin:    "sekret-admin-token",
		config.EnvMCPScopeKey:      "sekret-scope",
		config.EnvAnthropicAPIKey:  "sekret-anthropic",
		config.EnvPseudonymKey:     "sekret-pseudonym",
		config.EnvOTLPHeaders:      "authorization=sekret-otlp",
		config.EnvAppSessionKey:    "sekret-session",
	}
	env := map[string]string{
		config.EnvBooksMCPURL:    "http://u:sekret-url-pass@books/mcp?token=sekret-url-token",
		config.EnvEvidenceMCPURL: "http://evidence/mcp",
		config.EnvLLMModelFast:   "haiku",
	}
	for k, v := range secrets {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	mc := manifestConfig(cfg)
	if mc.LLMModelFast != "haiku" || mc.LLMModelStrong != "sonnet" || mc.LLMProvider != "claude-cli" || mc.LLMDailyBudgetUSD != "2" || mc.LLMRunTokenCap != 200000 {
		t.Errorf("manifest config %+v", mc)
	}
	b, err := json.Marshal(evals.Manifest{Config: mc, Flags: evals.Flags{Suite: "s"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sekret") || strings.Contains(string(b), "redacted") {
		t.Errorf("manifest carries a secret or a secret's mask: %s", b)
	}
}
