//go:build integration

package evals

// The runner end to end: Postgres (testcontainers), the real books and
// evidence MCP servers over the fake ERPNext's skeleton month, and the same
// agent.Workflow the app runs, with no explainer (runs end partial). No
// real model and no real ERPNext.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

func setupStack(t *testing.T) (*store.Store, *fakeerp.Stack) {
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
	return st, stack
}

func TestRunnerFakeERP(t *testing.T) {
	st, stack := setupStack(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{
		BooksMCPURL: stack.BooksURL, EvidenceMCPURL: stack.EvidenceURL, MCPTokenAgent: config.NewSecret(stack.Token),
		LLMProvider: config.ProviderClaudeCLI, LLMModelFast: "haiku", LLMModelStrong: "sonnet",
		LLMDailyBudgetUSD: 2, LLMRunTokenCap: 200000,
	}
	reg, err := agent.NewRegistry(t.Context(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	profiles, err := company.LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := company.LoadRules(filepath.Join("..", "..", "config", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]company.Profile{}
	for _, p := range profiles {
		byID[p.ID] = p
	}
	results := t.TempDir()
	wf := &agent.Workflow{
		Store: st, Books: &agent.MCPBooks{Registry: reg, Companies: st}, Evidence: &agent.MCPEvidence{Registry: reg},
		Profiles: byID, Rules: rules, Config: cfg, ResultsDir: results, Log: log,
	}

	// The fake ERPNext serves September only; August, the control month,
	// has no statement, so preflight refuses it and the run ends failed.
	suite, err := LoadSuite(writeSuite(t, "suite-it",
		"suite: suite-it\nevaluated:\n  - {company: sharma, months: [\""+fakeerp.Month+"\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Closer: wf, Reader: st, ResultsDir: results, Config: ManifestConfig{LLMModelFast: "haiku", LLMModelStrong: "sonnet"}, Commit: "test"}

	t.Run("failed control month", func(t *testing.T) {
		dir, man, err := r.Run(t.Context(), suite, suite.Months)
		if !errors.Is(err, ErrRunsFailed) {
			t.Fatalf("Run = %v, want ErrRunsFailed for the refused control month", err)
		}
		checkFolder(t, dir, "manifest.json", "sharma-2026-08.json", "sharma-2026-09.json")
		if len(man.Results) != 2 || man.Results[0].Status != store.RunPartial || man.Results[0].Failed ||
			man.Results[1].Status != store.RunFailed || !man.Results[1].Failed {
			t.Errorf("manifest results %+v", man.Results)
		}
		checkSeptember(t, filepath.Join(dir, "sharma-2026-09.json"))
	})

	t.Run("only the evaluated month", func(t *testing.T) {
		months, err := suite.Filter([]string{"sharma:" + fakeerp.Month})
		if err != nil {
			t.Fatal(err)
		}
		r.ResultsDir = t.TempDir() // the first run's folder may carry the same second
		dir, man, err := r.Run(t.Context(), suite, months)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		checkFolder(t, dir, "manifest.json", "sharma-2026-09.json")
		if man.Failed != 0 || len(man.Results) != 1 {
			t.Errorf("manifest %+v", man)
		}
		checkSeptember(t, filepath.Join(dir, "sharma-2026-09.json"))
	})
}

func checkFolder(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, want) {
		t.Errorf("%s holds %v, want %v", dir, names, want)
	}
}

// checkSeptember checks the skeleton month's result: partial, with the
// three planted bank charges matched by their bank_txn_id keys.
func checkSeptember(t *testing.T, path string) {
	t.Helper()
	var res Result
	readJSON(t, path, &res)
	if res.Status != store.RunPartial || res.RunID == nil || res.Error != "" || res.CostUSD != "0" || res.TraceID != nil {
		t.Errorf("result status %q run %v error %q cost %q trace %v", res.Status, res.RunID, res.Error, res.CostUSD, res.TraceID)
	}
	var got []string
	for _, f := range res.Findings {
		if f.Type == checks.TypeUnrecordedBankCharge {
			got = append(got, f.Keys["bank_txn_id"])
		}
		if len(f.Evidence) == 0 || f.Evidence[0].Artifact == "" {
			t.Errorf("finding %v has no evidence artifact", f.Keys)
		}
	}
	var want []string
	for _, c := range fakeerp.PlantedCharges {
		want = append(want, c.TxnID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || len(res.Findings) != len(want) {
		t.Errorf("bank charge findings %v (of %d), want %v", got, len(res.Findings), want)
	}
}
