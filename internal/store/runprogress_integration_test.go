//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// recordCall stores a prompt and a response artifact and one llm_calls
// row for step, dated at (when not zero).
func recordCall(t *testing.T, st *store.Store, run, step uuid.UUID, in, out, cache int64, cost string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	p, err := st.PutArtifact(ctx, store.ArtifactPrompt, run, step, map[string]string{"prompt": uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.PutArtifact(ctx, store.ArtifactResponse, run, step, map[string]string{"response": uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertLLMCall(ctx, store.LLMCall{RunID: run, StepID: step, Model: "claude-haiku-5-5",
		PromptSHA256: p, ResponseSHA256: r, InputTokens: in, OutputTokens: out, CacheReadTokens: cache, CostUSD: cost})
	if err != nil {
		t.Fatal(err)
	}
	if !at.IsZero() {
		if _, err := st.Pool().Exec(ctx, `UPDATE llm_calls SET created_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunProgress(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	t.Run("SetRunState", func(t *testing.T) {
		run := uuid.New()
		if err := st.CreateCloseRun(ctx, store.CloseRun{ID: run, CompanyID: "testcorp", Month: "2026-08", Status: store.RunQueued}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunState(ctx, run, store.RunRunning, ""); err != nil {
			t.Fatal(err)
		}
		r, _ := st.GetCloseRun(ctx, run)
		if r.Status != store.RunRunning || r.StartedAt == nil || r.FinishedAt != nil || r.Error != nil {
			t.Fatalf("running: %+v", r)
		}
		started := *r.StartedAt

		if err := st.SetRunState(ctx, run, store.RunFailed, "cancelled"); err != nil {
			t.Fatal(err)
		}
		r, _ = st.GetCloseRun(ctx, run)
		if r.Status != store.RunFailed || r.FinishedAt == nil || r.Error == nil || *r.Error != "cancelled" {
			t.Fatalf("failed: %+v", r)
		}

		// A resume goes back to running: started_at kept, finished_at and
		// the error cleared.
		if err := st.SetRunState(ctx, run, store.RunRunning, ""); err != nil {
			t.Fatal(err)
		}
		r, _ = st.GetCloseRun(ctx, run)
		if r.FinishedAt != nil || r.Error != nil || !r.StartedAt.Equal(started) {
			t.Fatalf("resumed: %+v", r)
		}
		if err := st.SetRunState(ctx, run, store.RunPartial, "explanations missing"); err != nil {
			t.Fatal(err)
		}

		if err := st.SetRunState(ctx, run, "explaining", ""); err == nil {
			t.Error("invalid status accepted")
		}
		if err := st.SetRunState(ctx, uuid.New(), store.RunDone, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing run: %v", err)
		}
	})

	t.Run("RollupAndTokens", func(t *testing.T) {
		run, other := newRun(t, st), newRun(t, st)
		s1, _, _ := st.Begin(ctx, run, store.StepKindExplain, "f1")
		s2, _, _ := st.Begin(ctx, run, store.StepKindVerify, "f1")
		so, _, _ := st.Begin(ctx, other, store.StepKindExplain, "f1")

		if n, err := st.RunTokensUsed(ctx, run); err != nil || n != 0 {
			t.Fatalf("tokens before any call: %d, %v", n, err)
		}
		u, err := st.RollupRunUsage(ctx, run)
		if err != nil || u != (store.RunUsage{CostUSD: "0"}) {
			t.Fatalf("empty rollup %+v, %v", u, err)
		}

		recordCall(t, st, run, s1.ID, 1000, 200, 50, "0.000123", time.Time{})
		recordCall(t, st, run, s1.ID, 500, 100, 0, "0.00005", time.Time{})
		recordCall(t, st, run, s2.ID, 300, 30, 10, "", time.Time{}) // unpriced
		recordCall(t, st, other, so.ID, 99999, 99999, 0, "1.5", time.Time{})

		if n, err := st.RunTokensUsed(ctx, run); err != nil || n != 1000+200+500+100+300+30 {
			t.Errorf("tokens %d, %v", n, err)
		}
		u, err = st.RollupRunUsage(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		// 0.000173 rounds to close_runs' four decimals.
		want := store.RunUsage{InputTokens: 1800, OutputTokens: 330, CacheReadTokens: 60, CostUSD: "0.0002"}
		if u != want {
			t.Errorf("rollup %+v, want %+v", u, want)
		}
		r, _ := st.GetCloseRun(ctx, run)
		if r.InputTokens != 1800 || r.OutputTokens != 330 || r.CacheReadTokens != 60 {
			t.Errorf("close_runs tokens %+v", r)
		}
		var cost string
		if err := st.Pool().QueryRow(ctx, `SELECT cost_usd::text FROM close_runs WHERE id = $1`, run).Scan(&cost); err != nil || cost != "0.0002" {
			t.Errorf("close_runs cost %q, %v", cost, err)
		}
		if _, err := st.RollupRunUsage(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("rollup of a missing run: %v", err)
		}
	})

	t.Run("LLMBudgetExhausted", func(t *testing.T) {
		run := newRun(t, st)
		step, _, _ := st.Begin(ctx, run, store.StepKindExplain, "budget")
		// A day no other test writes to, so a shared database can't skew it.
		day := time.Date(2031, 1, 15, 0, 0, 0, 0, time.UTC)
		recordCall(t, st, run, step.ID, 10, 10, 0, "0.75", day.Add(9*time.Hour))
		recordCall(t, st, run, step.ID, 10, 10, 0, "0.5", day.Add(23*time.Hour))
		recordCall(t, st, run, step.ID, 10, 10, 0, "5", day.Add(-time.Minute)) // the day before
		recordCall(t, st, run, step.ID, 10, 10, 0, "5", day.Add(24*time.Hour)) // the day after

		noon := day.Add(12 * time.Hour)
		for _, tt := range []struct {
			budget float64
			now    time.Time
			want   bool
		}{
			{2, noon, false},
			{1.25, noon, true}, // spent equals the budget
			{1.2500001, noon, false},
			{1, noon.In(time.FixedZone("IST", 5*3600+1800)), true}, // the UTC day counts
			{0, day.Add(48 * time.Hour), true},
			{0.01, day.Add(72 * time.Hour), false},
		} {
			got, err := st.LLMBudgetExhausted(ctx, config.Config{LLMDailyBudgetUSD: tt.budget}, tt.now)
			if err != nil || got != tt.want {
				t.Errorf("budget %v at %s: %v, %v; want %v", tt.budget, tt.now, got, err, tt.want)
			}
		}
		if _, err := st.LLMBudgetExhausted(ctx, config.Config{LLMDailyBudgetUSD: -1}, noon); err == nil {
			t.Error("negative budget accepted")
		}
	})

	t.Run("SetFindingStatus", func(t *testing.T) {
		run := newRun(t, st)
		f := testFinding(t, st, run, "TXN-REVIEW")
		if err := st.CreateFindings(ctx, []store.Finding{f}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetFindingStatus(ctx, f.ID, "needs_review"); err != nil {
			t.Fatal(err)
		}
		fs, _ := st.ListFindingsByRun(ctx, run)
		if len(fs) != 1 || fs[0].Status != "needs_review" {
			t.Errorf("findings %+v", fs)
		}
		if err := st.SetFindingStatus(ctx, f.ID, "closed"); err == nil {
			t.Error("invalid finding status accepted")
		}
		if err := st.SetFindingStatus(ctx, uuid.New(), "open"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing finding: %v", err)
		}
	})

	t.Run("ValidateCallCost", func(t *testing.T) {
		for cost, ok := range map[string]bool{
			"": true, "0": true, "0.000001": true, "9999.999999": true, "1e3": true,
			"-0.000001": false, "10000": false, "10000.5": false, "1e4": false, "abc": false, "NaN": false,
		} {
			if err := store.ValidateCallCost(cost); (err == nil) != ok {
				t.Errorf("ValidateCallCost(%q) = %v, want ok=%v", cost, err, ok)
			}
		}
		// What it accepts fits llm_calls.cost_usd.
		run := newRun(t, st)
		step, _, _ := st.Begin(ctx, run, store.StepKindExplain, "max-cost")
		recordCall(t, st, run, step.ID, 1, 1, 0, "9999.999999", time.Date(2031, 2, 1, 0, 0, 0, 0, time.UTC))
	})
}
