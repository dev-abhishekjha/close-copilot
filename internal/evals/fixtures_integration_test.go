//go:build integration

package evals

// Record and replay end to end: Postgres (testcontainers), the real books
// and evidence MCP servers over the fake ERPNext's skeleton month, and the
// app's agent.Workflow. The recording runs live through the MCP readers;
// then the MCP servers are stopped and the replay must produce the same
// findings from the fixtures alone.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

// findingKey is what must survive a replay: type, keys and amount.
func findingKeys(t *testing.T, path string) []string {
	t.Helper()
	var res Result
	readJSON(t, path, &res)
	if res.Status != store.RunPartial || res.Error != "" {
		t.Errorf("%s: status %q error %q", path, res.Status, res.Error)
	}
	var out []string
	for _, f := range res.Findings {
		amt := "nil"
		if f.AmountPaise != nil {
			amt = strconv.FormatInt(int64(*f.AmountPaise), 10)
		}
		out = append(out, f.Type+" "+canonicalKeys(f.Keys)+" "+amt)
	}
	slices.Sort(out)
	return out
}

// skeletonTruth writes the ground truth of the fake month: the three
// planted bank charges, keyed by bank_txn_id.
func skeletonTruth(t *testing.T, dir, suite string) {
	t.Helper()
	type planted struct {
		ID          string            `json:"id"`
		Type        string            `json:"type"`
		Keys        map[string]string `json:"keys"`
		AmountPaise json.Number       `json:"amount_paise"`
	}
	var ps []planted
	for i, c := range fakeerp.PlantedCharges {
		ps = append(ps, planted{ID: "E0" + string(rune('1'+i)), Type: checks.TypeUnrecordedBankCharge,
			Keys: map[string]string{"bank_txn_id": c.TxnID}, AmountPaise: "100"})
	}
	gt := map[string]any{"scenario": suite, "company": fakeerp.CompanyID, "month": fakeerp.Month, "clean": false,
		"planted": ps, "expected": []any{}, "investigations": []any{}}
	b, err := json.Marshal(gt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, TruthFileName(fakeerp.CompanyID, fakeerp.Month)), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecordReplayRoundTrip(t *testing.T) {
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
	suite, err := LoadSuite(writeSuite(t, "suite-rt",
		"suite: suite-rt\nevaluated:\n  - {company: sharma, months: [\""+fakeerp.Month+"\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	months, err := suite.Filter([]string{"sharma:" + fakeerp.Month})
	if err != nil {
		t.Fatal(err)
	}
	truthDir := filepath.Join(t.TempDir(), "ground_truth")
	skeletonTruth(t, truthDir, suite.Name)
	root := filepath.Join(t.TempDir(), "fixtures")

	// Record, live.
	rec, err := NewRecorder(root, suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	rb := rec.Books(&agent.MCPBooks{Registry: reg, Companies: st})
	wf := &agent.Workflow{Store: st, Books: rb, Evidence: rec.Evidence(&agent.MCPEvidence{Registry: reg}),
		Profiles: byID, Rules: rules, Config: cfg, ResultsDir: t.TempDir(), Log: log}
	r := &Runner{Closer: &ScopedCloser{Closer: wf, Scope: rec, After: rb.PrimeMonth}, Reader: st, ResultsDir: t.TempDir(),
		Config: ManifestConfig{LLMModelFast: "haiku", LLMModelStrong: "sonnet"}, Flags: Flags{Record: true}, Commit: "test"}
	liveDir, _, err := r.Run(t.Context(), suite, months)
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	idx, _, err := rec.Finish(t.Context(), RecordMeta{Suite: suite, TruthDir: truthDir, Months: months, SeederCommit: "test",
		RecordedAt: time.Now(), Profiles: profiles, ExtraAllowed: []string{fakeerp.CompanyGSTIN, fakeerp.SupplierGSTIN}})
	if err != nil {
		t.Fatalf("finish recording: %v", err)
	}
	if len(idx.Files) == 0 {
		t.Fatal("nothing recorded")
	}

	// Stop the MCP servers: the replay can't reach them.
	reg.Close()
	stack.Close()

	rp, sum, err := OpenReplay(ReplayOptions{Dir: root, Suite: suite, TruthDir: truthDir, Months: months})
	if err != nil {
		t.Fatal(err)
	}
	wf2 := &agent.Workflow{Store: st, Books: rp.Books(), Evidence: rp.Evidence(),
		Profiles: byID, Rules: rules, Config: cfg, ResultsDir: t.TempDir(), Log: log}
	r2 := &Runner{Closer: &ScopedCloser{Closer: wf2, Scope: rp}, Reader: st, ResultsDir: t.TempDir(),
		Config: ManifestConfig{LLMModelFast: "haiku", LLMModelStrong: "sonnet"}, Flags: Flags{Replay: true},
		Commit: "test", FixturesIndexSHA256: sum}
	replayDir, man, err := r2.Run(t.Context(), suite, months)
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if man.FixturesIndexSHA256 != sum || !man.Flags.Replay {
		t.Errorf("manifest fixtures %q flags %+v", man.FixturesIndexSHA256, man.Flags)
	}

	file := TruthFileName(fakeerp.CompanyID, fakeerp.Month)
	live, replayed := findingKeys(t, filepath.Join(liveDir, file)), findingKeys(t, filepath.Join(replayDir, file))
	if !slices.Equal(live, replayed) || len(live) != len(fakeerp.PlantedCharges) {
		t.Errorf("findings differ:\n live   %v\n replay %v", live, replayed)
	}

	// The replay scores the same against the truth.
	s, err := ScoreRun(ScoreOptions{ResultsDir: replayDir, TruthDir: truthDir})
	if err != nil {
		t.Fatal(err)
	}
	if s.Types[checks.TypeUnrecordedBankCharge].Caught != 3 {
		t.Errorf("replay caught %+v", s.Types)
	}

	// Snapshots are still stored on a replay, so findings stay rebuildable.
	var res Result
	readJSON(t, filepath.Join(replayDir, file), &res)
	for _, f := range res.Findings {
		if len(f.Evidence) == 0 || f.Evidence[0].Artifact == "" {
			t.Errorf("replayed finding %v has no evidence artifact", f.Keys)
		}
	}

	// A call outside the recording fails; it never goes live.
	rp.UseMonth(fakeerp.CompanyID, fakeerp.Month)
	if _, err := rp.Books().GLEntries(t.Context(), fakeerp.CompanyID, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrFixtureMissing) {
		t.Errorf("unrecorded call = %v", err)
	}
}
