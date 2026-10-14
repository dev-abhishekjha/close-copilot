//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestRunMetricsIntegration(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	run := newRun(t, st)
	ex1, _, _ := st.Begin(ctx, run, store.StepKindExplain, "f1")
	ex2, _, _ := st.Begin(ctx, run, store.StepKindExplain, "f2")
	vf, _, _ := st.Begin(ctx, run, store.StepKindVerify, "f1")
	chk, _, _ := st.Begin(ctx, run, "check.bankrec", "bankrec")
	recordCall(t, st, run, ex1.ID, 1000, 200, 50, "0.000123", time.Time{})
	recordCall(t, st, run, ex2.ID, 500, 100, 0, "0.00005", time.Time{})
	recordCall(t, st, run, vf.ID, 300, 30, 10, "0.01", time.Time{})
	recordCall(t, st, run, chk.ID, 7, 7, 7, "", time.Time{})

	got, err := st.RunMetricsByStep(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != "explain" || got[1].Kind != "verify" || got[2].Kind != "other" {
		t.Fatalf("families = %+v", got)
	}
	if e := got[0]; e.InputTokens != 1500 || e.OutputTokens != 300 || e.CacheReadTokens != 50 || e.Calls != 2 || e.CostUSD != "0.0002" {
		t.Errorf("explain = %+v", e)
	}
	if o := got[2]; o.InputTokens != 7 || o.Calls != 1 || o.CostUSD != "0" {
		t.Errorf("other = %+v", o)
	}
	if none, err := st.RunMetricsByStep(ctx, uuid.New()); err != nil || len(none) != 0 {
		t.Errorf("unknown run = %+v, %v", none, err)
	}

	// Summary: a far-future done run with findings is the newest.
	fin := uuid.New()
	start := time.Now().UTC().Add(24 * time.Hour)
	if err := st.CreateCloseRun(ctx, store.CloseRun{ID: fin, CompanyID: "testcorp", Month: "2026-08", Status: store.RunDone, StartedAt: &start}); err != nil {
		t.Fatal(err)
	}
	mk := func(sev, status string, verified bool) {
		f := store.Finding{ID: uuid.New(), RunID: fin, Type: "unrecorded_bank_charge", Severity: sev, Title: "t",
			Keys: map[string]string{"k": uuid.NewString()}, Evidence: []store.EvidenceRef{}, Status: status, Verified: verified}
		if err := st.CreateFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	mk("high", "open", true)
	mk("high", "dismissed", false)
	mk("low", "open", true)
	stats, err := st.RunsSummaryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Found || stats.RunID != fin || stats.Total != 3 || stats.Verified != 2 || stats.OpenHigh != 1 {
		t.Errorf("stats = %+v", stats)
	}
}
