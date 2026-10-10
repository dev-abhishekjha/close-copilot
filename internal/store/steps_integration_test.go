//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// newRun inserts a synthetic close run and returns its ID.
func newRun(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := st.CreateCloseRun(context.Background(), store.CloseRun{
		ID: id, CompanyID: "testcorp", Month: "2026-08", Status: "running",
	}); err != nil {
		t.Fatalf("CreateCloseRun: %v", err)
	}
	return id
}

// putSnapshot stores a synthetic tool_result artifact and returns its sha.
func putSnapshot(t *testing.T, st *store.Store, run uuid.UUID, txn string) string {
	t.Helper()
	sha, err := st.PutArtifact(context.Background(), store.ArtifactToolResult, run, uuid.Nil, map[string]any{
		"server": "evidence", "tool": "list_bank_lines",
		"args":   map[string]string{"company": "testcorp", "from_date": "2026-08-01", "to_date": "2026-08-31"},
		"result": []map[string]any{{"txn_id": txn, "amount_paise": -29500, "narration": "SMS CHGS"}},
	})
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	return sha
}

// testFinding is a synthetic bank-charge finding whose evidence names a
// stored snapshot.
func testFinding(t *testing.T, st *store.Store, runID uuid.UUID, txn string) store.Finding {
	t.Helper()
	amt := money.Paise(29500)
	return store.Finding{
		ID: uuid.New(), RunID: runID, Type: "unrecorded_bank_charge", Severity: "medium",
		Title: "Unrecorded bank charge: SMS CHGS " + txn, AmountPaise: &amt,
		Keys:     map[string]string{"bank_txn_id": txn},
		Evidence: []store.EvidenceRef{{Server: "evidence", Tool: "list_bank_lines", IDs: []string{txn}, Artifact: putSnapshot(t, st, runID, txn)}},
		Status:   "open",
	}
}

func TestSteps(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)

	t.Run("begin, finish, begin again is done", func(t *testing.T) {
		run := newRun(t, st)
		s1, done, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
		if err != nil || done {
			t.Fatalf("Begin: done=%v err=%v", done, err)
		}
		if s1.Status != store.StepRunning || s1.Attempt != 1 || s1.StartedAt == nil {
			t.Fatalf("new step: %+v", s1)
		}
		out := putSnapshot(t, st, run, "TXN-OUT")
		if err := st.Finish(ctx, s1, store.StepDone, []string{out}, ""); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		s2, done, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
		if err != nil {
			t.Fatalf("second Begin: %v", err)
		}
		if !done || s2.ID != s1.ID || s2.Status != store.StepDone || s2.Attempt != 1 {
			t.Fatalf("second Begin on a done step: done=%v %+v", done, s2)
		}
		if len(s2.OutputRefs) != 1 || s2.OutputRefs[0] != out || s2.FinishedAt == nil {
			t.Errorf("done step refs/finished_at: %+v", s2)
		}
		if err := st.Finish(ctx, s1, store.StepFailed, nil, "late"); !errors.Is(err, store.ErrStepDone) {
			t.Errorf("Finish on a done step: %v, want ErrStepDone", err)
		}
	})

	t.Run("failed step is re-begun with attempt+1", func(t *testing.T) {
		run := newRun(t, st)
		s1, _, err := st.Begin(ctx, run, "explain", "finding-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, s1, store.StepFailed, nil, "model timeout"); err != nil {
			t.Fatal(err)
		}
		failed, err := st.GetStep(ctx, s1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != store.StepFailed || failed.Error == nil || *failed.Error != "model timeout" {
			t.Fatalf("failed step: %+v", failed)
		}
		s2, done, err := st.Begin(ctx, run, "explain", "finding-1")
		if err != nil || done {
			t.Fatalf("re-Begin: done=%v err=%v", done, err)
		}
		if s2.ID != s1.ID || s2.Attempt != 2 || s2.Status != store.StepRunning || s2.Error != nil || s2.FinishedAt != nil {
			t.Fatalf("re-begun step: %+v", s2)
		}
	})

	t.Run("concurrent begin gives one running row", func(t *testing.T) {
		run := newRun(t, st)
		const n = 8
		var wg sync.WaitGroup
		ids := make([]uuid.UUID, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
				ids[i], errs[i] = s.ID, err
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Begin %d: %v", i, err)
			}
			if ids[i] != ids[0] {
				t.Errorf("Begin %d returned step %s, want %s", i, ids[i], ids[0])
			}
		}
		steps, err := st.ListSteps(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Status != store.StepRunning || steps[0].Attempt != n {
			t.Fatalf("steps after concurrent Begin: %+v", steps)
		}
	})

	t.Run("stale attempt can't finish the step", func(t *testing.T) {
		run := newRun(t, st)
		a1, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
		if err != nil {
			t.Fatal(err)
		}
		// The run is resumed while attempt 1 is still alive somewhere.
		a2, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
		if err != nil || a2.Attempt != 2 {
			t.Fatalf("second Begin: %+v %v", a2, err)
		}
		if err := st.Finish(ctx, a1, store.StepFailed, nil, "stale failure"); !errors.Is(err, store.ErrStaleAttempt) {
			t.Errorf("stale Finish: %v, want ErrStaleAttempt", err)
		}
		stale := []store.Finding{testFinding(t, st, run, "TXN-STALE")}
		if err := st.FinishWithFindings(ctx, a1, nil, stale); !errors.Is(err, store.ErrStaleAttempt) {
			t.Errorf("stale FinishWithFindings: %v, want ErrStaleAttempt", err)
		}
		cur, err := st.GetStep(ctx, a1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != store.StepRunning || cur.Attempt != 2 || cur.Error != nil {
			t.Fatalf("a stale attempt changed the step: %+v", cur)
		}
		if got, _ := st.ListFindingsByRun(ctx, run); len(got) != 0 {
			t.Fatalf("stale attempt saved %d findings", len(got))
		}
		live := []store.Finding{testFinding(t, st, run, "TXN-LIVE")}
		if err := st.FinishWithFindings(ctx, a2, nil, live); err != nil {
			t.Fatalf("live FinishWithFindings: %v", err)
		}
		if err := st.Finish(ctx, a2, store.StepFailed, nil, "again"); !errors.Is(err, store.ErrStepDone) {
			t.Errorf("Finish after done: %v, want ErrStepDone", err)
		}
		if got, _ := st.ListFindingsByRun(ctx, run); len(got) != 1 {
			t.Fatalf("findings %d, want 1", len(got))
		}
	})

	t.Run("finish with findings commits both", func(t *testing.T) {
		run := newRun(t, st)
		s, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
		if err != nil {
			t.Fatal(err)
		}
		fs := []store.Finding{testFinding(t, st, run, "TXN-1"), testFinding(t, st, run, "TXN-2")}
		if err := st.FinishWithFindings(ctx, s, []string{fs[0].Evidence[0].Artifact}, fs); err != nil {
			t.Fatalf("FinishWithFindings: %v", err)
		}
		got, err := st.ListFindingsByRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("findings: %d, want 2", len(got))
		}
		step, err := st.GetStep(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if step.Status != store.StepDone || len(step.OutputRefs) != 1 {
			t.Fatalf("step after FinishWithFindings: %+v", step)
		}
	})

	t.Run("finish with findings rolls back on failure", func(t *testing.T) {
		begin := func(t *testing.T, run uuid.UUID) store.Step {
			s, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		tests := []struct {
			name    string
			prepare func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding)
			wantErr error
		}{
			{
				name: "step already done",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					s := begin(t, run)
					if err := st.Finish(ctx, s, store.StepDone, nil, ""); err != nil {
						t.Fatal(err)
					}
					return s, nil, []store.Finding{testFinding(t, st, run, "TXN-9")}
				},
				wantErr: store.ErrStepDone,
			},
			{
				name: "step missing",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					return store.Step{ID: uuid.New(), Attempt: 1}, nil, []store.Finding{testFinding(t, st, run, "TXN-8")}
				},
				wantErr: store.ErrNotFound,
			},
			{
				name: "finding from another run",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					s := begin(t, run)
					other := newRun(t, st)
					return s, nil, []store.Finding{testFinding(t, st, run, "TXN-7"), testFinding(t, st, other, "TXN-6")}
				},
			},
			{
				name: "evidence ref with an empty artifact",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					f := testFinding(t, st, run, "TXN-5")
					f.Evidence[0].Artifact = ""
					return begin(t, run), nil, []store.Finding{testFinding(t, st, run, "TXN-4"), f}
				},
				wantErr: store.ErrMissingArtifact,
			},
			{
				name: "evidence ref naming no artifact",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					f := testFinding(t, st, run, "TXN-3")
					f.Evidence[0].Artifact = "0000000000000000000000000000000000000000000000000000000000000000"
					return begin(t, run), nil, []store.Finding{testFinding(t, st, run, "TXN-2"), f}
				},
				wantErr: store.ErrMissingArtifact,
			},
			{
				name: "finding without evidence",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					f := testFinding(t, st, run, "TXN-1")
					f.Evidence = nil
					return begin(t, run), nil, []store.Finding{f}
				},
				wantErr: store.ErrMissingArtifact,
			},
			{
				name: "output ref naming no artifact",
				prepare: func(t *testing.T, run uuid.UUID) (store.Step, []string, []store.Finding) {
					return begin(t, run), []string{"not-an-artifact"}, []store.Finding{testFinding(t, st, run, "TXN-0")}
				},
				wantErr: store.ErrMissingArtifact,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				run := newRun(t, st)
				step, refs, fs := tt.prepare(t, run)
				err := st.FinishWithFindings(ctx, step, refs, fs)
				if err == nil {
					t.Fatal("FinishWithFindings: expected an error")
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("error %v, want %v", err, tt.wantErr)
				}
				for _, f := range fs {
					got, err := st.ListFindingsByRun(ctx, f.RunID)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 0 {
						t.Errorf("run %s kept %d findings after the rollback", f.RunID, len(got))
					}
				}
				if cur, err := st.GetStep(ctx, step.ID); err == nil && tt.wantErr != store.ErrStepDone && cur.Status == store.StepDone {
					t.Errorf("step marked done despite the error")
				}
			})
		}
	})

	t.Run("finish rejects output refs that are not artifacts", func(t *testing.T) {
		run := newRun(t, st)
		s, _, err := st.Begin(ctx, run, "synthesize", "report")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, s, store.StepDone, []string{"sha-that-was-never-stored"}, ""); !errors.Is(err, store.ErrMissingArtifact) {
			t.Fatalf("Finish: %v, want ErrMissingArtifact", err)
		}
		cur, err := st.GetStep(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != store.StepRunning {
			t.Errorf("step changed: %+v", cur)
		}
	})

	t.Run("validation", func(t *testing.T) {
		run := newRun(t, st)
		if _, _, err := st.Begin(ctx, run, "checks.bankrec", "bankrec"); err == nil {
			t.Error("Begin accepted an invalid kind")
		}
		if _, _, err := st.Begin(ctx, run, "router", ""); err == nil {
			t.Error("Begin accepted an empty subject")
		}
		s, _, err := st.Begin(ctx, run, "router", "close")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, s, "finished", nil, ""); err == nil {
			t.Error("Finish accepted an invalid status")
		}
		if err := st.Finish(ctx, s, store.StepRunning, nil, ""); err == nil {
			t.Error("Finish accepted a non-terminal status")
		}
		if _, err := st.GetStep(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetStep of a missing step: %v", err)
		}
	})
}
