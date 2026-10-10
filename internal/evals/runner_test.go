package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fakeRun is what the fake closer and reader return for one month.
type fakeRun struct {
	status, reason string
	err            error
	noRun          bool // RunClose fails before creating a run
	findings       []store.Finding
	calls          []store.LLMCall
	readErr        error
}

// fakeStack is a fake Closer and RunReader over planned runs.
type fakeStack struct {
	plan   map[string]fakeRun // by company:month
	calls  []string
	runs   map[uuid.UUID]string
	nextID int
	cancel context.CancelFunc // cancels the suite after the first close
}

func newFake(plan map[string]fakeRun) *fakeStack {
	return &fakeStack{plan: plan, runs: map[uuid.UUID]string{}}
}

func (f *fakeStack) RunClose(_ context.Context, company, month string) (agent.Result, error) {
	key := company + ":" + month
	f.calls = append(f.calls, key)
	if f.cancel != nil {
		f.cancel()
	}
	p, ok := f.plan[key]
	if !ok {
		p = fakeRun{status: store.RunPartial}
	}
	if p.noRun {
		return agent.Result{}, p.err
	}
	f.nextID++
	id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextID))
	f.runs[id] = key
	res := agent.Result{RunID: id, Status: p.status, Reason: p.reason, ReportPath: "results/runs/" + id.String() + ".md", Findings: len(p.findings)}
	return res, p.err
}

func (f *fakeStack) run(id uuid.UUID) (fakeRun, error) {
	key, ok := f.runs[id]
	if !ok {
		return fakeRun{}, store.ErrNotFound
	}
	p := f.plan[key]
	if p.status == "" {
		p.status = store.RunPartial
	}
	return p, p.readErr
}

func (f *fakeStack) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	p, err := f.run(id)
	if err != nil {
		return store.CloseRun{}, err
	}
	start := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Second)
	r := store.CloseRun{ID: id, Status: p.status, StartedAt: &start, FinishedAt: &end, CostUSD: 99}
	if p.reason != "" {
		r.Error = &p.reason
	}
	return r, nil
}

func (f *fakeStack) ListFindingsByRun(_ context.Context, id uuid.UUID) ([]store.Finding, error) {
	p, err := f.run(id)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(p.findings)
	for i := range out {
		out[i].RunID = id
	}
	return out, nil
}

// stepFor maps a run to two fake steps, so calls spread over steps.
func stepFor(run uuid.UUID, n int) uuid.UUID {
	b := run
	b[0] = byte(n + 1)
	return b
}

func (f *fakeStack) ListSteps(_ context.Context, id uuid.UUID) ([]store.Step, error) {
	if _, err := f.run(id); err != nil {
		return nil, err
	}
	return []store.Step{{ID: stepFor(id, 0), RunID: id}, {ID: stepFor(id, 1), RunID: id}}, nil
}

func (f *fakeStack) ListLLMCalls(_ context.Context, stepID uuid.UUID) ([]store.LLMCall, error) {
	for id := range f.runs {
		for n := range 2 {
			if stepFor(id, n) == stepID {
				p, _ := f.run(id)
				var out []store.LLMCall
				for i, c := range p.calls {
					if i%2 == n {
						out = append(out, c)
					}
				}
				return out, nil
			}
		}
	}
	return nil, nil
}

// clock is a fake clock that ticks 1.5 s on every reading.
func clock() func() time.Time {
	t := time.Date(2026, 10, 10, 8, 30, 0, 0, time.UTC)
	return func() time.Time {
		now := t
		t = t.Add(1500 * time.Millisecond)
		return now
	}
}

func bankCharge(ref string, paise int64) store.Finding {
	amt := money.Paise(paise)
	return store.Finding{
		ID:          uuid.MustParse("11111111-1111-4111-8111-" + fmt.Sprintf("%012d", paise)),
		Type:        "unrecorded_bank_charge",
		Severity:    "medium",
		Title:       "Unrecorded bank charge " + ref,
		AmountPaise: &amt,
		Keys:        map[string]string{"bank_ref": ref, "account": "HDFC Current 0001 - STPL"},
		Evidence: []store.EvidenceRef{{
			Server: "evidence", Tool: "get_bank_lines", Args: json.RawMessage(`{"company":"sharma","from":"2026-09-01","to":"2026-09-30"}`),
			IDs: []string{ref}, Artifact: "ab12cd34",
		}},
		Status: "open",
	}
}

var testSuite = Suite{
	Name: "suite-test", Path: "evals/scenarios/suite-test.yaml", SHA256: "0123abcd",
	Months: []SuiteMonth{
		{Company: "sharma", Month: "2026-09"},
		{Company: "mehta", Month: "2026-09"},
		{Company: "sharma", Month: "2026-08", Control: true},
	},
}

func newRunner(t *testing.T, f *fakeStack) *Runner {
	t.Helper()
	return &Runner{
		Closer: f, Reader: f,
		ResultsDir: t.TempDir(),
		Config:     ManifestConfig{LLMProvider: "claude-cli", LLMModelFast: "haiku", LLMModelStrong: "sonnet", LLMRunTokenCap: 200000, LLMDailyBudgetUSD: "2"},
		Flags:      Flags{Suite: "suite-test", ResultsDir: "results", ScenariosDir: "evals/scenarios", ConfigDir: "config"},
		Commit:     "dev-abc123",
		Now:        clock(),
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// golden compares the file at got with testdata/<name>, or rewrites it
// with -update.
func golden(t *testing.T, got, name string) {
	t.Helper()
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Errorf("%s differs from %s\n got:\n%s\nwant:\n%s", got, path, b, want)
	}
}

func TestRunnerWritesResultsAndManifest(t *testing.T) {
	f := newFake(map[string]fakeRun{
		"sharma:2026-09": {
			status:   store.RunPartial,
			findings: []store.Finding{bankCharge("BC-1", 11800), bankCharge("BC-2", 59000), bankCharge("BC-3", 35400)},
			calls: []store.LLMCall{
				{Model: "sonnet", InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, CostUSD: "0.0123"},
				{Model: "haiku", InputTokens: 10, OutputTokens: 5, CostUSD: "0.5"},
				{Model: "sonnet", InputTokens: 1, OutputTokens: 1, CostUSD: "0.000001"},
				{Model: "", CostUSD: ""},
			},
		},
		"mehta:2026-09":  {status: store.RunDone},
		"sharma:2026-08": {status: store.RunPartial},
	})
	r := newRunner(t, f)
	dir, man, err := r.Run(t.Context(), testSuite, testSuite.Months)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := filepath.Join(r.ResultsDir, "suite-test", "20261010T083000Z"); dir != want {
		t.Errorf("folder %s, want %s", dir, want)
	}
	if want := []string{"sharma:2026-09", "mehta:2026-09", "sharma:2026-08"}; !slices.Equal(f.calls, want) {
		t.Errorf("closes ran %v, want %v in suite order", f.calls, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"manifest.json", "mehta-2026-09.json", "sharma-2026-08.json", "sharma-2026-09.json"}; !slices.Equal(names, want) {
		t.Errorf("folder holds %v, want %v (no temp files)", names, want)
	}
	if man.Failed != 0 || len(man.Results) != 3 {
		t.Errorf("manifest failed %d, results %d", man.Failed, len(man.Results))
	}

	var res Result
	readJSON(t, filepath.Join(dir, "sharma-2026-09.json"), &res)
	if res.CostUSD != "0.512301" {
		t.Errorf("cost %q, want the exact sum 0.512301", res.CostUSD)
	}
	if res.Tokens != (Tokens{Input: 1011, Output: 206, CacheRead: 50}) {
		t.Errorf("tokens %+v", res.Tokens)
	}
	if want := []string{"haiku", "sonnet"}; !slices.Equal(res.Models.Called, want) || res.Models.Fast != "haiku" || res.Models.Strong != "sonnet" {
		t.Errorf("models %+v", res.Models)
	}
	if len(res.Findings) != 3 || res.Findings[0].Keys["bank_ref"] != "BC-1" || res.Findings[0].Evidence[0].Artifact != "ab12cd34" {
		t.Errorf("findings %+v", res.Findings)
	}
	if res.TraceID != nil || res.RunID == nil || res.Commit != "dev-abc123" || res.DurationMS != 1500 {
		t.Errorf("result metadata trace %v run %v commit %q duration %d", res.TraceID, res.RunID, res.Commit, res.DurationMS)
	}

	golden(t, filepath.Join(dir, "sharma-2026-09.json"), "result-sharma-2026-09.json")
	golden(t, filepath.Join(dir, "sharma-2026-08.json"), "result-sharma-2026-08.json")
	golden(t, filepath.Join(dir, ManifestFile), "manifest.json")
	for _, n := range []string{"sharma-2026-09.json", ManifestFile} {
		checkSortedIndented(t, filepath.Join(dir, n))
	}
}

// checkSortedIndented checks that every object in the file has its keys
// in sorted order and that the file is 2-space indented.
func checkSortedIndented(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ind bytes.Buffer
	if err := json.Indent(&ind, bytes.TrimSpace(b), "", "  "); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(b), ind.Bytes()) {
		t.Errorf("%s is not 2-space indented", path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	type frame struct {
		object  bool
		wantKey bool
		last    string
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		top := len(stack) - 1
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				if top >= 0 && stack[top].object {
					stack[top].wantKey = true
				}
				stack = append(stack, frame{object: d == '{', wantKey: d == '{'})
			} else {
				stack = stack[:top]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].wantKey = true
				}
			}
			continue
		}
		if top >= 0 && stack[top].object {
			if stack[top].wantKey {
				k := tok.(string)
				if stack[top].last != "" && k <= stack[top].last {
					t.Errorf("%s: key %q after %q is out of order", path, k, stack[top].last)
				}
				stack[top].last = k
				stack[top].wantKey = false
			} else {
				stack[top].wantKey = true
			}
		}
	}
}

func TestRunnerFailedMonthContinues(t *testing.T) {
	f := newFake(map[string]fakeRun{
		"sharma:2026-09": {status: store.RunFailed, reason: "preflight: no bank statement loaded", err: agent.ErrRunFailed},
		"mehta:2026-09":  {noRun: true, err: agent.ErrUnknownCompany},
		"sharma:2026-08": {status: store.RunPartial},
	})
	r := newRunner(t, f)
	dir, man, err := r.Run(t.Context(), testSuite, testSuite.Months)
	if !errors.Is(err, ErrRunsFailed) {
		t.Fatalf("Run = %v, want ErrRunsFailed", err)
	}
	if !strings.Contains(err.Error(), "sharma:2026-09") || !strings.Contains(err.Error(), "mehta:2026-09") {
		t.Errorf("error %q does not name the failed months", err)
	}
	if len(f.calls) != 3 {
		t.Errorf("ran %v; a failed month must not stop the suite", f.calls)
	}
	if man.Failed != 2 {
		t.Errorf("manifest failed = %d, want 2", man.Failed)
	}
	var on Manifest
	readJSON(t, filepath.Join(dir, ManifestFile), &on)
	got := map[string]ManifestEntry{}
	for _, e := range on.Results {
		got[e.Company+":"+e.Month] = e
	}
	if e := got["sharma:2026-09"]; !e.Failed || e.Status != store.RunFailed || e.Reason == "" || e.RunID == nil || e.File != "sharma-2026-09.json" {
		t.Errorf("failed run entry %+v", e)
	}
	if e := got["mehta:2026-09"]; !e.Failed || e.Status != store.RunFailed || e.RunID != nil {
		t.Errorf("no-run entry %+v", e)
	}
	if e := got["sharma:2026-08"]; e.Failed || e.Status != store.RunPartial {
		t.Errorf("partial entry %+v; partial is not a failure", e)
	}
	var res Result
	readJSON(t, filepath.Join(dir, "mehta-2026-09.json"), &res)
	if res.RunID != nil || res.Status != store.RunFailed || !strings.Contains(res.Error, "unknown company") {
		t.Errorf("no-run result %+v", res)
	}
}

func TestRunnerOnly(t *testing.T) {
	f := newFake(nil)
	r := newRunner(t, f)
	months, err := testSuite.Filter([]string{"sharma:2026-08"})
	if err != nil {
		t.Fatal(err)
	}
	r.Flags.Only = []string{"sharma:2026-08"}
	dir, man, err := r.Run(t.Context(), testSuite, months)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"sharma:2026-08"}) {
		t.Errorf("ran %v", f.calls)
	}
	if len(man.Results) != 1 || !man.Results[0].Control {
		t.Errorf("manifest results %+v", man.Results)
	}
	var on Manifest
	readJSON(t, filepath.Join(dir, ManifestFile), &on)
	if !slices.Equal(on.Flags.Only, []string{"sharma:2026-08"}) {
		t.Errorf("manifest flags %+v", on.Flags)
	}
}

func TestRunnerExportError(t *testing.T) {
	f := newFake(map[string]fakeRun{"sharma:2026-09": {status: store.RunPartial, readErr: errors.New("db gone")}})
	r := newRunner(t, f)
	_, man, err := r.Run(t.Context(), testSuite, testSuite.Months[:1])
	if !errors.Is(err, ErrRunsFailed) || !man.Results[0].Failed {
		t.Errorf("Run = %v, entry %+v; an export that fails counts as failed", err, man.Results[0])
	}
}

func TestRunnerCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := newFake(nil)
	f.cancel = cancel
	r := newRunner(t, f)
	dir, man, err := r.Run(ctx, testSuite, testSuite.Months)
	if !errors.Is(err, ErrRunsFailed) {
		t.Fatalf("Run = %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("ran %v after the cancel", f.calls)
	}
	if man.Failed != 2 || man.Results[1].Reason != agent.ReasonCancelled || man.Results[1].File != "" {
		t.Errorf("manifest %+v", man.Results)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err != nil {
		t.Errorf("no manifest after a cancel: %v", err)
	}
}

func TestRunnerRefusesExistingFolder(t *testing.T) {
	f := newFake(nil)
	r := newRunner(t, f)
	if err := os.MkdirAll(filepath.Join(r.ResultsDir, "suite-test", "20261010T083000Z"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Run(t.Context(), testSuite, testSuite.Months); err == nil || len(f.calls) != 0 {
		t.Errorf("Run over an existing folder = %v after %v closes", err, f.calls)
	}
	if _, _, err := r.Run(t.Context(), testSuite, nil); err == nil {
		t.Error("Run with no months succeeded")
	}
}

// TestManifestConfigHasNoSecrets checks that the manifest's config type
// can't hold a config.Secret, and that a manifest written from a config
// full of secrets carries none of them.
func TestManifestConfigHasNoSecrets(t *testing.T) {
	secretT := reflect.TypeFor[config.Secret]()
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Struct:
			if rt == secretT {
				t.Errorf("%s is a config.Secret", path)
				return
			}
			if rt == reflect.TypeFor[config.Config]() {
				t.Errorf("%s is a whole config.Config", path)
				return
			}
			for i := range rt.NumField() {
				walk(rt.Field(i).Type, path+"."+rt.Field(i).Name)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(rt.Elem(), path+"[]")
		}
	}
	walk(reflect.TypeFor[Manifest](), "Manifest")
	walk(reflect.TypeFor[Result](), "Result")
}

func TestSumDecimals(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{nil, "0"},
		{[]string{""}, "0"},
		{[]string{"0.0123", "0.5"}, "0.5123"},
		{[]string{"1", "2"}, "3"},
		{[]string{"0.1", "0.2"}, "0.3"},
		{[]string{"0.000001", "0.999999"}, "1"},
		{[]string{"12.340000"}, "12.34"},
	}
	for _, tt := range tests {
		got, err := sumDecimals(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("sumDecimals(%v) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"1e3", "1/3", "x", "NaN"} {
		if _, err := sumDecimals([]string{bad}); err == nil {
			t.Errorf("sumDecimals(%q) accepted it", bad)
		}
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := writeFileAtomic(path, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new\n" {
		t.Errorf("content %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("folder holds %d entries, want only a.json (no temp files)", len(entries))
	}

	// A rename that fails (the target is a folder) leaves the folder as it
	// was: no temp file, no partial file.
	target := filepath.Join(dir, "sub")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x")); err == nil {
		t.Error("write over a non-empty folder succeeded")
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("after a failed write the folder holds %v", names)
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing", "b.json"), []byte("x")); err == nil {
		t.Error("write into a missing folder succeeded")
	}
}

// TestRunnerVerifierCounts: a verify step's earlier attempts are rejects,
// a done step with a reason is a last-attempt reject, and every explain
// attempt after the first is a retry.
func TestRunnerVerifierCounts(t *testing.T) {
	reason := "V_AMOUNT after 3 explain attempts"
	empty := ""
	steps := []store.Step{
		{Kind: store.StepKindExplain, Status: store.StepDone, Attempt: 1},                // passed first time
		{Kind: store.StepKindVerify, Status: store.StepDone, Attempt: 1},                 // passed
		{Kind: store.StepKindExplain, Status: store.StepDone, Attempt: 2},                // one retry
		{Kind: store.StepKindVerify, Status: store.StepDone, Attempt: 2, Error: &empty},  // one reject, then passed
		{Kind: store.StepKindExplain, Status: store.StepDone, Attempt: 3},                // two retries
		{Kind: store.StepKindVerify, Status: store.StepDone, Attempt: 3, Error: &reason}, // three rejects
		{Kind: "check.bank_rec", Status: store.StepDone, Attempt: 4},                     // not counted
	}
	var res Result
	for _, st := range steps {
		countVerification(st, &res)
	}
	if res.VerifierRejects != 4 || res.Retries != 3 {
		t.Errorf("rejects %d retries %d, want 4 and 3", res.VerifierRejects, res.Retries)
	}
	b, _ := json.Marshal(Result{})
	if strings.Contains(string(b), "verifier_rejects") {
		t.Errorf("a result without the agent carries verifier counts: %s", b)
	}
}
