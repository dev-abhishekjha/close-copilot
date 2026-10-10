package evals

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Synthetic identifiers for the fixture tests (checksum-valid shapes,
// made up; passed to the scan as its extra allowlist).
const (
	fxCompanyGSTIN  = "27AAACS1234A1Z2"
	fxSupplierGSTIN = "27AABCV5678B1Z8"
)

var fxExtra = []string{fxCompanyGSTIN, fxSupplierGSTIN}

// fakeLive is a live reader with synthetic data; it counts its calls and
// can fail on one tool.
type fakeLive struct {
	calls  int
	failOn string
	// vary changes the trial balance on every call (a non-deterministic
	// live reader).
	vary bool
}

var errLive = errors.New("live: connection refused")

func (f *fakeLive) hit(tool string) error {
	f.calls++
	if tool == f.failOn {
		return errLive
	}
	return nil
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func (f *fakeLive) TrialBalance(_ context.Context, company string, from, to time.Time) (ledger.TB, error) {
	if err := f.hit(ToolTrialBalance); err != nil {
		return ledger.TB{}, err
	}
	tb := ledger.TB{Company: company, From: from, To: to, Rows: []ledger.TBRow{{Account: "Bank Charges - STPL", Debit: 61360}}}
	if f.vary {
		tb.Rows[0].Debit += money.Paise(f.calls)
	}
	return tb, nil
}

func (f *fakeLive) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	return nil, f.hit(ToolGLEntries)
}

func (f *fakeLive) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	return []ledger.PurchaseInvoice{}, f.hit(ToolPurchaseInvoices)
}

func (f *fakeLive) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	return nil, f.hit(ToolSalesInvoices)
}

func (f *fakeLive) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	return nil, f.hit(ToolPayments)
}

func (f *fakeLive) AccountHistory(_ context.Context, _, _ string, through string, _ int) ([]ledger.MonthTotal, error) {
	if err := f.hit(ToolAccountHistory); err != nil {
		return nil, err
	}
	return []ledger.MonthTotal{{Month: through}}, nil
}

func (f *fakeLive) RecurringSuppliers(context.Context, string, string, int, int, int) ([]checks.RecurringSupplier, error) {
	if err := f.hit(ToolRecurringSuppliers); err != nil {
		return nil, err
	}
	return []checks.RecurringSupplier{{Supplier: "Vendor A", MedianAmount: 1234567, TypicalDay: 5, MonthsSeen: []string{"2026-07", "2026-08"}}}, nil
}

func (f *fakeLive) BankLines(_ context.Context, company string, _, _ time.Time) ([]store.BankLine, error) {
	if err := f.hit(ToolBankLines); err != nil {
		return nil, err
	}
	bal := money.Paise(9_223_372_036_854_775_807) // the largest paise value survives exactly
	return []store.BankLine{
		{CompanyID: company, TxnID: "HDFC-20260915-C1", TxnDate: day("2026-09-15"), Narration: "NEFT CHARGES INCL GST", AmountPaise: -590, BalancePaise: &bal, SourceFile: "bank/sharma-2026-09.csv"},
		{CompanyID: company, TxnID: "HDFC-20260920-C2", TxnDate: day("2026-09-20"), Narration: "DEBIT CARD ANNUAL FEE", AmountPaise: -59000, SourceFile: "bank/sharma-2026-09.csv"},
	}, nil
}

func (f *fakeLive) GSTR2BEntries(_ context.Context, company, period string) ([]store.GSTR2BEntry, error) {
	if err := f.hit(ToolGSTR2BEntries); err != nil {
		return nil, err
	}
	return []store.GSTR2BEntry{{CompanyID: company, Period: period, SupplierGSTIN: fxSupplierGSTIN, InvoiceNo: "INV/1",
		InvoiceNoNorm: "INV1", InvoiceDate: day("2026-09-03"), TaxablePaise: 100000, IGSTPaise: 18000, ITCAvailable: true}}, nil
}

// fxEnv is a suite, its ground truth and a fixtures root.
type fxEnv struct {
	suite    Suite
	truthDir string
	root     string
	months   []SuiteMonth
}

func newFxEnv(t *testing.T) fxEnv {
	t.Helper()
	scen := t.TempDir()
	path := writeSuite(t, "suite-fx", "suite: suite-fx\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n")
	suite, err := LoadSuite(path)
	if err != nil {
		t.Fatal(err)
	}
	truthDir := filepath.Join(scen, "ground_truth")
	if err := os.MkdirAll(truthDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, m := range suite.Months {
		body := `{"scenario":"suite-fx","company":"sharma","month":"` + m.Month + `","clean":` + map[bool]string{true: "true", false: "false"}[m.Control] +
			`,"planted":[],"expected":[],"investigations":[]}`
		if err := os.WriteFile(filepath.Join(truthDir, TruthFileName(m.Company, m.Month)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fxEnv{suite: suite, truthDir: truthDir, root: filepath.Join(t.TempDir(), "fixtures"), months: suite.Months}
}

// exercise calls every reader method in both months.
func exercise(t *testing.T, scope MonthScope, b checks.BooksReader, e checks.EvidenceReader) map[string]any {
	t.Helper()
	ctx := t.Context()
	out := map[string]any{}
	for _, m := range []string{"2026-09", "2026-08"} {
		scope.UseMonth("sharma", m)
		from, _ := time.Parse("2006-01", m)
		to := from.AddDate(0, 1, -1)
		must := func(name string, v any, err error) {
			if err != nil {
				t.Fatalf("%s %s: %v", m, name, err)
			}
			out[m+"/"+name] = v
		}
		tb, err := b.TrialBalance(ctx, "sharma", from, to)
		must("tb", tb, err)
		gl, err := b.GLEntries(ctx, "sharma", from, to)
		must("gl", gl, err)
		pi, err := b.PurchaseInvoices(ctx, "sharma", from, to)
		must("pi", pi, err)
		si, err := b.SalesInvoices(ctx, "sharma", from, to)
		must("si", si, err)
		pe, err := b.PaymentEntries(ctx, "sharma", from, to)
		must("pe", pe, err)
		ah, err := b.AccountHistory(ctx, "sharma", "Bank Charges - STPL", m, 6)
		must("ah", ah, err)
		rs, err := b.RecurringSuppliers(ctx, "sharma", m, 6, 3, 10)
		must("rs", rs, err)
		bl, err := e.BankLines(ctx, "sharma", from, to)
		must("bl", bl, err)
		g2, err := e.GSTR2BEntries(ctx, "sharma", m)
		must("g2", g2, err)
	}
	return out
}

// record records every call into env.root and returns the live results.
func record(t *testing.T, env fxEnv, live *fakeLive) map[string]any {
	t.Helper()
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Abort)
	got := exercise(t, rec, rec.Books(live), rec.Evidence(live))
	if _, _, err := rec.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months,
		SeederCommit: "dev-test", RecordedAt: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC), ExtraAllowed: fxExtra}); err != nil {
		t.Fatal(err)
	}
	return got
}

func openReplay(env fxEnv) (*Replayer, string, error) {
	return OpenReplay(ReplayOptions{Dir: env.root, Suite: env.suite, TruthDir: env.truthDir, Months: env.months})
}

func TestFixtureArgsHashStable(t *testing.T) {
	a, err := canonicalArgs(map[string]any{"company": "sharma", "from": "2026-09-01", "to": "2026-09-30"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := canonicalArgs(map[string]any{"to": "2026-09-30", "company": "sharma", "from": "2026-09-01"})
	if string(a) != string(b) || argsHash(a) != argsHash(b) {
		t.Errorf("key order changed the args: %s vs %s", a, b)
	}
	if string(a) != `{"company":"sharma","from":"2026-09-01","to":"2026-09-30"}` || len(argsHash(a)) != 16 {
		t.Errorf("canonical %s hash %s", a, argsHash(a))
	}
	// Stored args re-encode to the same bytes, whatever their spacing.
	re, err := recanonical([]byte(`{ "to" : "2026-09-30", "from":"2026-09-01","company":"sharma" }`))
	if err != nil || string(re) != string(a) {
		t.Errorf("recanonical = %s, %v", re, err)
	}
	n, _ := canonicalArgs(accountHistoryArgs("sharma", "A", "2026-09", 6))
	rn, _ := recanonical(n)
	if string(n) != string(rn) {
		t.Errorf("integer args changed on re-encoding: %s vs %s", n, rn)
	}
	if dateArg(day("2026-09-01").In(time.FixedZone("IST", 19800))) != "2026-09-01" || dateArg(time.Time{}) != "" {
		t.Error("dateArg")
	}
}

func TestRecordReplayUnit(t *testing.T) {
	env := newFxEnv(t)
	live := &fakeLive{}
	want := record(t, env, live)

	rp, sum, err := openReplay(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 64 || rp.Index().SeederCommit != "dev-test" || len(rp.Index().Files) != 18 || len(rp.Index().Truth) != 2 {
		t.Errorf("index %+v sha %s", rp.Index(), sum)
	}
	calls := live.calls
	got := exercise(t, rp, rp.Books(), rp.Evidence())
	if live.calls != calls {
		t.Errorf("replay made %d live calls", live.calls-calls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replayed results differ:\n got %v\nwant %v", got, want)
	}

	// Every file is <company>-<month>/<tool>-<16 hex>.json with
	// {tool, args, result}.
	err = filepath.WalkDir(filepath.Join(env.root, env.suite.Name), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(env.root, env.suite.Name), p)
		rel = filepath.ToSlash(rel)
		if rel != FixtureIndexFile && !fixtureRelRe.MatchString(rel) {
			t.Errorf("fixture path %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(env.root, env.suite.Name, "sharma-2026-09", ToolBankLines+"-"+
		argsHash(mustCanon(t, periodArgs("sharma", day("2026-09-01"), day("2026-09-30"))))+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"tool": "list_bank_lines"`, `"args": {`, `"result": [`, `"balance_paise": 9223372036854775807`, `"amount_paise": -59000`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("fixture lacks %s:\n%s", s, b)
		}
	}
}

func mustCanon(t *testing.T, args map[string]any) []byte {
	t.Helper()
	c, err := canonicalArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReplayErrors(t *testing.T) {
	setup := func(t *testing.T) (fxEnv, string) {
		env := newFxEnv(t)
		record(t, env, &fakeLive{})
		return env, filepath.Join(env.root, env.suite.Name)
	}
	tbRel := func(month string) string {
		from, _ := time.Parse("2006-01", month)
		return month + "/" + ToolTrialBalance + "-" + argsHash(mustCanon(t, periodArgs("sharma", from, from.AddDate(0, 1, -1)))) + ".json"
	}
	rewriteIndex := func(t *testing.T, dir string, edit func(*FixtureIndex)) {
		t.Helper()
		var idx FixtureIndex
		readJSON(t, filepath.Join(dir, FixtureIndexFile), &idx)
		edit(&idx)
		if err := writeJSON(filepath.Join(dir, FixtureIndexFile), idx); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("missing fixture names the tool and args", func(t *testing.T) {
		env, _ := setup(t)
		rp, _, err := openReplay(env)
		if err != nil {
			t.Fatal(err)
		}
		rp.UseMonth("sharma", "2026-09")
		_, err = rp.Books().TrialBalance(t.Context(), "sharma", day("2026-01-01"), day("2026-01-31"))
		if !errors.Is(err, ErrFixtureMissing) || !strings.Contains(err.Error(), ToolTrialBalance) || !strings.Contains(err.Error(), `"from":"2026-01-01"`) {
			t.Errorf("missing fixture = %v", err)
		}
		_, err = rp.Evidence().GSTR2BEntries(t.Context(), "kaveri", "2026-09")
		if !errors.Is(err, ErrFixtureMissing) || !strings.Contains(err.Error(), "kaveri") {
			t.Errorf("other company = %v", err)
		}
		rp.UseMonth("sharma", "2026-07") // not recorded
		if _, err := rp.Evidence().GSTR2BEntries(t.Context(), "sharma", "2026-07"); !errors.Is(err, ErrFixtureMissing) {
			t.Errorf("unrecorded month = %v", err)
		}
	})

	t.Run("stored args mismatch", func(t *testing.T) {
		env, dir := setup(t)
		rel := "sharma-" + tbRel("2026-09")
		p := filepath.Join(dir, filepath.FromSlash(rel))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.Replace(string(b), `"to": "2026-09-30"`, `"to": "2026-09-29"`, 1))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteIndex(t, dir, func(idx *FixtureIndex) { idx.Files[rel] = sha256Hex(b) })
		rp, _, err := openReplay(env)
		if err != nil {
			t.Fatal(err)
		}
		rp.UseMonth("sharma", "2026-09")
		_, err = rp.Books().TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30"))
		if !errors.Is(err, ErrFixturesInvalid) || !strings.Contains(err.Error(), "holds args") {
			t.Errorf("args mismatch = %v", err)
		}
	})

	t.Run("unknown field in a result", func(t *testing.T) {
		env, dir := setup(t)
		rel := "sharma-" + tbRel("2026-09")
		p := filepath.Join(dir, filepath.FromSlash(rel))
		b, _ := os.ReadFile(p)
		b = []byte(strings.Replace(string(b), `"company": "sharma",`, `"company": "sharma", "bogus": 1,`, 1))
		_ = os.WriteFile(p, b, 0o600)
		rewriteIndex(t, dir, func(idx *FixtureIndex) { idx.Files[rel] = sha256Hex(b) })
		rp, _, err := openReplay(env)
		if err != nil {
			t.Fatal(err)
		}
		rp.UseMonth("sharma", "2026-09")
		if _, err := rp.Books().TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); !errors.Is(err, ErrFixturesInvalid) {
			t.Errorf("unknown field = %v", err)
		}
	})

	t.Run("extra unlisted file", func(t *testing.T) {
		env, dir := setup(t)
		if err := os.WriteFile(filepath.Join(dir, "sharma-2026-09", "list_gl_entries-0000000000000000.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesInvalid) || !strings.Contains(err.Error(), "not listed") {
			t.Errorf("extra file = %v", err)
		}
	})

	t.Run("hash mismatch", func(t *testing.T) {
		env, dir := setup(t)
		p := filepath.Join(dir, filepath.FromSlash("sharma-"+tbRel("2026-08")))
		b, _ := os.ReadFile(p)
		if err := os.WriteFile(p, append(b, ' '), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesInvalid) || !strings.Contains(err.Error(), "the index says") {
			t.Errorf("hash mismatch = %v", err)
		}
	})

	t.Run("listed file missing", func(t *testing.T) {
		env, dir := setup(t)
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash("sharma-"+tbRel("2026-08")))); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesInvalid) || !strings.Contains(err.Error(), "missing") {
			t.Errorf("missing listed file = %v", err)
		}
	})

	t.Run("bad index entry", func(t *testing.T) {
		env, dir := setup(t)
		rewriteIndex(t, dir, func(idx *FixtureIndex) { idx.Files["../escape.json"] = "00" })
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesInvalid) {
			t.Errorf("bad index entry = %v", err)
		}
	})

	t.Run("no index", func(t *testing.T) {
		env := newFxEnv(t)
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixtureMissing) || !strings.Contains(err.Error(), "make record-fixtures") {
			t.Errorf("no index = %v", err)
		}
	})

	t.Run("stale suite", func(t *testing.T) {
		env, _ := setup(t)
		env.suite.SHA256 = strings.Repeat("0", 64)
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesStale) ||
			!strings.Contains(err.Error(), "fixtures are stale: re-record after seeding (make record-fixtures)") {
			t.Errorf("stale suite = %v", err)
		}
	})

	t.Run("stale truth", func(t *testing.T) {
		env, _ := setup(t)
		p := filepath.Join(env.truthDir, "sharma-2026-09.json")
		b, _ := os.ReadFile(p)
		if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesStale) || !strings.Contains(err.Error(), "sharma-2026-09.json changed") {
			t.Errorf("stale truth = %v", err)
		}
	})

	t.Run("month not recorded", func(t *testing.T) {
		env, _ := setup(t)
		env.months = append(env.months, SuiteMonth{Company: "mehta", Month: "2026-09"})
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesStale) || !strings.Contains(err.Error(), "mehta-2026-09 was not recorded") {
			t.Errorf("unrecorded month = %v", err)
		}
	})

	t.Run("other suite's index", func(t *testing.T) {
		env, dir := setup(t)
		rewriteIndex(t, dir, func(idx *FixtureIndex) { idx.Suite = "suite-other" })
		if _, _, err := openReplay(env); !errors.Is(err, ErrFixturesInvalid) {
			t.Errorf("other suite = %v", err)
		}
	})
}

// TestReplayNoLiveFallback checks that the replay types hold no live
// reader, nor anything that could reach one.
func TestReplayNoLiveFallback(t *testing.T) {
	books := reflect.TypeFor[checks.BooksReader]()
	evidence := reflect.TypeFor[checks.EvidenceReader]()
	var walk func(t reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(typ reflect.Type, path string, seen map[reflect.Type]bool) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			if typ.Kind() == reflect.Map {
				walk(typ.Key(), path+"[key]", seen)
			}
			walk(typ.Elem(), path+"[]", seen)
			return
		case reflect.Interface, reflect.Func, reflect.Chan:
			t.Errorf("%s is %s: a replay type must hold only data", path, typ)
			return
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, path+"."+f.Name, seen)
			}
		}
		if typ.Kind() != reflect.Pointer && (reflect.PointerTo(typ).Implements(books) || reflect.PointerTo(typ).Implements(evidence)) &&
			typ != reflect.TypeFor[ReplayBooks]() && typ != reflect.TypeFor[ReplayEvidence]() {
			t.Errorf("%s (%s) is a reader", path, typ)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[ReplayBooks](), reflect.TypeFor[ReplayEvidence](), reflect.TypeFor[Replayer]()} {
		walk(typ, typ.Name(), map[reflect.Type]bool{})
	}
}

func TestRecordAbortsOnLiveError(t *testing.T) {
	env := newFxEnv(t)
	live := &fakeLive{failOn: ToolBankLines}
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	b, e := rec.Books(live), rec.Evidence(live)
	rec.UseMonth("sharma", "2026-09")
	if _, err := b.TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BankLines(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); !errors.Is(err, errLive) {
		t.Errorf("live error = %v, want it unchanged", err)
	}
	calls := live.calls
	if _, err := b.GLEntries(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); !errors.Is(err, ErrRecordingAborted) {
		t.Errorf("call after the abort = %v", err)
	}
	if live.calls != calls {
		t.Error("a call after the abort reached the live reader")
	}
	if rec.Err() == nil {
		t.Error("Err() is nil after a live error")
	}
	_, _, err = rec.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months, ExtraAllowed: fxExtra})
	if !errors.Is(err, ErrRecordingAborted) {
		t.Errorf("Finish = %v", err)
	}
	// Nothing was written: no suite folder, no staging folder left.
	entries, _ := os.ReadDir(env.root)
	if len(entries) != 0 {
		t.Errorf("fixtures root holds %v", entries)
	}
}

// TestRecordKeepsOldFixturesOnFailure checks that a failed re-recording
// leaves the previous fixtures in place.
func TestRecordKeepsOldFixturesOnFailure(t *testing.T) {
	env := newFxEnv(t)
	record(t, env, &fakeLive{})
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	rec.UseMonth("sharma", "2026-09")
	_, _ = rec.Evidence(&fakeLive{failOn: ToolBankLines}).BankLines(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30"))
	if _, _, err := rec.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months, ExtraAllowed: fxExtra}); err == nil {
		t.Fatal("Finish after a live error succeeded")
	}
	if _, _, err := openReplay(env); err != nil {
		t.Errorf("old fixtures no longer replay: %v", err)
	}
}

func TestRecordNonDeterministicLive(t *testing.T) {
	env := newFxEnv(t)
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Abort)
	b := rec.Books(&fakeLive{vary: true})
	rec.UseMonth("sharma", "2026-09")
	if _, err := b.TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); !errors.Is(err, ErrRecordingAborted) ||
		!strings.Contains(err.Error(), "different results") {
		t.Errorf("second, different result = %v", err)
	}
}

func TestRecordNeedsTruthAndScope(t *testing.T) {
	env := newFxEnv(t)
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Abort)
	if _, err := rec.Books(&fakeLive{}).TrialBalance(t.Context(), "sharma", day("2026-09-01"), day("2026-09-30")); !errors.Is(err, ErrRecordingAborted) {
		t.Errorf("call with no month in scope = %v", err)
	}

	rec2, _ := NewRecorder(env.root, env.suite.Name)
	t.Cleanup(rec2.Abort)
	exercise(t, rec2, rec2.Books(&fakeLive{}), rec2.Evidence(&fakeLive{}))
	_ = os.Remove(filepath.Join(env.truthDir, "sharma-2026-08.json"))
	if _, _, err := rec2.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months, ExtraAllowed: fxExtra}); err == nil ||
		!strings.Contains(err.Error(), "ground truth for sharma:2026-08") {
		t.Errorf("Finish without truth = %v", err)
	}

	if _, err := NewRecorder(filepath.Join(t.TempDir(), "evals", "scenarios"), "suite-fx"); err == nil {
		t.Error("recorded under evals/scenarios")
	}
}

// TestRecordScanFails checks that the recorder scans before it writes the
// index: a stray GSTIN aborts the recording.
func TestRecordScanFails(t *testing.T) {
	env := newFxEnv(t)
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, rec, rec.Books(&fakeLive{}), rec.Evidence(&fakeLive{}))
	_, _, err = rec.Finish(t.Context(), RecordMeta{Suite: env.suite, TruthDir: env.truthDir, Months: env.months}) // no allowlist
	if !errors.Is(err, ErrFixtureLeak) {
		t.Errorf("Finish with a GSTIN outside the allowlist = %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, env.suite.Name)); !os.IsNotExist(err) {
		t.Errorf("fixtures were written: %v", err)
	}
}

func TestScopedCloser(t *testing.T) {
	env := newFxEnv(t)
	rec, err := NewRecorder(env.root, env.suite.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Abort)
	live := &fakeLive{}
	rb := rec.Books(live)
	c := &ScopedCloser{Closer: closerFunc(func(ctx context.Context, company, month string) (agent.Result, error) {
		_, err := rec.Evidence(live).GSTR2BEntries(ctx, company, month)
		return agent.Result{}, err
	}), Scope: rec, After: rb.PrimeMonth}
	if _, err := c.RunClose(t.Context(), "sharma", "2026-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rec.stage, "sharma-2026-09")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(rec.stage, "sharma-2026-09"))
	var names []string
	for _, e := range entries {
		names = append(names, strings.SplitN(e.Name(), "-", 2)[0])
	}
	if strings.Join(names, ",") != "get_trial_balance,list_gstr2b_entries" {
		t.Errorf("recorded %v, want the month's trial balance primed", names)
	}
}

type closerFunc func(ctx context.Context, company, month string) (agent.Result, error)

func (f closerFunc) RunClose(ctx context.Context, company, month string) (agent.Result, error) {
	return f(ctx, company, month)
}

// TestFixtureSize keeps the committed fixtures small.
func TestFixtureSize(t *testing.T) {
	const limit = 5 << 20
	root := filepath.Join("..", "..", "evals", "fixtures")
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no evals/fixtures yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	if total > limit {
		t.Errorf("evals/fixtures holds %d bytes, over the %d-byte limit", total, limit)
	}
}
