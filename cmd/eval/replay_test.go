package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
)

// runCLIEnv runs the eval entry point with exactly env as the environment.
func runCLIEnv(t *testing.T, env map[string]string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &o, &e
	defer func() { stdout, stderr = oldOut, oldErr }()
	load := func(req ...string) (config.Config, error) {
		return config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, req...)
	}
	code = cli.Run("eval", args, &o, &e, load, requiredFor(args), run)
	return code, o.String(), e.String()
}

// replaySuite writes a suite with its ground truth under a temp scenarios
// folder and returns the folder.
func replaySuite(t *testing.T) string {
	t.Helper()
	scen := t.TempDir()
	body := "suite: suite-rp\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n"
	if err := os.WriteFile(filepath.Join(scen, "suite-rp.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	truth := filepath.Join(scen, "suite-rp", "ground_truth")
	if err := os.MkdirAll(truth, 0o750); err != nil {
		t.Fatal(err)
	}
	for m, clean := range map[string]string{"2026-09": "false", "2026-08": "true"} {
		gt := `{"scenario":"suite-rp","company":"sharma","month":"` + m + `","clean":` + clean + `,"planted":[],"expected":[],"investigations":[]}`
		if err := os.WriteFile(filepath.Join(truth, "sharma-"+m+".json"), []byte(gt), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return scen
}

// TestReplayCLI checks eval run --replay up to the database: it needs
// DATABASE_URL only, checks the fixtures before anything else, and never
// asks for an MCP URL or token.
func TestReplayCLI(t *testing.T) {
	scen := replaySuite(t)
	configDir, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := filepath.Join(t.TempDir(), "fixtures")
	// An unreachable database: replay must get this far with no MCP setting.
	env := map[string]string{config.EnvDatabaseURL: "postgres://copilot@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"}
	args := func(extra ...string) []string {
		return append([]string{"run", "--suite", "suite-rp", "--replay", "--no-llm", "--scenarios-dir", scen,
			"--config-dir", configDir, "--fixtures-dir", fixtures, "--results-dir", t.TempDir()}, extra...)
	}

	t.Run("no fixtures", func(t *testing.T) {
		code, _, errText := runCLIEnv(t, env, args()...)
		if code == 0 || !strings.Contains(errText, "no fixtures index") || !strings.Contains(errText, "make record-fixtures") {
			t.Errorf("exit %d: %s", code, errText)
		}
		if strings.Contains(errText, config.EnvBooksMCPURL) || strings.Contains(errText, config.EnvMCPTokenAgent) {
			t.Errorf("replay asked for MCP settings: %s", errText)
		}
	})

	// Record an (empty) set of fixtures for both months.
	suite, err := evals.LoadSuite(filepath.Join(scen, "suite-rp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := evals.NewRecorder(fixtures, "suite-rp")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rec.Finish(t.Context(), evals.RecordMeta{Suite: suite, TruthDir: filepath.Join(scen, "suite-rp", "ground_truth"),
		Months: suite.Months, SeederCommit: "test", RecordedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	t.Run("fixtures good, database down", func(t *testing.T) {
		code, _, errText := runCLIEnv(t, env, args()...)
		if code == 0 {
			t.Fatal("exit 0 with no database")
		}
		if strings.Contains(errText, "fixture") || strings.Contains(errText, "MCP") || strings.Contains(errText, config.EnvMCPTokenAgent) {
			t.Errorf("replay failed on fixtures or MCP, not the database: %s", errText)
		}
	})

	t.Run("stale truth", func(t *testing.T) {
		p := filepath.Join(scen, "suite-rp", "ground_truth", "sharma-2026-09.json")
		b, _ := os.ReadFile(p)
		if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(p, b, 0o600) }()
		code, _, errText := runCLIEnv(t, env, args()...)
		if code == 0 || !strings.Contains(errText, "fixtures are stale: re-record after seeding (make record-fixtures)") {
			t.Errorf("exit %d: %s", code, errText)
		}
	})

	t.Run("record and replay together", func(t *testing.T) {
		code, _, errText := runCLIEnv(t, env, args("--record")...)
		if code == 0 || !strings.Contains(errText, "--record and --replay") {
			t.Errorf("exit %d: %s", code, errText)
		}
	})

	t.Run("a live run still needs the MCP settings", func(t *testing.T) {
		code, _, errText := runCLIEnv(t, env, "run", "--suite", "suite-rp", "--scenarios-dir", scen)
		if code == 0 || !strings.Contains(errText, config.EnvBooksMCPURL) {
			t.Errorf("exit %d: %s", code, errText)
		}
	})
}
