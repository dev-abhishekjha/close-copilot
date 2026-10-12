package checks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const vrMonth = "2026-08"

type varianceBooks struct {
	tb      ledger.TB
	gl      []ledger.GLEntry
	hist    map[string][]ledger.MonthTotal
	tbErr   error
	glErr   error
	histErr error
	pi      []ledger.PurchaseInvoice
	piErr   error
	piCalls int
}

func (f *varianceBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	return f.tb, f.tbErr
}
func (f *varianceBooks) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	return f.gl, f.glErr
}
func (f *varianceBooks) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	f.piCalls++
	return f.pi, f.piErr
}
func (f *varianceBooks) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	return nil, nil
}
func (f *varianceBooks) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	return nil, nil
}
func (f *varianceBooks) AccountHistory(_ context.Context, _, account, _ string, _ int) ([]ledger.MonthTotal, error) {
	return f.hist[account], f.histErr
}
func (f *varianceBooks) RecurringSuppliers(context.Context, string, string, int, int, int) ([]RecurringSupplier, error) {
	return nil, nil
}

// vrHist builds four months of history from nets in rupees (debit positive).
func vrHist(rupees ...int64) []ledger.MonthTotal {
	months := []string{"2026-05", "2026-06", "2026-07", "2026-08"}
	out := make([]ledger.MonthTotal, len(rupees))
	for i, r := range rupees {
		p := money.Paise(r * 100)
		mt := ledger.MonthTotal{Month: months[i], Net: p}
		if p >= 0 {
			mt.Debit = p
		} else {
			mt.Credit = -p
		}
		out[i] = mt
	}
	return out
}

func vrRow(account, root string) ledger.TBRow {
	return ledger.TBRow{Account: account, RootType: root, Debit: 1}
}

func vrInputs(b *varianceBooks) Inputs {
	return Inputs{
		Company: "Test Co", Month: vrMonth, Books: b,
		Rules: company.Rules{Variance: company.VarianceRules{PctThreshold: 25, AbsThresholdINR: 50000}},
	}
}

func vrOne(account, root string, rupees ...int64) *varianceBooks {
	return &varianceBooks{
		tb:   ledger.TB{Rows: []ledger.TBRow{vrRow(account, root)}},
		hist: map[string][]ledger.MonthTotal{account: vrHist(rupees...)},
	}
}

func TestVarianceThresholds(t *testing.T) {
	// prior months total 3*100000 = 300000 rupees (average 100000).
	tests := []struct {
		name  string
		root  string
		nets  []int64
		want  bool
		title string
	}{
		{"expense up", "Expense", []int64{100000, 100000, 100000, 220000}, true, "up by ₹1,20,000.00"},
		{"expense down", "Expense", []int64{100000, 100000, 100000, 0}, true, "down by ₹1,00,000.00"},
		{"income up (credit)", "Income", []int64{-100000, -100000, -100000, -220000}, true, "up by ₹1,20,000.00"},
		{"income down", "Income", []int64{-100000, -100000, -100000, -10000}, true, "down by ₹90,000.00"},
		{"expense negative net is down", "Expense", []int64{100000, 100000, 100000, -20000}, true, "down by ₹1,20,000.00"},
		{"pct passes abs fails", "Expense", []int64{1000, 1000, 1000, 2000}, false, ""},
		{"abs passes pct fails", "Expense", []int64{1000000, 1000000, 1000000, 1100000}, false, ""},
		{"exactly at abs threshold", "Expense", []int64{0, 0, 0, 50000}, false, ""},
		{"just over abs threshold from zero", "Expense", []int64{0, 0, 0, 50001}, true, "up by ₹50,001.00"},
		// avg 200000; need |change| > 25% = 50000 and > abs 50000: exactly 50000 fails both.
		{"exactly at both", "Expense", []int64{200000, 200000, 200000, 250000}, false, ""},
		{"exactly at pct, abs far over", "Expense", []int64{8000000, 8000000, 8000000, 10000000}, false, ""},
		{"over pct and abs", "Expense", []int64{200000, 200000, 200000, 250001}, true, "up by ₹50,001.00"},
		{"only prior months active, now zero", "Expense", []int64{100000, 0, 0, 0}, false, ""},
		{"prior active, now zero, flagged down", "Expense", []int64{200000, 100000, 0, 0}, true, "down by ₹1,00,000.00"},
		{"all four zero skipped", "Expense", []int64{0, 0, 0, 0}, false, ""},
		{"steady", "Expense", []int64{100000, 100000, 100000, 100000}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acct := "Software Subscriptions - STPL"
			got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(vrOne(acct, tc.root, tc.nets...)))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("want no findings, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d", len(got))
			}
			f := got[0]
			wantTitle := fmt.Sprintf("%s %s against the 3-month average", acct, tc.title)
			if f.Title != wantTitle {
				t.Errorf("title = %q, want %q", f.Title, wantTitle)
			}
			if f.Type != TypeVariance || f.Severity != SeverityLow || f.Status != StatusOpen {
				t.Errorf("bad type/severity/status: %+v", f)
			}
			if f.Action == nil || *f.Action != ActionInvestigate {
				t.Errorf("action = %v", f.Action)
			}
			if f.Keys["account"] != acct || f.Keys["month"] != vrMonth || f.Keys[varianceKeyRelated] != "" {
				t.Errorf("keys = %v", f.Keys)
			}
			if f.AmountPaise == nil || *f.AmountPaise <= 0 {
				t.Errorf("amount = %v", f.AmountPaise)
			}
		})
	}
}

func TestVarianceAmount(t *testing.T) {
	got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(vrOne("A", "Expense", 100000, 100000, 100000, 220000)))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if *got[0].AmountPaise != 12000000 {
		t.Errorf("amount = %d", *got[0].AmountPaise)
	}
}

func TestVarianceIgnoresOtherRootTypes(t *testing.T) {
	b := &varianceBooks{hist: map[string][]ledger.MonthTotal{}}
	for _, root := range []string{"Asset", "Liability", "Equity"} {
		b.tb.Rows = append(b.tb.Rows, vrRow("X-"+root, root))
		b.hist["X-"+root] = vrHist(0, 0, 0, 9000000)
	}
	b.tb.Rows = append(b.tb.Rows, ledger.TBRow{Account: "Quiet", RootType: "Expense"}) // no activity
	b.hist["Quiet"] = vrHist(0, 0, 0, 9000000)
	got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestVarianceSortedByAccount(t *testing.T) {
	b := &varianceBooks{
		tb: ledger.TB{Rows: []ledger.TBRow{vrRow("Zed", "Expense"), vrRow("Alpha", "Expense")}},
		hist: map[string][]ledger.MonthTotal{
			"Zed": vrHist(0, 0, 0, 100000), "Alpha": vrHist(0, 0, 0, 100000),
		},
	}
	got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if err != nil || len(got) != 2 || got[0].Keys["account"] != "Alpha" || got[1].Keys["account"] != "Zed" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func vrGL(name, account string, debit, credit int64, vt, vno string, cancelled bool) ledger.GLEntry {
	return ledger.GLEntry{
		Name: name, Account: account, Debit: money.Paise(debit), Credit: money.Paise(credit),
		VoucherType: vt, VoucherNo: vno, IsCancelled: cancelled,
		PostingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	}
}

func TestVarianceEvidence(t *testing.T) {
	acct := "Software Subscriptions - STPL"
	b := vrOne(acct, "Expense", 0, 0, 0, 100000)
	b.gl = []ledger.GLEntry{
		vrGL("GL-9", acct, 500, 0, "Journal Entry", "JV-1", false),
		vrGL("GL-1", acct, 9000, 0, "Purchase Invoice", "PINV-2", false),
		vrGL("GL-2", acct, 9000, 0, "Purchase Invoice", "PINV-1", false),
		vrGL("GL-3", acct, 0, 7000, "Journal Entry", "JV-2", false),
		vrGL("GL-4", acct, 6000, 0, "Purchase Invoice", "PINV-1", false),
		vrGL("GL-5", acct, 5000, 0, "Journal Entry", "JV-3", false),
		vrGL("GL-C", acct, 99999999, 0, "Purchase Invoice", "PINV-X", true),
		vrGL("GL-O", "Other", 99999999, 0, "Purchase Invoice", "PINV-O", false),
	}
	b.pi = []ledger.PurchaseInvoice{{Name: "PINV-1"}, {Name: "PINV-2"}, {Name: "PINV-X"}}
	got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	ev := got[0].Evidence
	if len(ev) != 3 {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev[0].Tool != "get_account_history" || len(ev[0].IDs) != 0 {
		t.Errorf("ev0 = %+v", ev[0])
	}
	if ev[1].Tool != "list_gl_entries" || !slices.Equal(ev[1].IDs, []string{"GL-1", "GL-2", "GL-3", "GL-4", "GL-5"}) {
		t.Errorf("ev1 ids = %v", ev[1].IDs)
	}
	if ev[2].Tool != "list_purchase_invoices" || !slices.Equal(ev[2].IDs, []string{"PINV-1", "PINV-2"}) {
		t.Errorf("ev2 ids = %v", ev[2].IDs)
	}
	for _, e := range ev {
		if e.Server != "books" {
			t.Errorf("server = %q", e.Server)
		}
	}
}

func TestVarianceDropsInvoicesNotRead(t *testing.T) {
	acct := "A"
	b := vrOne(acct, "Expense", 0, 0, 0, 100000)
	b.gl = []ledger.GLEntry{
		vrGL("GL-1", acct, 9000, 0, "Purchase Invoice", "PINV-1", false),
		vrGL("GL-2", acct, 8000, 0, "Purchase Invoice", "PINV-2", false),
	}
	b.pi = []ledger.PurchaseInvoice{{Name: "PINV-2"}}
	got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if err != nil || len(got) != 1 || len(got[0].Evidence) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if ids := got[0].Evidence[2].IDs; !slices.Equal(ids, []string{"PINV-2"}) {
		t.Errorf("ids = %v", ids)
	}
	b.pi = nil
	got, _ = (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if len(got) != 1 || len(got[0].Evidence) != 2 {
		t.Errorf("no invoices read: evidence = %+v", got)
	}
}

func TestVarianceReadsInvoicesOnlyWhenFlagged(t *testing.T) {
	b := vrOne("A", "Expense", 10000, 10000, 10000, 10000)
	if got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b)); err != nil || len(got) != 0 || b.piCalls != 0 {
		t.Errorf("unflagged: got %v, %v, calls %d", got, err, b.piCalls)
	}
	b = &varianceBooks{hist: map[string][]ledger.MonthTotal{}}
	for _, a := range []string{"A", "B"} {
		b.tb.Rows = append(b.tb.Rows, vrRow(a, ledger.RootTypeExpense))
		b.hist[a] = vrHist(0, 0, 0, 100000)
	}
	if got, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b)); err != nil || len(got) != 2 || b.piCalls != 1 {
		t.Errorf("flagged: got %d, %v, calls %d", len(got), err, b.piCalls)
	}
}

func TestVarianceNoInvoiceEvidenceWithoutPurchaseVouchers(t *testing.T) {
	b := vrOne("A", "Expense", 0, 0, 0, 100000)
	b.gl = []ledger.GLEntry{vrGL("GL-1", "A", 100, 0, "Journal Entry", "JV-1", false)}
	got, _ := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if len(got) != 1 || len(got[0].Evidence) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestVarianceErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name  string
		mod   func(b *varianceBooks, in *Inputs)
		isErr error
	}{
		{"trial balance", func(b *varianceBooks, _ *Inputs) { b.tbErr = boom }, boom},
		{"purchase invoices", func(b *varianceBooks, _ *Inputs) { b.piErr = boom }, boom},
		{"gl entries", func(b *varianceBooks, _ *Inputs) { b.glErr = boom }, boom},
		{"history", func(b *varianceBooks, _ *Inputs) { b.histErr = boom }, boom},
		{"bad month", func(_ *varianceBooks, in *Inputs) { in.Month = "2026-13" }, nil},
		{"short history", func(b *varianceBooks, _ *Inputs) { b.hist["A"] = vrHist(1, 2, 3) }, nil},
		{"wrong last month", func(b *varianceBooks, _ *Inputs) {
			h := vrHist(1, 2, 3, 4)
			h[3].Month = "2026-09"
			b.hist["A"] = h
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := vrOne("A", "Expense", 0, 0, 0, 100000)
			in := vrInputs(b)
			tc.mod(b, &in)
			_, err := (&VarianceCheck{}).Run(context.Background(), in)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.isErr != nil && !errors.Is(err, tc.isErr) {
				t.Errorf("error %v does not wrap %v", err, tc.isErr)
			}
		})
	}
}

func TestVarianceOverflowIsError(t *testing.T) {
	b := vrOne("A", "Expense", 0, 0, 0, 1)
	b.hist["A"][3].Net = money.Paise(9e18 / 3)
	if _, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b)); err == nil {
		t.Fatal("want overflow error")
	}
}

func vrFinding(typ string, keys map[string]string, ev ...EvidenceRef) Finding {
	return Finding{ID: uuid.New(), Type: typ, Keys: keys, Evidence: ev, Status: StatusOpen}
}

func TestVarianceLink(t *testing.T) {
	acct := "Software Subscriptions - STPL"
	vkeys := func() map[string]string { return map[string]string{"account": acct, "month": vrMonth} }
	vEv := BooksEvidence("list_purchase_invoices", map[string]string{"company": "c"}, "PINV-7", "PINV-8")

	prepaid := vrFinding(TypePrepaidNotSpread, map[string]string{"invoice": "PINV-7"})
	sameAcct := vrFinding(TypeMissingAccrual, map[string]string{"account": acct, "month": vrMonth})
	otherMonth := vrFinding(TypeMissingAccrual, map[string]string{"account": acct, "month": "2026-07"})
	otherVar := vrFinding(TypeVariance, map[string]string{"account": acct, "month": vrMonth})
	unrelated := vrFinding(TypePrepaidNotSpread, map[string]string{"invoice": "PINV-99"})

	tests := []struct {
		name  string
		v     Finding
		peers []Finding
		want  *Finding
	}{
		{"by invoice", vrFinding(TypeVariance, vkeys(), vEv), []Finding{unrelated, prepaid}, &prepaid},
		{"by account and month", vrFinding(TypeVariance, vkeys()), []Finding{otherMonth, sameAcct}, &sameAcct},
		{"first in input order wins", vrFinding(TypeVariance, vkeys(), vEv), []Finding{sameAcct, prepaid}, &sameAcct},
		{"no peer", vrFinding(TypeVariance, vkeys(), vEv), []Finding{unrelated, otherMonth}, nil},
		{"never another variance", vrFinding(TypeVariance, vkeys()), []Finding{otherVar}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]Finding{tc.v}, tc.peers...)
			out := LinkVariances(in)
			got, ok := out[0].Keys[varianceKeyRelated]
			if tc.want == nil {
				if ok {
					t.Fatalf("related = %q, want none", got)
				}
				return
			}
			if got != tc.want.ID.String() {
				t.Errorf("related = %q, want %q", got, tc.want.ID)
			}
			if _, ok := in[0].Keys[varianceKeyRelated]; ok {
				t.Error("input map mutated")
			}
			if len(out) != len(in) {
				t.Errorf("len = %d", len(out))
			}
		})
	}

	t.Run("already related is left alone", func(t *testing.T) {
		keys := vkeys()
		keys[varianceKeyRelated] = "keep"
		out := LinkVariances([]Finding{vrFinding(TypeVariance, keys), sameAcct})
		if out[0].Keys[varianceKeyRelated] != "keep" {
			t.Errorf("related = %q", out[0].Keys[varianceKeyRelated])
		}
	})
	t.Run("deterministic", func(t *testing.T) {
		in := []Finding{vrFinding(TypeVariance, vkeys(), vEv), prepaid, sameAcct}
		a, b := LinkVariances(in), LinkVariances(in)
		if a[0].Keys[varianceKeyRelated] != b[0].Keys[varianceKeyRelated] {
			t.Error("not deterministic")
		}
	})
	t.Run("non-variance findings unchanged", func(t *testing.T) {
		out := LinkVariances([]Finding{prepaid})
		if _, ok := out[0].Keys[varianceKeyRelated]; ok {
			t.Error("related set on non-variance")
		}
	})
}

func TestVarianceFourPrepaidCasesLinked(t *testing.T) {
	b := &varianceBooks{hist: map[string][]ledger.MonthTotal{}}
	var prepaids []Finding
	for i := 1; i <= 4; i++ {
		acct := fmt.Sprintf("Subscriptions %d - STPL", i)
		inv := fmt.Sprintf("PINV-%d", i)
		b.tb.Rows = append(b.tb.Rows, vrRow(acct, ledger.RootTypeExpense))
		b.hist[acct] = vrHist(10000, 10000, 10000, 1210000)
		b.gl = append(b.gl, vrGL(fmt.Sprintf("GL-%d", i), acct, 121000000, 0, "Purchase Invoice", inv, false))
		b.pi = append(b.pi, ledger.PurchaseInvoice{Name: inv})
		prepaids = append(prepaids, vrFinding(TypePrepaidNotSpread, map[string]string{"invoice": inv}))
	}
	vs, err := (&VarianceCheck{}).Run(context.Background(), vrInputs(b))
	if err != nil || len(vs) != 4 {
		t.Fatalf("got %d, %v", len(vs), err)
	}
	for i := range vs {
		vs[i].ID = uuid.New()
	}
	out := LinkVariances(append(slices.Clone(prepaids), vs...))
	for i := 0; i < 4; i++ {
		v := out[4+i]
		want := prepaids[i].ID.String()
		if v.Keys[varianceKeyRelated] != want {
			t.Errorf("variance %d related = %q, want %q", i, v.Keys[varianceKeyRelated], want)
		}
	}
}
