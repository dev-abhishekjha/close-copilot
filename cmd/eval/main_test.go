package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

type ctxKey struct{}

func TestPrimeScope(t *testing.T) {
	const key = "test-scope-key"
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	withScope := func(ctx context.Context, h string) context.Context { return context.WithValue(ctx, ctxKey{}, h) }
	run := func(key, company, month string, calleeErr error) (hdr string, scoped bool, err error) {
		f := primeScope(key, func() time.Time { return now }, withScope, func(ctx context.Context, _, _ string) error {
			hdr, scoped = ctx.Value(ctxKey{}).(string)
			return calleeErr
		})
		err = f(t.Context(), company, month)
		return
	}

	t.Run("scope matches the month", func(t *testing.T) {
		hdr, scoped, err := run(key, "sharma", "2026-09", nil)
		if err != nil || !scoped {
			t.Fatalf("err=%v scoped=%v", err, scoped)
		}
		sc, err := mcpkit.Verify([]byte(key), hdr, now)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := mcpkit.Window("sharma", sc.RunID, "2026-09", time.Unix(sc.Exp, 0))
		if sc != want {
			t.Errorf("scope = %+v, want %+v", sc, want)
		}
		if !regexp.MustCompile(`^eval-prime-[A-Za-z0-9._:-]{1,50}$`).MatchString(sc.RunID) {
			t.Errorf("run ID %q", sc.RunID)
		}
		if d := time.Unix(sc.Exp, 0).Sub(now); d <= 0 || d > 10*time.Minute {
			t.Errorf("expiry %v from now", d)
		}
		if sc.Company != "sharma" || sc.From != "2025-09-01" || sc.To != "2026-10-31" {
			t.Errorf("scope = %+v", sc)
		}
	})
	t.Run("fresh run ID each call", func(t *testing.T) {
		a, _, _ := run(key, "sharma", "2026-09", nil)
		b, _, _ := run(key, "sharma", "2026-09", nil)
		if a == b {
			t.Error("two calls shared a scope header")
		}
	})
	t.Run("empty key passes ctx through", func(t *testing.T) {
		_, scoped, err := run("", "sharma", "2026-09", nil)
		if err != nil || scoped {
			t.Errorf("err=%v scoped=%v, want an unchanged ctx", err, scoped)
		}
	})
	t.Run("bad month errors without a call", func(t *testing.T) {
		called := false
		f := primeScope(key, func() time.Time { return now }, withScope, func(context.Context, string, string) error { called = true; return nil })
		if err := f(t.Context(), "sharma", "2026-13"); err == nil || called {
			t.Errorf("err=%v called=%v", err, called)
		}
	})
	t.Run("bad company errors", func(t *testing.T) {
		if _, _, err := run(key, "", "2026-09", nil); err == nil {
			t.Error("empty company accepted")
		}
	})
	t.Run("callee error is returned", func(t *testing.T) {
		boom := errors.New("books tool: call outside the run scope refused")
		if _, _, err := run(key, "sharma", "2026-09", boom); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
}

// goodKey is a synthetic, test-only pseudonym key (32 bytes as hex).
var goodKey = strings.Repeat("a1", 32)

func TestPseudonymKey(t *testing.T) {
	t.Run("good keys", func(t *testing.T) {
		for _, v := range []string{goodKey, "  " + goodKey + "\n"} {
			key, err := pseudonymKey(config.Config{PseudonymKey: config.NewSecret(v)})
			if err != nil || len(key) != 32 || key[0] != 0xa1 {
				t.Errorf("key of %d chars: %d bytes, %v", len(v), len(key), err)
			}
		}
	})
	t.Run("bad keys", func(t *testing.T) {
		for name, bad := range map[string]string{
			"empty":   "",
			"short":   strings.Repeat("b2", 16),
			"long":    strings.Repeat("b2", 33),
			"non-hex": strings.Repeat("g7", 32),
			"text":    "sekret-pseudonym-zz",
		} {
			_, err := pseudonymKey(config.Config{PseudonymKey: config.NewSecret(bad)})
			if !errors.Is(err, errPseudonymKey) {
				t.Errorf("%s: err %v, want errPseudonymKey", name, err)
				continue
			}
			msg := err.Error()
			if !strings.Contains(msg, config.EnvPseudonymKey) || !strings.Contains(msg, "--no-llm") {
				t.Errorf("%s: error %q must name %s and --no-llm", name, msg, config.EnvPseudonymKey)
			}
			for n := 4; n <= len(bad); n++ {
				if strings.Contains(msg, bad[:n]) {
					t.Errorf("%s: error holds a %d-char prefix of the value", name, n)
					break
				}
			}
		}
	})
	t.Run("tier 2 needs the key", func(t *testing.T) {
		if _, err := explainerKey(config.Config{}, true); !errors.Is(err, errPseudonymKey) {
			t.Errorf("no key: err %v, want errPseudonymKey", err)
		}
		key, err := explainerKey(config.Config{PseudonymKey: config.NewSecret(goodKey)}, true)
		if err != nil || len(key) != 32 {
			t.Errorf("good key: %d bytes, %v", len(key), err)
		}
	})
}

func TestTier1NeedsNoPseudonymKey(t *testing.T) {
	key, err := explainerKey(config.Config{}, false)
	if key != nil || err != nil {
		t.Errorf("agent off: %v, %v; want nil, nil", key, err)
	}
	// Even a malformed key is not read with the agent off.
	key, err = explainerKey(config.Config{PseudonymKey: config.NewSecret("not-hex")}, false)
	if key != nil || err != nil {
		t.Errorf("agent off, bad key: %v, %v; want nil, nil", key, err)
	}
}

// Synthetic identifiers: invented names, a PAN shaped like a company's
// and a GSTIN derived from it (company.GSTIN). Never real ones.
const (
	testPersonA = "Lakshmi Venkatesan"
	testPersonB = "Arvind Kulkarni"
	testPAN     = "ZIACS2610I"
)

func individual(name string) company.Supplier {
	return company.Supplier{ID: strings.ToLower(strings.ReplaceAll(name, " ", "-")), Name: name, Individual: true}
}

func TestNewExplainer(t *testing.T) {
	key, err := pseudonymKey(config.Config{PseudonymKey: config.NewSecret(goodKey)})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("pseudonymiser set", func(t *testing.T) {
		ex := newExplainer(newFakeExplainStore(), staticAccounts{}, config.Config{LLMModelFast: "fast", LLMModelStrong: "strong"}, key, []string{testPersonA}, nil)
		if ex.NewPseudonymiser == nil {
			t.Fatal("NewPseudonymiser is nil")
		}
		if p := ex.NewPseudonymiser(uuid.New()); p == nil {
			t.Fatal("NewPseudonymiser returned nil")
		}
		if ex.Store == nil || ex.Accounts == nil || ex.FastModel != "fast" || ex.StrongModel != "strong" {
			t.Errorf("explainer fields: %+v", ex)
		}
	})
	t.Run("names union over the suite's companies", func(t *testing.T) {
		byID := map[string]company.Profile{
			"sharma": {ID: "sharma", Suppliers: []company.Supplier{individual(testPersonA), {ID: "acme", Name: "Acme Pvt Ltd"}, individual(testPersonB)}},
			"mehta":  {ID: "mehta", Suppliers: []company.Supplier{individual(testPersonB), individual("Meera Pillai")}},
			"other":  {ID: "other", Suppliers: []company.Supplier{individual("Not In Suite")}},
		}
		months := []evals.SuiteMonth{
			{Company: "sharma", Month: "2026-08"}, {Company: "sharma", Month: "2026-09"},
			{Company: "mehta", Month: "2026-09"}, {Company: "missing", Month: "2026-09"},
		}
		got := suiteIndividualNames(byID, months)
		want := []string{testPersonA, testPersonB, "Meera Pillai"}
		if !slices.Equal(got, want) {
			t.Errorf("names = %q, want %q", got, want)
		}
		if got := suiteIndividualNames(byID, nil); len(got) != 0 {
			t.Errorf("no months: names = %q", got)
		}
	})
}

func TestExplainerInputMasked(t *testing.T) {
	key, err := pseudonymKey(config.Config{PseudonymKey: config.NewSecret(goodKey)})
	if err != nil {
		t.Fatal(err)
	}
	gstin, err := company.GSTIN("29", testPAN)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{testPersonA}
	st := newFakeExplainStore()
	snap := map[string]any{
		"server": agent.ServerEvidence, "tool": "list_bank_lines",
		"args": map[string]any{"company": "sharma", "from_date": "2026-09-01", "to_date": "2026-09-30"},
		"result": []map[string]any{{
			"company_id": "sharma", "txn_id": "HDFC-20260915-C1", "txn_date": "2026-09-15",
			"narration": "NEFT " + strings.ToUpper(testPersonA) + " " + gstin + " PAN " + testPAN, "amount_paise": -590,
		}},
	}
	sha, err := st.PutArtifact(t.Context(), store.ArtifactToolResult, uuid.Nil, uuid.Nil, snap)
	if err != nil {
		t.Fatal(err)
	}
	amt := money.Paise(590)
	runID := uuid.New()
	model := llm.NewFakeProvider()
	req := agent.ExplainRequest{
		RunID: runID, StepID: uuid.New(), Attempt: 1, Company: "sharma", Month: "2026-09",
		Finding: store.Finding{
			ID: uuid.New(), RunID: runID, Type: "unrecorded_bank_charge", Severity: "low",
			Title:       "Unrecorded payment to " + testPersonA + " (" + gstin + ")",
			AmountPaise: &amt,
			Keys:        map[string]string{"bank_txn_id": "HDFC-20260915-C1", "supplier_gstin": gstin},
			Evidence: []store.EvidenceRef{{Server: agent.ServerEvidence, Tool: "list_bank_lines",
				Args: json.RawMessage(`{"company":"sharma"}`), IDs: []string{"HDFC-20260915-C1"}, Artifact: sha}},
			Status: "open",
		},
		EvidenceRefs: []string{sha},
		Model:        model,
	}
	accounts := staticAccounts{"Bank Charges - STPL", testPersonA + " Capital - STPL"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ex := newExplainer(st, accounts, config.Config{LLMModelFast: "fast"}, key, names, log)
	_, _ = ex.Explain(t.Context(), req) // the fake's answer is malformed; only the requests matter

	calls := model.Calls()
	if len(calls) == 0 {
		t.Fatal("the model was never called")
	}
	r := agent.NewRedactor(key, runID, names)
	for i, c := range calls {
		var b strings.Builder
		for _, blk := range c.System {
			b.WriteString(blk.Text)
		}
		for _, m := range c.Messages {
			b.WriteString(m.Content)
		}
		text := b.String()
		for _, raw := range []string{gstin, testPAN, testPersonA, strings.ToUpper(testPersonA)} {
			if strings.Contains(text, raw) {
				t.Errorf("call %d holds a raw identifier (%d chars)", i, len(raw))
			}
		}
		for _, masked := range []string{r.MaskString(gstin), r.MaskString(testPAN), r.MaskString(testPersonA)} {
			if !strings.Contains(text, masked) {
				t.Errorf("call %d lacks the masked form %q", i, masked)
			}
		}
	}
}

func TestRunSuiteNeedsPseudonymKey(t *testing.T) {
	base := []string{"run", "--suite", "suite-skeleton", "--scenarios-dir", "../../evals/scenarios", "--config-dir", "../../config"}
	// A URL that fails at parse: the run never dials a database.
	cfg := config.Config{DatabaseURL: config.NewSecret("::not-a-url")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name    string
		mode    []string
		wantKey bool
	}{
		{"agent on", nil, true},
		{"replay, agent on", []string{"--replay", "--fixtures-dir", t.TempDir(), "--results-dir", t.TempDir()}, true},
		{"no-llm", []string{"--no-llm", "--results-dir", t.TempDir()}, false},
		{"no-agent", []string{"--no-agent", "--results-dir", t.TempDir()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t.Context(), cfg, log, append(slices.Clone(base), tc.mode...))
			if got := errors.Is(err, errPseudonymKey); got != tc.wantKey {
				t.Errorf("err = %v; errPseudonymKey %v, want %v", err, got, tc.wantKey)
			}
			if !tc.wantKey && err == nil {
				t.Error("want the store error, got nil")
			}
		})
	}
}

// staticAccounts is a fixed account list.
type staticAccounts []string

func (a staticAccounts) Accounts(context.Context, string, string) ([]string, error) {
	return slices.Clone(a), nil
}

// fakeExplainStore keeps artifacts in memory, hashed as the store hashes
// them, and records explanations.
type fakeExplainStore struct {
	mu        sync.Mutex
	artifacts map[string]store.Artifact
}

func newFakeExplainStore() *fakeExplainStore {
	return &fakeExplainStore{artifacts: map[string]store.Artifact{}}
}

func (s *fakeExplainStore) PutArtifact(_ context.Context, kind string, _, _ uuid.UUID, v any) (string, error) {
	canon, sha, err := store.CanonicalHash(v)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[sha] = store.Artifact{SHA256: sha, Kind: kind, Content: canon}
	return sha, nil
}

func (s *fakeExplainStore) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.artifacts[sha]
	if !ok {
		return store.Artifact{}, fmt.Errorf("%w: artifact %s", store.ErrNotFound, sha)
	}
	return a, nil
}

func (s *fakeExplainStore) SetFindingExplanation(context.Context, uuid.UUID, uuid.UUID, string, string, string, []store.Citation, *store.JournalProposal) error {
	return nil
}

type cannedDocs struct{ ps []agent.Passage }

func (c cannedDocs) Retrieve(context.Context, agent.RetrieveRequest) ([]agent.Passage, error) {
	return c.ps, nil
}

// recordRP records the replay test suite, with the given documents (nil
// records none), and returns an opened replay.
func recordRP(t *testing.T, docs agent.Retriever) *evals.Replayer {
	t.Helper()
	scen := replaySuite(t)
	suite, err := evals.LoadSuite(filepath.Join(scen, "suite-rp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	truth := filepath.Join(scen, "suite-rp", "ground_truth")
	root := filepath.Join(t.TempDir(), "fixtures")
	rec, err := evals.NewRecorder(root, suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Abort()
	for _, m := range suite.Months {
		rec.UseMonth(m.Company, m.Month)
		if docs == nil {
			continue
		}
		if _, err := rec.Documents(docs).Retrieve(t.Context(), agent.RetrieveRequest{Company: m.Company, Month: m.Month, Query: "prepaid not spread"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := rec.Finish(t.Context(), evals.RecordMeta{Suite: suite, TruthDir: truth, Months: suite.Months, SeederCommit: "dev-test",
		RecordedAt: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	rp, _, err := evals.OpenReplay(evals.ReplayOptions{Dir: root, Suite: suite, TruthDir: truth, Months: suite.Months})
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

// TestReplayRetriever: replay serves the recorded passages through the
// agent's Retriever with no registry and no network, and a query that was
// never recorded is ErrFixtureMissing.
func TestReplayRetriever(t *testing.T) {
	want := []agent.Passage{{DocID: "POL-1", DocType: "policy", Section: "§1 Spread", Title: "Policy", Text: "Spread annual costs monthly.", SHA256: "ab"}}
	rp := recordRP(t, cannedDocs{ps: want})
	r := replayRetriever(rp, slog.New(slog.NewTextHandler(io.Discard, nil)), "suite-rp")
	if r == nil {
		t.Fatal("no retriever for a suite recorded with searches")
	}
	rp.UseMonth("sharma", "2026-09")
	got, err := r.Retrieve(t.Context(), agent.RetrieveRequest{Company: "sharma", Month: "2026-09", Query: "prepaid not spread"})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("replayed %+v, %v; want %+v", got, err, want)
	}
	_, err = r.Retrieve(t.Context(), agent.RetrieveRequest{Company: "sharma", Month: "2026-09", Query: "never recorded"})
	if !errors.Is(err, evals.ErrFixtureMissing) {
		t.Errorf("missing fixture: %v", err)
	}
	// The clean control month was recorded too, under its own scope.
	rp.UseMonth("sharma", "2026-08")
	if got, err := r.Retrieve(t.Context(), agent.RetrieveRequest{Company: "sharma", Month: "2026-08", Query: "prepaid not spread"}); err != nil || len(got) != 1 {
		t.Errorf("control month: %+v, %v", got, err)
	}
}

// TestReplayRetrieverNone: a suite recorded without search fixtures (the
// committed suites) replays with no retriever, a true nil interface, so
// the workflow records its single skipped retrieve step as before.
func TestReplayRetrieverNone(t *testing.T) {
	rp := recordRP(t, nil)
	if r := replayRetriever(rp, slog.New(slog.NewTextHandler(io.Discard, nil)), "suite-rp"); r != nil {
		t.Errorf("retriever %T for a suite with no search fixtures", r)
	}
}

func TestFaultsReport(t *testing.T) {
	clean := evals.Score{Items: []evals.ItemOutcome{
		{Key: "sharma-2026-09/E01", Type: "unrecorded_bank_charge", Outcome: evals.OutcomeCaught},
		{Key: "sharma-2026-09/E02", Type: "unrecorded_bank_charge", Outcome: evals.OutcomeCaught},
	}}
	clean.Overall.Recall = evals.NewRatio(2, 2)
	clean.Overall.Precision = evals.NewRatio(2, 2)
	faulted := evals.Score{
		Items:      []evals.ItemOutcome{{Key: "sharma-2026-09/E01", Outcome: evals.OutcomeCaught}, {Key: "sharma-2026-09/E02", Outcome: evals.OutcomeMissed}},
		FailedRuns: []evals.RunRef{{Company: "sharma", Month: "2026-08", Reason: "preflight: books are unreachable: ERPNext unavailable; agent: run failed: x"}},
	}
	faulted.Overall.Recall = evals.NewRatio(1, 2)
	faulted.Overall.Precision = evals.NewRatio(1, 1)
	md := faultsReport("suite-x", "erp:error=503:rate=0.1", 1, clean, faulted)
	for _, want := range []string{
		"`erp:error=503:rate=0.1`, seed 1",
		"| Recall | 100.0% (2/2) | 50.0% (1/2) |",
		"- `sharma-2026-09/E02` (unrecorded_bank_charge) caught without faults, missed with them",
		"- sharma 2026-08: preflight: books are unreachable: ERPNext unavailable\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "E01") {
		t.Errorf("report lists E01, which was still caught:\n%s", md)
	}
	if none := faultsReport("s", "x", 0, clean, clean); !strings.Contains(none, "None: every item") {
		t.Errorf("no loss not stated:\n%s", none)
	}
}
