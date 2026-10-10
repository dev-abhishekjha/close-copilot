//go:build integration

package main

// The eval binary end to end: built here, it runs a temp suite against
// Postgres (testcontainers) and the real books and evidence MCP servers
// over the fake ERPNext's skeleton month, served in this test process.
// No explainer, no real model, no real ERPNext.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

type evalEnv struct {
	dbURL     string
	stack     *fakeerp.Stack
	bin       string
	scenarios string
}

func setupEval(t *testing.T) *evalEnv {
	t.Helper()
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
		postgres.WithDatabase("test_copilot"),
		postgres.WithUsername("copilot"),
		postgres.WithPassword("copilot"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start testcontainers postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(pg); err != nil {
			t.Logf("terminate testcontainers postgres: %v", err)
		}
	})
	dbURL, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	lines, err := fakeerp.SkeletonStatement()
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeerp.Seed(ctx, st, lines); err != nil {
		t.Fatal(err)
	}
	stack, err := fakeerp.Start(st, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stack.Close)

	// The fake ERPNext serves September only: the control month (August)
	// has no statement, so preflight refuses it.
	scenarios := t.TempDir()
	suite := "suite: suite-it\nevaluated:\n  - {company: sharma, months: [\"" + fakeerp.Month + "\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n"
	if err := os.WriteFile(filepath.Join(scenarios, "suite-it.yaml"), []byte(suite), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "eval")
	if out, err := exec.CommandContext(t.Context(), goTool(t), "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return &evalEnv{dbURL: dbURL, stack: stack, bin: bin, scenarios: scenarios}
}

// run runs eval with only the environment it needs and returns the exit
// code, stdout and stderr.
func (e *evalEnv) run(t *testing.T, results string, args ...string) (int, string, string) {
	t.Helper()
	configDir, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, "--results-dir", results, "--scenarios-dir", e.scenarios, "--config-dir", configDir)
	cmd := exec.CommandContext(t.Context(), e.bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"DATABASE_URL=" + e.dbURL,
		"BOOKS_MCP_URL=" + e.stack.BooksURL,
		"EVIDENCE_MCP_URL=" + e.stack.EvidenceURL,
		"MCP_TOKEN_AGENT=" + e.stack.Token,
		"LLM_PROVIDER=claude-cli",
		"CLAUDE_CLI_PATH=/nonexistent/claude", // no model may run
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func readManifest(t *testing.T, stdout string) (string, evals.Manifest) {
	t.Helper()
	path := strings.TrimSpace(stdout)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("manifest path %q from stdout: %v", path, err)
	}
	var m evals.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path), m
}

func folder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestEvalCLI(t *testing.T) {
	e := setupEval(t)

	t.Run("a failed month exits non-zero after every month ran", func(t *testing.T) {
		results := t.TempDir()
		code, stdout, stderr := e.run(t, results, "run", "--suite", "suite-it")
		if code == 0 {
			t.Fatalf("eval exited 0 with a failed control month\nstderr: %s", stderr)
		}
		dir, man := readManifest(t, stdout)
		if want := []string{"manifest.json", "sharma-2026-08.json", "sharma-2026-09.json"}; !slices.Equal(folder(t, dir), want) {
			t.Errorf("results %v, want %v", folder(t, dir), want)
		}
		if !strings.HasPrefix(dir, filepath.Join(results, "suite-it")+string(filepath.Separator)) {
			t.Errorf("results folder %s is not under %s/suite-it", dir, results)
		}
		if man.Agent || man.Failed != 1 || man.Suite.Name != "suite-it" || len(man.Suite.SHA256) != 64 {
			t.Errorf("manifest agent %v failed %d suite %+v", man.Agent, man.Failed, man.Suite)
		}
		if len(man.Results) != 2 || man.Results[0].Status != store.RunPartial || man.Results[1].Status != store.RunFailed ||
			!strings.HasPrefix(man.Results[1].Reason, "preflight:") {
			t.Errorf("manifest results %+v", man.Results)
		}
	})

	t.Run("the evaluated month alone exits zero", func(t *testing.T) {
		results := t.TempDir()
		code, stdout, stderr := e.run(t, results, "run", "--suite", "suite-it", "--only", "sharma:"+fakeerp.Month, "--model-fast", "fast-override", "--no-agent")
		if code != 0 {
			t.Fatalf("eval exited %d\nstderr: %s", code, stderr)
		}
		dir, man := readManifest(t, stdout)
		if want := []string{"manifest.json", "sharma-2026-09.json"}; !slices.Equal(folder(t, dir), want) {
			t.Errorf("results %v, want %v", folder(t, dir), want)
		}
		if man.Config.LLMModelFast != "fast-override" || !man.Flags.NoAgent || !slices.Equal(man.Flags.Only, []string{"sharma:" + fakeerp.Month}) {
			t.Errorf("manifest config %+v flags %+v", man.Config, man.Flags)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), e.stack.Token) || strings.Contains(string(raw), "copilot:copilot") {
			t.Errorf("manifest carries a secret:\n%s", raw)
		}

		var res evals.Result
		b, err := os.ReadFile(filepath.Join(dir, "sharma-2026-09.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &res); err != nil {
			t.Fatal(err)
		}
		if res.Status != store.RunPartial || res.Models.Fast != "fast-override" || len(res.Models.Called) != 0 {
			t.Errorf("result status %q models %+v", res.Status, res.Models)
		}
		var got, want []string
		for _, f := range res.Findings {
			if f.Type == checks.TypeUnrecordedBankCharge {
				got = append(got, f.Keys["bank_txn_id"])
			}
		}
		for _, c := range fakeerp.PlantedCharges {
			want = append(want, c.TxnID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || len(res.Findings) != 3 {
			t.Errorf("bank charges %v of %d findings, want %v", got, len(res.Findings), want)
		}
	})

	t.Run("bad arguments exit non-zero without results", func(t *testing.T) {
		for _, args := range [][]string{
			{"run", "--suite", "suite-nope"},
			{"run", "--suite", "suite-it", "--only", "sharma:2026-01"},
			{"run"},
			{"close"},
		} {
			results := t.TempDir()
			if code, _, _ := e.run(t, results, args...); code == 0 {
				t.Errorf("eval %v exited 0", args)
			}
			if names := folder(t, results); len(names) != 0 {
				t.Errorf("eval %v wrote %v", args, names)
			}
		}
	})
}
