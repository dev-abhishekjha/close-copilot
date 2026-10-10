//go:build integration

package main

// CC-705 end to end: a close of the fake ERPNext's skeleton month with the
// real explainer and verifier over a scripted fake model (never a real
// one) and COPILOT_FAULT=corrupt_explanation, then audit rebuild on every
// finding against the same Postgres. Tampering with a stored verdict makes
// the rebuild fail.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

// groundedModel answers emit_explanation with the finding's own amount,
// so every answer is grounded; with invent set, every explanation also
// states an invented amount, so every answer fails verification.
type groundedModel struct {
	mu     sync.Mutex
	calls  int
	invent bool
	// failModel, when set, makes every call to that model an error.
	failModel string
}

func (m *groundedModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.failModel != "" && req.Model == m.failModel {
		return llm.Response{}, errors.New("grounded model: scripted model error")
	}
	user := req.Messages[0].Content
	_, rest, _ := strings.Cut(user, "\n<<<FINDING\n")
	body, _, ok := strings.Cut(rest, "\nFINDING>>>")
	if !ok {
		return llm.Response{}, errors.New("grounded model: no FINDING section")
	}
	var f struct {
		AmountPaise money.Paise `json:"amount_paise"`
	}
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		return llm.Response{}, err
	}
	text := fmt.Sprintf("The bank debited %s that the books have not recorded.", f.AmountPaise.Format())
	if m.invent {
		text += " It also adds ₹4,321.00 of fees."
	}
	args, err := json.Marshal(map[string]any{
		"explanation":      text,
		"suggested_action": "book_entry", "action_note": "Book the charge.",
		"cited_amounts_paise": []money.Paise{f.AmountPaise}, "citations": []any{}, "needs_review": false,
		"proposal": map[string]any{"posting_date": "2026-09-30", "remark": "Bank charge per statement", "lines": []map[string]any{
			{"account": "Bank Charges - STPL", "debit_paise": f.AmountPaise, "credit_paise": 0},
			{"account": "HDFC Current 0001 - STPL", "debit_paise": 0, "credit_paise": f.AmountPaise},
		}},
	})
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Model:      req.Model + "-20261001",
		ToolCalls:  []llm.ToolCall{{ID: "call_0", Name: agent.ExplainToolName, Args: args}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 900, OutputTokens: 120},
	}, nil
}

// crashAfterReopen applies the first ReopenStep and then fails, as a
// process that dies right after the reopen commits; later reopens fail
// without being applied.
type crashAfterReopen struct {
	agent.RunStore
	err     error
	crashed bool
}

func (c *crashAfterReopen) ReopenStep(ctx context.Context, step store.Step, feedback json.RawMessage) error {
	if c.crashed {
		return c.err
	}
	if err := c.RunStore.ReopenStep(ctx, step, feedback); err != nil {
		return err
	}
	c.crashed = true
	return c.err
}

func TestAuditRebuildSkeletonMonth(t *testing.T) {
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
		postgres.WithDatabase("test_copilot"), postgres.WithUsername("copilot"), postgres.WithPassword("copilot"),
		postgres.BasicWaitStrategies())
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

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := agent.NewRegistry(ctx, config.Config{
		BooksMCPURL: stack.BooksURL, EvidenceMCPURL: stack.EvidenceURL, MCPTokenAgent: config.NewSecret(stack.Token),
	}, log)
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

	fault, err := agent.NewFault(config.FaultCorruptExplanation)
	if err != nil {
		t.Fatal(err)
	}
	books := &agent.MCPBooks{Registry: reg, Companies: st}
	model := &groundedModel{}
	wf := &agent.Workflow{
		Store: st, Books: books, Evidence: &agent.MCPEvidence{Registry: reg}, Profiles: byID, Rules: rules,
		Explainer: &agent.LLMExplainer{Store: st, Accounts: agent.BooksAccounts{Books: books},
			FastModel: "claude-haiku-5-5", StrongModel: "claude-sonnet-5-5", Fault: fault, Log: log},
		Verifier: &agent.CodeVerifier{Store: st, Citations: st},
		Model:    model, Parallel: 1,
		Config:     config.Config{LLMDailyBudgetUSD: 2, LLMRunTokenCap: 200000},
		ResultsDir: t.TempDir(), Log: log,
	}
	res, err := wf.RunClose(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunDone || res.Findings != 3 || model.calls != 4 {
		t.Fatalf("result %+v with %d model calls; want done, 3 findings, 4 calls", res, model.calls)
	}
	findings, err := st.ListFindingsByRun(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}

	load := func(...string) (config.Config, error) {
		return config.Config{DatabaseURL: config.NewSecret(dbURL)}, nil
	}
	audit := func(id string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{"rebuild", id}, &stdout, &stderr, load, openStore)
		return code, stdout.String() + stderr.String()
	}
	for _, f := range findings {
		if !f.Verified {
			t.Errorf("finding %s not verified", f.ID)
		}
		if code, out := audit(f.ID.String()); code != exitMatch || !strings.Contains(out, "== result: match") {
			t.Errorf("rebuild %s: exit %d\n%s", f.ID, code, out)
		}
	}

	// Tampering with a stored verdict row makes the rebuild fail.
	target := findings[0].ID.String()
	var verdict string
	if err := st.Pool().QueryRow(ctx, `SELECT output_refs[1] FROM run_steps WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res.RunID, target).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE artifacts SET content = jsonb_set(content, '{pass}', 'false') WHERE sha256 = $1`, verdict); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(target); code != exitMismatch {
		t.Errorf("rebuild of a tampered verdict: exit %d, want 1\n%s", code, out)
	}
	// A findings row that differs from the stored records makes the
	// rebuild fail: an edited explanation, and a done explain step whose
	// verify output ref is gone.
	if _, err := st.Pool().Exec(ctx, `UPDATE findings SET explanation = 'Edited by hand.' WHERE id = $1`, findings[1].ID); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(findings[1].ID.String()); code != exitMismatch || !strings.Contains(out, "explanation") {
		t.Errorf("rebuild of an edited explanation: exit %d, want 1\n%s", code, out)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE run_steps SET output_refs = '{}' WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res.RunID, findings[2].ID.String()); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(findings[2].ID.String()); code != exitMismatch {
		t.Errorf("rebuild without a verdict: exit %d, want 1\n%s", code, out)
	}

	// A second close where every explanation fails three times: every
	// finding ends needs_review, unverified, and still rebuilds; marking
	// one verified by hand makes the rebuild fail.
	wf.Model = &groundedModel{invent: true}
	res2, err := wf.RunClose(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatalf("second RunClose: %v", err)
	}
	if res2.Status != store.RunPartial {
		t.Fatalf("second run %+v, want partial", res2)
	}
	review, err := st.ListFindingsByRun(ctx, res2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range review {
		if f.Verified || f.Status != "needs_review" {
			t.Errorf("finding %s: verified %v status %s", f.ID, f.Verified, f.Status)
		}
		if code, out := audit(f.ID.String()); code != exitMatch {
			t.Errorf("rebuild of a needs_review finding %s: exit %d\n%s", f.ID, code, out)
		}
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE findings SET verified = true WHERE id = $1`, review[0].ID); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(review[0].ID.String()); code != exitMismatch || !strings.Contains(out, "verified is true") {
		t.Errorf("rebuild of a needs_review finding marked verified: exit %d, want 1\n%s", code, out)
	}
	// A final failure set back to open, and a final failure whose verify
	// step lost its error, make the rebuild fail.
	if _, err := st.Pool().Exec(ctx, `UPDATE findings SET status = 'open' WHERE id = $1`, review[1].ID); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(review[1].ID.String()); code != exitMismatch || !strings.Contains(out, "status is") {
		t.Errorf("rebuild of a final failure set back to open: exit %d, want 1\n%s", code, out)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE run_steps SET error = NULL WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res2.RunID, review[2].ID.String()); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(review[2].ID.String()); code != exitMismatch || !strings.Contains(out, "verify step has no error") {
		t.Errorf("rebuild of a final failure whose verify step lost its error: exit %d, want 1\n%s", code, out)
	}
	// A passing verdict's verify step with an error makes it fail too.
	if _, err := st.Pool().Exec(ctx, `UPDATE run_steps SET error = 'verification failed' WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res.RunID, findings[0].ID.String()); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(findings[0].ID.String()); code != exitMismatch {
		t.Errorf("rebuild of a passing verdict whose verify step has an error: exit %d, want 1\n%s", code, out)
	}

	// A third close where every explanation fails twice and the third
	// (strong-model) attempt errors: every finding ends needs_review with
	// no final explanation, and rebuilds from its last stored verdict.
	// Setting one back to open makes the rebuild fail.
	wf.Model = &groundedModel{invent: true, failModel: "claude-sonnet-5-5"}
	res3, err := wf.RunClose(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatalf("third RunClose: %v", err)
	}
	if res3.Status != store.RunPartial {
		t.Fatalf("third run %+v, want partial", res3)
	}
	errored, err := st.ListFindingsByRun(ctx, res3.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range errored {
		if f.Verified || f.Status != "needs_review" || f.Explanation != nil {
			t.Errorf("finding %s: verified %v status %s explanation %v", f.ID, f.Verified, f.Status, f.Explanation != nil)
		}
		code, out := audit(f.ID.String())
		if code != exitMatch || !strings.Contains(out, "rebuilding the last rejected explanation (attempt 2)") {
			t.Errorf("rebuild of a finding whose last retry errored %s: exit %d\n%s", f.ID, code, out)
		}
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE findings SET status = 'open' WHERE id = $1`, errored[0].ID); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(errored[0].ID.String()); code != exitMismatch || !strings.Contains(out, "status is") {
		t.Errorf("rebuild of an errored retry set back to open: exit %d, want 1\n%s", code, out)
	}
	var lastVerdict string
	if err := st.Pool().QueryRow(ctx, `SELECT input_refs[array_length(input_refs, 1)] FROM run_steps WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res3.RunID, errored[1].ID.String()).Scan(&lastVerdict); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE artifacts SET content = jsonb_set(content, '{pass}', 'true') WHERE sha256 = $1`, lastVerdict); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(errored[1].ID.String()); code != exitMismatch {
		t.Errorf("rebuild of an errored retry with a tampered last verdict: exit %d, want 1\n%s", code, out)
	}

	// A fourth close that crashes right after the first reopen, then
	// resumes with a model that errors: the crashed finding's re-explain
	// errors, it ends needs_review with no explanation, and it rebuilds
	// from the attempt-1 verdict the verify step kept.
	errCrash := errors.New("synthetic crash after the reopen")
	wf.Store = &crashAfterReopen{RunStore: st, err: errCrash}
	wf.Model = &groundedModel{invent: true}
	res4, err := wf.RunClose(ctx, "sharma", "2026-09")
	if !errors.Is(err, errCrash) {
		t.Fatalf("fourth RunClose: %v, want the crash", err)
	}
	wf.Store = st
	wf.Model = &groundedModel{failModel: "claude-haiku-5-5"}
	if res, err := wf.ResumeClose(ctx, res4.RunID); err != nil || res.Status != store.RunPartial {
		t.Fatalf("resume of the fourth run %+v: %v, want partial", res, err)
	}
	crashed, err := st.ListFindingsByRun(ctx, res4.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := 0
	for _, f := range crashed {
		if f.Status != "needs_review" {
			continue
		}
		reviewed++
		if f.Verified || f.Explanation != nil {
			t.Errorf("crashed finding %s: verified %v explanation %v", f.ID, f.Verified, f.Explanation != nil)
		}
		code, out := audit(f.ID.String())
		if code != exitMatch || !strings.Contains(out, "rebuilding the last rejected explanation (attempt 1)") {
			t.Errorf("rebuild of the crashed finding %s: exit %d\n%s", f.ID, code, out)
		}
	}
	if reviewed != 1 {
		t.Errorf("%d findings in needs_review after the crash and resume, want 1", reviewed)
	}

	// A verdict that run_steps names but the artifacts table lost is an
	// integrity failure, not a store error.
	var lostVerdict string
	if err := st.Pool().QueryRow(ctx, `SELECT input_refs[array_length(input_refs, 1)] FROM run_steps WHERE run_id = $1 AND kind = 'verify' AND subject = $2`,
		res3.RunID, errored[2].ID.String()).Scan(&lostVerdict); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `DELETE FROM artifacts WHERE sha256 = $1`, lostVerdict); err != nil {
		t.Fatal(err)
	}
	if code, out := audit(errored[2].ID.String()); code != exitMismatch || !strings.Contains(out, "is missing") {
		t.Errorf("rebuild with a deleted verdict: exit %d, want 1\n%s", code, out)
	}

	// An unknown finding is a store error.
	if code, _ := audit("6f1c2a52-7d1b-4c1e-9d1a-2b3c4d5e6f70"); code != exitError {
		t.Errorf("unknown finding: exit %d, want 2", code)
	}
}
