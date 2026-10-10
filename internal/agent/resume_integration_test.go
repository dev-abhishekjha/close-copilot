//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// setupStore starts a throwaway Postgres with the repo's migrations.
func setupStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	pg, err := postgres.Run(ctx,
		"pgvector/pgvector:pg17",
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
	url, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func createRun(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := st.CreateCloseRun(context.Background(), store.CloseRun{ID: id, CompanyID: synthCompany, Month: synthMonth, Status: "running"}); err != nil {
		t.Fatalf("CreateCloseRun: %v", err)
	}
	return id
}

var errCrash = errors.New("simulated crash")

// testWorkflow is a small stand-in for the CC-703 close workflow: router,
// two checks over snapshotting readers, one explain step per finding and a
// synthesize step, each a run_steps row.
type testWorkflow struct {
	st    *store.Store
	books checks.BooksReader
	ev    checks.EvidenceReader
	model *llm.FakeProvider

	// crashMid is the index of the step that does its work and then
	// "dies" before finishing (-1 for none).
	crashMid int
	// cancelAfter cancels the run's context once that many steps have
	// finished in this attempt (0 for never).
	cancelAfter int
	cancel      context.CancelFunc

	begun    int
	finished int
}

type stepWork func(ctx context.Context, s store.Step) (refs []string, findings []store.Finding, err error)

func (w *testWorkflow) step(ctx context.Context, runID uuid.UUID, kind, subject string, work stepWork) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	idx := w.begun
	w.begun++
	s, done, err := w.st.Begin(ctx, runID, kind, subject)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	refs, findings, err := work(ctx, s)
	if err != nil {
		_ = w.st.Finish(context.WithoutCancel(ctx), s, store.StepFailed, nil, err.Error())
		return err
	}
	if idx == w.crashMid {
		return errCrash // the step stays running, as after kill -9
	}
	if err := w.st.FinishWithFindings(ctx, s, refs, findings); err != nil {
		return err
	}
	w.finished++
	if w.cancelAfter > 0 && w.finished == w.cancelAfter && w.cancel != nil {
		w.cancel()
	}
	return nil
}

func (w *testWorkflow) run(ctx context.Context, runID uuid.UUID) error {
	w.begun, w.finished = 0, 0
	if err := w.step(ctx, runID, store.StepKindRouter, "close", func(context.Context, store.Step) ([]string, []store.Finding, error) {
		return nil, nil, nil
	}); err != nil {
		return err
	}
	if err := w.step(ctx, runID, "check.bankrec", "bankrec", func(ctx context.Context, s store.Step) ([]string, []store.Finding, error) {
		snap := NewSnapshots(w.st, runID, s.ID)
		fs, err := (&checks.BankRecCheck{}).Run(ctx, synthInputs(SnapshotBooks(w.books, snap), SnapshotEvidence(w.ev, snap)))
		if err != nil {
			return nil, nil, err
		}
		for i := range fs {
			fs[i].ID, fs[i].RunID = uuid.New(), runID
		}
		fs = checks.DedupeFindings(fs)
		if err := snap.Annotate(fs); err != nil {
			return nil, nil, err
		}
		return snap.Refs(), fs, nil
	}); err != nil {
		return err
	}
	if err := w.step(ctx, runID, "check.tb", "tb", func(ctx context.Context, s store.Step) ([]string, []store.Finding, error) {
		snap := NewSnapshots(w.st, runID, s.ID)
		if _, err := SnapshotBooks(w.books, snap).TrialBalance(ctx, synthCompany, synthDay(1), synthDay(31)); err != nil {
			return nil, nil, err
		}
		return snap.Refs(), nil, nil
	}); err != nil {
		return err
	}
	findings, err := w.st.ListFindingsByRun(ctx, runID)
	if err != nil {
		return err
	}
	sort.Slice(findings, func(i, j int) bool { return checks.DedupeKey(findings[i]) < checks.DedupeKey(findings[j]) })
	for _, f := range findings {
		if err := w.step(ctx, runID, store.StepKindExplain, f.ID.String(), func(ctx context.Context, s store.Step) ([]string, []store.Finding, error) {
			p := NewRecordingProvider(w.model, w.st, runID, s.ID)
			resp, err := p.Complete(ctx, llm.Request{
				Model:     "claude-haiku-5-5",
				Messages:  []llm.Message{llm.NewUserTextMessage(fmt.Sprintf("Explain %s (evidence %s).", f.Title, f.Evidence[0].Artifact))},
				Tools:     []llm.ToolSpec{{Name: "emit_explanation", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				ForceTool: "emit_explanation",
				MaxTokens: 256,
			})
			if err != nil {
				return nil, nil, err
			}
			sha, err := w.st.PutArtifact(ctx, store.ArtifactExplanation, runID, s.ID, map[string]any{"key": checks.DedupeKey(f), "text": resp.Text})
			if err != nil {
				return nil, nil, err
			}
			return []string{sha}, nil, nil
		}); err != nil {
			return err
		}
	}
	return w.step(ctx, runID, store.StepKindSynthesize, "report", func(ctx context.Context, s store.Step) ([]string, []store.Finding, error) {
		keys := make([]string, 0, len(findings))
		for _, f := range findings {
			keys = append(keys, checks.DedupeKey(f))
		}
		sha, err := w.st.PutArtifact(ctx, store.ArtifactReport, runID, s.ID, map[string]any{"findings": keys})
		if err != nil {
			return nil, nil, err
		}
		return []string{sha}, nil, nil
	})
}

func newModel() *llm.FakeProvider {
	return llm.NewFakeProvider().WithFallback(llm.Response{
		Model: "claude-haiku-5-5", Text: "A synthetic bank charge not yet booked.", StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 120, OutputTokens: 20},
	})
}

// normalized strips the per-run IDs from findings so two runs compare.
func normalized(t *testing.T, fs []store.Finding) string {
	t.Helper()
	out := slices.Clone(fs)
	for i := range out {
		out[i].ID, out[i].RunID = uuid.Nil, uuid.Nil
	}
	sort.Slice(out, func(i, j int) bool { return checks.DedupeKey(out[i]) < checks.DedupeKey(out[j]) })
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// verifyRun checks the CC-709 audit invariants of a finished run: every
// step done and unique, every finding's evidence artifact present and
// verifying, every LLM call with both artifacts.
func verifyRun(t *testing.T, st *store.Store, runID uuid.UUID, wantSteps int) ([]store.Step, []store.Finding, int) {
	t.Helper()
	ctx := context.Background()
	steps, err := st.ListSteps(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != wantSteps {
		t.Fatalf("run_steps rows %d, want %d", len(steps), wantSteps)
	}
	seen := map[string]bool{}
	llmCalls := 0
	for _, s := range steps {
		key := s.Kind + "|" + s.Subject
		if seen[key] {
			t.Errorf("duplicate step %s", key)
		}
		seen[key] = true
		if s.Status != store.StepDone || s.FinishedAt == nil {
			t.Errorf("step %s not done: %+v", key, s)
		}
		for _, ref := range s.OutputRefs {
			if _, err := st.GetArtifact(ctx, ref); err != nil {
				t.Errorf("step %s output ref %s: %v", key, ref, err)
			}
		}
		calls, err := st.ListLLMCalls(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range calls {
			llmCalls++
			for _, sha := range []string{c.PromptSHA256, c.ResponseSHA256} {
				if _, err := st.GetArtifact(ctx, sha); err != nil {
					t.Errorf("llm call %s artifact %s: %v", c.ID, sha, err)
				}
			}
		}
	}
	findings, err := st.ListFindingsByRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		for _, ref := range f.Evidence {
			a, err := st.GetArtifact(ctx, ref.Artifact)
			if err != nil {
				t.Errorf("finding %s evidence %s: %v", f.Type, ref.Artifact, err)
				continue
			}
			if a.Kind != store.ArtifactToolResult {
				t.Errorf("finding %s evidence artifact kind %s", f.Type, a.Kind)
			}
		}
	}
	return steps, findings, llmCalls
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	st := setupStore(t)

	// The uninterrupted reference run.
	refRun := createRun(t, st)
	ref := &testWorkflow{st: st, books: &synthBooks{}, ev: &synthEvidence{}, model: newModel(), crashMid: -1}
	if err := ref.run(ctx, refRun); err != nil {
		t.Fatalf("reference run: %v", err)
	}
	const nSteps = 8 // router, 2 checks, 4 explains, synthesize
	_, refFindings, refCalls := verifyRun(t, st, refRun, nSteps)
	if len(refFindings) != 4 || refCalls != 4 {
		t.Fatalf("reference run: %d findings, %d llm calls; want 4 and 4", len(refFindings), refCalls)
	}
	want := normalized(t, refFindings)

	type stepKey struct{ kind, subject string }
	tests := []struct {
		name        string
		crashMid    int // step index that dies mid-way (-1 none)
		cancelAfter int // cancel after this many finished steps (0 none)
		// the step re-run with attempt 2, by position in run order (-1 none)
		wantRetried int
		extraCalls  int // model calls repeated by the resume
	}{
		{name: "cancel after 1 step", crashMid: -1, cancelAfter: 1, wantRetried: -1},
		{name: "cancel after the checks", crashMid: -1, cancelAfter: 3, wantRetried: -1},
		{name: "cancel after 5 steps", crashMid: -1, cancelAfter: 5, wantRetried: -1},
		{name: "die inside the bankrec check", crashMid: 1, wantRetried: 1},
		{name: "die inside an explain step after the model call", crashMid: 4, wantRetried: 4, extraCalls: 1},
		{name: "die inside synthesize", crashMid: 7, wantRetried: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := createRun(t, st)
			model := newModel()
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			w := &testWorkflow{st: st, books: &synthBooks{}, ev: &synthEvidence{}, model: model, crashMid: tt.crashMid, cancelAfter: tt.cancelAfter, cancel: cancel}
			err := w.run(runCtx, run)
			if !errors.Is(err, errCrash) && !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v, want a crash or cancellation", err)
			}
			before, err := st.ListSteps(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			doneBefore := 0
			for _, s := range before {
				if s.Status == store.StepDone {
					doneBefore++
				}
			}
			if doneBefore >= nSteps {
				t.Fatalf("interrupted run already finished all %d steps", doneBefore)
			}

			// Resume: a fresh workflow over the same run.
			w2 := &testWorkflow{st: st, books: &synthBooks{}, ev: &synthEvidence{}, model: model, crashMid: -1}
			if err := w2.run(ctx, run); err != nil {
				t.Fatalf("resume: %v", err)
			}
			steps, findings, calls := verifyRun(t, st, run, nSteps)
			if got := normalized(t, findings); got != want {
				t.Errorf("findings after resume differ from the uninterrupted run:\n got %s\nwant %s", got, want)
			}
			if calls != 4+tt.extraCalls || model.CallCount() != 4+tt.extraCalls {
				t.Errorf("llm calls rows %d, model calls %d; want %d", calls, model.CallCount(), 4+tt.extraCalls)
			}

			// Attempts: only the step that died mid-way ran twice.
			order := map[stepKey]int{{store.StepKindRouter, "close"}: 0, {"check.bankrec", "bankrec"}: 1, {"check.tb", "tb"}: 2, {store.StepKindSynthesize, "report"}: 7}
			sorted := slices.Clone(findings)
			sort.Slice(sorted, func(i, j int) bool { return checks.DedupeKey(sorted[i]) < checks.DedupeKey(sorted[j]) })
			for i, f := range sorted {
				order[stepKey{store.StepKindExplain, f.ID.String()}] = 3 + i
			}
			for _, s := range steps {
				pos, ok := order[stepKey{s.Kind, s.Subject}]
				if !ok {
					t.Errorf("unexpected step %s/%s", s.Kind, s.Subject)
					continue
				}
				wantAttempt := 1
				if pos == tt.wantRetried {
					wantAttempt = 2
				}
				if s.Attempt != wantAttempt {
					t.Errorf("step %d %s/%s attempt %d, want %d", pos, s.Kind, s.Subject, s.Attempt, wantAttempt)
				}
			}

			// A second resume of the finished run does nothing.
			w3 := &testWorkflow{st: st, books: &synthBooks{}, ev: &synthEvidence{}, model: model, crashMid: -1}
			if err := w3.run(ctx, run); err != nil {
				t.Fatalf("second resume: %v", err)
			}
			again, _, calls2 := verifyRun(t, st, run, nSteps)
			if calls2 != calls || model.CallCount() != 4+tt.extraCalls {
				t.Errorf("second resume made model calls: rows %d -> %d", calls, calls2)
			}
			if !reflect.DeepEqual(again, steps) {
				t.Errorf("second resume changed steps")
			}
		})
	}
}

func TestSnapshotWithStore(t *testing.T) {
	ctx := context.Background()
	st := setupStore(t)
	run := createRun(t, st)
	s, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
	if err != nil {
		t.Fatal(err)
	}
	snap := NewSnapshots(st, run, s.ID)
	fs, err := (&checks.BankRecCheck{}).Run(ctx, synthInputs(SnapshotBooks(&synthBooks{}, snap), SnapshotEvidence(&synthEvidence{}, snap)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range fs {
		fs[i].ID, fs[i].RunID = uuid.New(), run
	}
	if err := snap.Annotate(fs); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if err := st.FinishWithFindings(ctx, s, snap.Refs(), fs); err != nil {
		t.Fatal(err)
	}
	stored, err := st.ListFindingsByRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(fs) || len(stored) == 0 {
		t.Fatalf("stored %d findings, want %d", len(stored), len(fs))
	}
	for _, f := range stored {
		for _, ref := range f.Evidence {
			a, err := st.GetArtifact(ctx, ref.Artifact)
			if err != nil {
				t.Fatalf("finding %s: %v", f.Type, err)
			}
			var snapContent toolSnapshot
			if err := json.Unmarshal(a.Content, &snapContent); err != nil {
				t.Fatal(err)
			}
			if snapContent.Server != ref.Server || snapContent.Tool != ref.Tool {
				t.Errorf("finding %s: artifact %s/%s, ref %s/%s", f.Type, snapContent.Server, snapContent.Tool, ref.Server, ref.Tool)
			}
			if a.ProducedBy == nil || *a.ProducedBy != s.ID {
				t.Errorf("artifact produced_by %v, want step %s", a.ProducedBy, s.ID)
			}
		}
	}
}

func TestRecordingProviderWithStore(t *testing.T) {
	ctx := context.Background()
	st := setupStore(t)
	run := createRun(t, st)
	s, _, err := st.Begin(ctx, run, store.StepKindExplain, "finding-1")
	if err != nil {
		t.Fatal(err)
	}
	inner := llm.NewFakeProvider(
		llm.ScriptedCall{Response: llm.Response{Model: "claude-haiku-5-5", Text: "ok", Usage: llm.Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.000075}}},
		llm.ScriptedCall{Response: llm.Response{Model: "claude-unlisted-9", Text: "ok", Usage: llm.Usage{InputTokens: 50, OutputTokens: 5}}, Err: llm.ErrUnpricedModel},
	)
	p := NewRecordingProvider(inner, st, run, s.ID)
	if _, err := p.Complete(ctx, explainRequest()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := p.Complete(ctx, explainRequest()); !errors.Is(err, llm.ErrUnpricedModel) {
		t.Fatalf("second call: %v, want ErrUnpricedModel", err)
	}
	calls, err := st.ListLLMCalls(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("llm_calls rows %d, want 2", len(calls))
	}
	for _, c := range calls {
		p, err := st.GetArtifact(ctx, c.PromptSHA256)
		if err != nil || p.Kind != store.ArtifactPrompt {
			t.Errorf("prompt artifact: %v %+v", err, p)
		}
		r, err := st.GetArtifact(ctx, c.ResponseSHA256)
		if err != nil || r.Kind != store.ArtifactResponse {
			t.Errorf("response artifact: %v %+v", err, r)
		}
	}
	if calls[0].CostUSD != "0.000075" || calls[1].Model != "claude-unlisted-9" || calls[1].CostUSD != "0" {
		t.Errorf("llm calls %+v", calls)
	}

	// A provider bound to another run can't record against this run's step.
	other := createRun(t, st)
	wrong := NewRecordingProvider(llm.NewFakeProvider(), st, other, s.ID)
	if _, err := wrong.Complete(ctx, explainRequest()); !errors.Is(err, store.ErrStepRunMismatch) {
		t.Errorf("cross-run record: %v, want ErrStepRunMismatch", err)
	}
	if after, _ := st.ListLLMCalls(ctx, s.ID); len(after) != 2 {
		t.Errorf("llm_calls rows %d after a cross-run attempt, want 2", len(after))
	}
}
