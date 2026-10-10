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
	for _, s := range []string{"--suite", "-no-agent", "-no-llm", "-record", "-replay", "-fixtures-dir", `"agent": false`, `"agent": true`,
		"DATABASE_URL only", "COPILOT_FAULT", "make record-fixtures", "baseline-update", "erp-reset seed load"} {
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

func TestRunArgs(t *testing.T) {
	var out bytes.Buffer
	for _, tt := range []struct {
		args                    []string
		noAgent, record, replay bool
		fixtures                string
	}{
		{[]string{"run", "--suite", "s"}, false, false, false, ""},
		{[]string{"run", "--suite", "s", "--no-llm"}, true, false, false, ""},
		{[]string{"run", "--suite", "s", "--no-agent"}, true, false, false, ""},
		{[]string{"run", "--suite", "s", "--replay", "--no-llm"}, true, false, true, "evals/fixtures"},
		{[]string{"run", "--suite", "s", "--record", "--fixtures-dir", "/tmp/fx"}, false, true, false, "/tmp/fx"},
		{[]string{"run", "--suite", "s", "--replay=true"}, false, false, true, "evals/fixtures"},
		{[]string{"run", "--suite", "s", "--fixtures-dir", "/tmp/ignored"}, false, false, false, ""},
	} {
		c, err := parseArgs(tt.args, &out)
		if err != nil {
			t.Errorf("%q: %v", tt.args, err)
			continue
		}
		if c.flags.NoAgent != tt.noAgent || c.flags.Record != tt.record || c.flags.Replay != tt.replay || c.flags.FixturesDir != tt.fixtures {
			t.Errorf("%q parsed %+v", tt.args, c.flags)
		}
	}
	for _, args := range [][]string{
		{"run", "--suite", "s", "--record", "--replay"},
		{"run", "--suite", "s", "--replay", "--fixtures-dir", ""},
		{"run", "--suite", "s", "--replay=maybe"},
	} {
		if _, err := parseArgs(args, &out); err == nil {
			t.Errorf("parseArgs(%q) accepted it", args)
		}
	}
	// The manifest records replay, record and the fixtures dir, and only
	// for a fixture run.
	c, _ := parseArgs([]string{"run", "--suite", "s", "--replay", "--no-llm"}, &out)
	b, err := json.Marshal(evals.Manifest{Flags: c.flags, FixturesIndexSHA256: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"replay":true`, `"fixtures_dir":"evals/fixtures"`, `"fixtures_index_sha256":"abc"`, `"no_agent":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("manifest %s lacks %s", b, want)
		}
	}
	live, _ := parseArgs([]string{"run", "--suite", "s"}, &out)
	if b, _ := json.Marshal(evals.Manifest{Flags: live.flags}); strings.Contains(string(b), "fixtures") || strings.Contains(string(b), "replay") {
		t.Errorf("live manifest %s mentions fixtures", b)
	}
}

func TestRequiredFor(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want []string
	}{
		{[]string{"run", "--suite", "s"}, required},
		{[]string{"run", "--suite", "s", "--record"}, required},
		{[]string{"run", "--suite", "s", "--replay", "--no-llm"}, []string{config.EnvDatabaseURL}},
		{[]string{"-v", "run", "--replay", "--suite", "s"}, []string{config.EnvDatabaseURL}},
		{[]string{"run", "-replay=true", "--suite", "s"}, []string{config.EnvDatabaseURL}},
		{[]string{"run", "--replay=false", "--suite", "s"}, required},
		{[]string{"run", "--replay", "--record", "--suite", "s"}, nil}, // a usage error: stops before any I/O
		{[]string{"run", "-h"}, nil},
		{[]string{"score", "x"}, nil},
		{[]string{"noise", "a", "b"}, nil},
	} {
		if got := requiredFor(tt.args); !slices.Equal(got, tt.want) {
			t.Errorf("requiredFor(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
	// A replay never asks for an MCP URL or token.
	for _, v := range requiredFor([]string{"run", "--suite", "s", "--replay"}) {
		if v != config.EnvDatabaseURL {
			t.Errorf("replay requires %s", v)
		}
	}
}

// TestRunRefusesFault checks that eval run never starts with
// COPILOT_FAULT set, in any mode.
func TestRunRefusesFault(t *testing.T) {
	for _, mode := range [][]string{nil, {"--replay"}, {"--record"}, {"--no-llm"}} {
		args := append([]string{"run", "--suite", "s"}, mode...)
		err := run(t.Context(), config.Config{Fault: config.FaultCorruptExplanation}, nil, args)
		if err == nil || !strings.Contains(err.Error(), config.EnvCopilotFault) {
			t.Errorf("run %q with a fault = %v", args, err)
		}
	}
}
