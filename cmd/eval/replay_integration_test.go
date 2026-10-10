//go:build integration

package main

// eval run --record and --replay end to end, in process: Postgres
// (testcontainers) and the real MCP servers over the fake ERPNext's
// skeleton month for the recording; the replays then run with the MCP
// settings pointing nowhere. No model, no real ERPNext.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

// replayEnv is a recorded suite: suite-it's evaluated month with its
// three planted bank charges.
type replayEnv struct {
	*evalEnv
	fixtures, truth, configDir string
}

// liveEnv is the environment of a live (recording) run.
func (e *evalEnv) liveEnv() map[string]string {
	return map[string]string{
		config.EnvDatabaseURL:    e.dbURL,
		config.EnvBooksMCPURL:    e.stack.BooksURL,
		config.EnvEvidenceMCPURL: e.stack.EvidenceURL,
		config.EnvMCPTokenAgent:  e.stack.Token,
		config.EnvLLMProvider:    config.ProviderClaudeCLI,
	}
}

// noNetworkEnv points every MCP setting at an unroutable address with
// garbage tokens: a replay that touched them would fail or hang.
func (e *evalEnv) noNetworkEnv() map[string]string {
	return map[string]string{
		config.EnvDatabaseURL:    e.dbURL,
		config.EnvBooksMCPURL:    "http://10.255.255.1:9/mcp",
		config.EnvEvidenceMCPURL: "http://10.255.255.1:9/mcp",
		config.EnvMCPTokenAgent:  "garbage-agent-token",
		config.EnvMCPTokenAdmin:  "garbage-admin-token",
		config.EnvLLMProvider:    config.ProviderClaudeCLI,
	}
}

// recordSuite writes suite-it's ground truth and records the evaluated
// month through eval run --record, then stops the MCP servers.
func recordSuite(t *testing.T) *replayEnv {
	t.Helper()
	e := setupEval(t)
	truth := filepath.Join(e.scenarios, "suite-it", "ground_truth")
	if err := os.MkdirAll(truth, 0o750); err != nil {
		t.Fatal(err)
	}
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
	b, _ := json.Marshal(map[string]any{"scenario": "suite-it", "company": fakeerp.CompanyID, "month": fakeerp.Month, "clean": false,
		"planted": ps, "expected": []any{}, "investigations": []any{}})
	if err := os.WriteFile(filepath.Join(truth, "sharma-"+fakeerp.Month+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	configDir, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	r := &replayEnv{evalEnv: e, fixtures: filepath.Join(t.TempDir(), "fixtures"), truth: truth, configDir: configDir}

	recordScanExtra = []string{fakeerp.CompanyGSTIN, fakeerp.SupplierGSTIN}
	t.Cleanup(func() { recordScanExtra = nil })
	code, _, errText := runCLIEnv(t, e.liveEnv(), r.args(t, "--record", "--no-llm")...)
	if code != 0 {
		t.Fatalf("eval run --record: exit %d\n%s", code, errText)
	}
	if _, err := os.Stat(filepath.Join(r.fixtures, "suite-it", evals.FixtureIndexFile)); err != nil {
		t.Fatalf("no index: %v", err)
	}
	e.stack.Close() // nothing live from here on
	return r
}

func (r *replayEnv) args(t *testing.T, extra ...string) []string {
	return append([]string{"run", "--suite", "suite-it", "--only", "sharma:" + fakeerp.Month,
		"--scenarios-dir", r.scenarios, "--config-dir", r.configDir, "--fixtures-dir", r.fixtures,
		"--results-dir", t.TempDir()}, extra...)
}

// replay runs eval run --replay --no-llm with no network settings and
// returns the results folder.
func (r *replayEnv) replay(t *testing.T) string {
	t.Helper()
	code, out, errText := runCLIEnv(t, r.noNetworkEnv(), r.args(t, "--replay", "--no-llm")...)
	if code != 0 {
		t.Fatalf("eval run --replay: exit %d\n%s", code, errText)
	}
	dir, man := readManifest(t, out)
	if !man.Flags.Replay || man.Agent || len(man.FixturesIndexSHA256) != 64 || man.Failed != 0 {
		t.Errorf("manifest flags %+v agent %v index %q failed %d", man.Flags, man.Agent, man.FixturesIndexSHA256, man.Failed)
	}
	return dir
}

func TestReplayNoNetwork(t *testing.T) {
	r := recordSuite(t)
	dir := r.replay(t)
	var res evals.Result
	b, err := os.ReadFile(filepath.Join(dir, "sharma-"+fakeerp.Month+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range res.Findings {
		got = append(got, f.Keys["bank_txn_id"])
	}
	var want []string
	for _, c := range fakeerp.PlantedCharges {
		want = append(want, c.TxnID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("replayed bank charges %v, want %v", got, want)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, evals.ManifestFile))
	if strings.Contains(string(raw), "garbage") || strings.Contains(string(raw), "10.255.255.1") {
		t.Errorf("manifest carries an MCP setting:\n%s", raw)
	}

	// Control: a live run can't start without reachable MCP servers (a
	// refused port, so the control doesn't wait out a connect timeout).
	refused := r.noNetworkEnv()
	refused[config.EnvBooksMCPURL], refused[config.EnvEvidenceMCPURL] = "http://127.0.0.1:1/mcp", "http://127.0.0.1:1/mcp"
	if code, _, _ := runCLIEnv(t, refused, r.args(t, "--no-llm")...); code == 0 {
		t.Error("a live run with no MCP server succeeded; the test proves nothing")
	}
}

// dropCheck wraps a check and drops its finding for one bank_txn_id: the
// sabotaged detector of the regression test.
type dropCheck struct {
	inner checks.Check
	txn   string
}

func (d dropCheck) Name() string { return d.inner.Name() }

func (d dropCheck) Run(ctx context.Context, in checks.Inputs) ([]checks.Finding, error) {
	fs, err := d.inner.Run(ctx, in)
	return slices.DeleteFunc(fs, func(f checks.Finding) bool { return f.Keys["bank_txn_id"] == d.txn }), err
}

// TestReplayGateCatchesBankrecRegression is the graph check: a change
// that breaks bank reconciliation fails the replay gate with a per-item
// report.
func TestReplayGateCatchesBankrecRegression(t *testing.T) {
	r := recordSuite(t)
	work := t.TempDir()
	baseline := filepath.Join(work, "baseline.json")
	pricing := filepath.Join(r.configDir, "pricing.yaml")
	gate := []string{"--require", "unrecorded_bank_charge.recall>=3/3"}

	// The good replay sets the baseline (never evals/baseline.json).
	good := r.replay(t)
	code, _, errText := runCLI(t, append([]string{"score", good, "--truth-dir", r.truth, "--out", filepath.Join(work, "good"),
		"--baseline-out", baseline, "--pricing", pricing}, gate...)...)
	if code != 0 {
		t.Fatalf("score the good replay: exit %d\n%s", code, errText)
	}
	code, _, errText = runCLI(t, append([]string{"score", good, "--truth-dir", r.truth, "--out", filepath.Join(work, "good-gate"),
		"--compare", baseline}, gate...)...)
	if code != 0 {
		t.Fatalf("the good replay fails its own baseline: exit %d\n%s", code, errText)
	}

	// The sabotaged bank reconciliation drops the second planted charge.
	lost := fakeerp.PlantedCharges[1].TxnID
	workflowHook = func(wf *agent.Workflow) {
		wf.Checks = []checks.Check{dropCheck{inner: &checks.BankRecCheck{}, txn: lost}}
	}
	bad := r.replay(t)
	workflowHook = nil

	out := filepath.Join(work, "bad-gate")
	code, _, errText = runCLI(t, append([]string{"score", bad, "--truth-dir", r.truth, "--out", out, "--compare", baseline}, gate...)...)
	if code == 0 {
		t.Fatal("eval score --compare passed a replay that lost a planted bank charge")
	}
	if !strings.Contains(errText, "regression sharma-2026-09/E02 (was caught, now missed)") {
		t.Errorf("stderr does not name the lost item:\n%s", errText)
	}
	sum, err := os.ReadFile(filepath.Join(out, evals.SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Eval gate (G4): FAIL", "| sharma-2026-09/E02 | regression | caught | missed |",
		"`go run ./cmd/eval run --suite suite-it --only sharma:2026-09`", "sharma-2026-09.json#E02`", lost,
		"unrecorded\\_bank\\_charge.recall\\>=3/3 failed"} {
		if !strings.Contains(string(sum), want) {
			t.Errorf("summary.md lacks %q:\n%s", want, sum)
		}
	}
}
