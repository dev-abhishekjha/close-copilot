//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func countArtifacts(t *testing.T, st *store.Store, sha string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM artifacts WHERE sha256 = $1`, sha).Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	return n
}

func TestArtifacts(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)

	run := newRun(t, st)
	step, _, err := st.Begin(ctx, run, "check.bankrec", "bankrec")
	if err != nil {
		t.Fatal(err)
	}

	snapshot := map[string]any{
		"server": "evidence",
		"tool":   "list_bank_lines",
		"args":   map[string]any{"company": "testcorp", "from_date": "2026-08-01", "to_date": "2026-08-31"},
		"result": []map[string]any{
			{"txn_id": "TXN-0001", "amount_paise": int64(-29500), "narration": "SMS CHGS AUG"},
			{"txn_id": "TXN-0002", "amount_paise": int64(9007199254740993), "narration": "BIG <&> CREDIT", "rate": 1.5e-7},
		},
	}

	t.Run("put is idempotent and get verifies", func(t *testing.T) {
		sha1, err := st.PutArtifact(ctx, store.ArtifactToolResult, run, step.ID, snapshot)
		if err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}
		sha2, err := st.PutArtifact(ctx, store.ArtifactToolResult, run, step.ID, snapshot)
		if err != nil {
			t.Fatalf("second PutArtifact: %v", err)
		}
		if sha1 != sha2 {
			t.Fatalf("same value, different addresses %s %s", sha1, sha2)
		}
		if n := countArtifacts(t, st, sha1); n != 1 {
			t.Fatalf("artifact rows: %d, want 1", n)
		}
		_, want, err := store.CanonicalHash(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if sha1 != want {
			t.Errorf("address %s, want %s", sha1, want)
		}
		a, err := st.GetArtifact(ctx, sha1)
		if err != nil {
			t.Fatalf("GetArtifact: %v", err)
		}
		canon, _ := store.Canonical(snapshot)
		if string(a.Content) != string(canon) {
			t.Errorf("content round trip:\n got %s\nwant %s", a.Content, canon)
		}
		if a.Kind != store.ArtifactToolResult || a.RunID == nil || *a.RunID != run || a.ProducedBy == nil || *a.ProducedBy != step.ID {
			t.Errorf("artifact metadata: %+v", a)
		}
	})

	t.Run("nil run and step are stored as NULL", func(t *testing.T) {
		sha, err := st.PutArtifact(ctx, store.ArtifactReport, uuid.Nil, uuid.Nil, map[string]string{"report": "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		a, err := st.GetArtifact(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		if a.RunID != nil || a.ProducedBy != nil {
			t.Errorf("expected NULL run and step: %+v", a)
		}
	})

	t.Run("get of tampered content fails", func(t *testing.T) {
		v := map[string]any{"txn_id": "TXN-0003", "amount_paise": int64(-1180)}
		sha, err := st.PutArtifact(ctx, store.ArtifactToolResult, run, step.ID, v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE artifacts SET content = jsonb_set(content, '{amount_paise}', '-118000') WHERE sha256 = $1`, sha); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		if _, err := st.GetArtifact(ctx, sha); !errors.Is(err, store.ErrArtifactMismatch) {
			t.Fatalf("GetArtifact of tampered content: %v, want ErrArtifactMismatch", err)
		}
	})

	t.Run("missing and invalid", func(t *testing.T) {
		if _, err := st.GetArtifact(ctx, "0000"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetArtifact of a missing sha: %v", err)
		}
		if _, err := st.PutArtifact(ctx, "snapshot", run, step.ID, 1); err == nil {
			t.Error("PutArtifact accepted an invalid kind")
		}
	})

	t.Run("unstorable content fails closed", func(t *testing.T) {
		for _, v := range []any{
			map[string]string{"narration": "SMS\x00CHGS"},
			map[string]string{"narration": "bad \xff byte"},
		} {
			if _, err := st.PutArtifact(ctx, store.ArtifactToolResult, run, step.ID, v); !errors.Is(err, store.ErrUnstorableContent) {
				t.Errorf("PutArtifact: %v, want ErrUnstorableContent", err)
			}
		}
	})

	t.Run("llm call row", func(t *testing.T) {
		p, err := st.PutArtifact(ctx, store.ArtifactPrompt, run, step.ID, map[string]string{"prompt": "explain TXN-0001"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := st.PutArtifact(ctx, store.ArtifactResponse, run, step.ID, map[string]string{"text": "a bank charge"})
		if err != nil {
			t.Fatal(err)
		}
		costs := []struct{ in, want string }{
			{"0.000123", "0.000123"},
			{"0.5", "0.5"},
			{"1e-06", "0.000001"},
			{"1234.567891", "1234.567891"},
			{"", ""},
		}
		ids := map[uuid.UUID]string{}
		for _, c := range costs {
			id, err := st.InsertLLMCall(ctx, store.LLMCall{
				RunID: run, StepID: step.ID, Model: "claude-haiku-5-5", PromptSHA256: p, ResponseSHA256: r,
				InputTokens: 120, OutputTokens: 30, CacheReadTokens: 7, CostUSD: c.in, LatencyMS: 842,
			})
			if err != nil {
				t.Fatalf("InsertLLMCall cost %q: %v", c.in, err)
			}
			ids[id] = c.want
		}
		calls, err := st.ListLLMCalls(ctx, step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != len(costs) {
			t.Fatalf("llm calls: %d, want %d", len(calls), len(costs))
		}
		for _, c := range calls {
			want, ok := ids[c.ID]
			if !ok || c.RunID != run || c.PromptSHA256 != p || c.ResponseSHA256 != r || c.InputTokens != 120 ||
				c.OutputTokens != 30 || c.CacheReadTokens != 7 || c.LatencyMS != 842 || c.CostUSD != want {
				t.Errorf("llm call %+v, want cost %q", c, want)
			}
		}
		if _, err := st.InsertLLMCall(ctx, store.LLMCall{RunID: run, StepID: step.ID, Model: "m", PromptSHA256: "missing", ResponseSHA256: r}); err == nil {
			t.Error("InsertLLMCall accepted a missing prompt artifact")
		}
		if _, err := st.InsertLLMCall(ctx, store.LLMCall{RunID: run, StepID: step.ID, Model: "m", PromptSHA256: p, ResponseSHA256: r, CostUSD: "0.1; DROP"}); err == nil {
			t.Error("InsertLLMCall accepted a non-decimal cost")
		}
		other := newRun(t, st)
		if _, err := st.InsertLLMCall(ctx, store.LLMCall{RunID: other, StepID: step.ID, Model: "m", PromptSHA256: p, ResponseSHA256: r}); !errors.Is(err, store.ErrStepRunMismatch) {
			t.Errorf("InsertLLMCall for another run's step: %v, want ErrStepRunMismatch", err)
		}
	})
}
