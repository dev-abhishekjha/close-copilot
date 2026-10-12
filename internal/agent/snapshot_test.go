package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ---- synthetic books and evidence (shared with resume_integration_test.go) ----

const (
	synthCompany = "testco"
	synthMonth   = "2026-08"
	synthBank    = "HDFC Current 0001 - TC"
)

func synthDay(d int) time.Time { return time.Date(2026, 8, d, 0, 0, 0, 0, time.UTC) }

func synthRef(s string) *string { return &s }

// synthBooks is a synthetic BooksReader: one matched rent payment and one
// unmatched journal on the bank account.
type synthBooks struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *synthBooks) count() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *synthBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	f.count()
	return ledger.TB{Company: synthCompany, Rows: []ledger.TBRow{{Account: synthBank, Closing: 100}}}, f.err
}

func (f *synthBooks) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	f.count()
	if f.err != nil {
		return nil, f.err
	}
	return []ledger.GLEntry{
		{Name: "GLE-0001", Account: synthBank, Credit: 5000000, PostingDate: synthDay(5), VoucherType: "Payment Entry", VoucherNo: "ACC-PAY-0001", Party: "Synthetic Estates"},
		{Name: "GLE-0002", Account: "Rent - TC", Debit: 5000000, PostingDate: synthDay(5), VoucherType: "Payment Entry", VoucherNo: "ACC-PAY-0001"},
		{Name: "GLE-0003", Account: synthBank, Credit: 77700, PostingDate: synthDay(20), VoucherType: "Journal Entry", VoucherNo: "ACC-JV-0009", Remarks: "Synthetic transfer"},
	}, nil
}

func (f *synthBooks) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	f.count()
	return []ledger.PurchaseInvoice{{Name: "ACC-PINV-0001", Supplier: "Synthetic Telecom", GrandTotal: 118000}}, f.err
}

func (f *synthBooks) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	f.count()
	return []ledger.SalesInvoice{{Name: "ACC-SINV-0001", Customer: "Synthetic Customer", GrandTotal: 2500000}}, f.err
}

func (f *synthBooks) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	f.count()
	if f.err != nil {
		return nil, f.err
	}
	return []ledger.PaymentEntry{{Name: "ACC-PAY-0001", Party: "Synthetic Estates", ReferenceNo: "UTR-RENT-0805", PaidAmount: 5000000, PostingDate: synthDay(5)}}, nil
}

func (f *synthBooks) AccountHistory(context.Context, string, string, string, int) ([]ledger.MonthTotal, error) {
	f.count()
	return []ledger.MonthTotal{{Month: "2026-07", Net: 1}}, f.err
}

func (f *synthBooks) RecurringSuppliers(context.Context, string, string, int, int, int) ([]checks.RecurringSupplier, error) {
	f.count()
	return []checks.RecurringSupplier{{Supplier: "Synthetic Telecom"}}, f.err
}

// synthEvidence is a synthetic EvidenceReader: the rent line, two bank
// charges and an unexplained credit.
type synthEvidence struct {
	err error
}

func (f *synthEvidence) BankLines(context.Context, string, time.Time, time.Time) ([]store.BankLine, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []store.BankLine{
		{CompanyID: synthCompany, TxnID: "TXN-0001", TxnDate: synthDay(5), Narration: "NEFT RENT SYNTHETIC ESTATES", Ref: synthRef("UTR-RENT-0805"), AmountPaise: -5000000},
		{CompanyID: synthCompany, TxnID: "TXN-0002", TxnDate: synthDay(10), Narration: "SMS CHGS QTR", AmountPaise: -29500},
		{CompanyID: synthCompany, TxnID: "TXN-0003", TxnDate: synthDay(15), Narration: "DEBIT CARD ANNUAL FEE", AmountPaise: -118000},
		{CompanyID: synthCompany, TxnID: "TXN-0004", TxnDate: synthDay(25), Narration: "NEFT FROM SYNTHETIC CUSTOMER", AmountPaise: 2500000},
	}, nil
}

func (f *synthEvidence) GSTR2BEntries(context.Context, string, string) ([]store.GSTR2BEntry, error) {
	return []store.GSTR2BEntry{{CompanyID: synthCompany, Period: synthMonth, SupplierGSTIN: "SYNTH-SUPPLIER-01", InvoiceNo: "INV/1", InvoiceNoNorm: "INV1"}}, f.err
}

func synthInputs(books checks.BooksReader, ev checks.EvidenceReader) checks.Inputs {
	return checks.Inputs{Company: synthCompany, Month: synthMonth, Books: books, Evidence: ev, BankAccounts: []string{synthBank}}
}

// ---- fake artifact store ----

type fakeArtifactStore struct {
	mu       sync.Mutex
	content  map[string][]byte
	kinds    map[string]string
	puts     int
	putErr   error
	failKind string
	calls    []store.LLMCall
	callErr  error
	block    bool // InsertLLMCall waits for its context to end
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{content: map[string][]byte{}, kinds: map[string]string{}}
}

func (f *fakeArtifactStore) PutArtifact(_ context.Context, kind string, _, _ uuid.UUID, v any) (string, error) {
	if err := store.ValidateArtifactKind(kind); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil && (f.failKind == "" || f.failKind == kind) {
		return "", f.putErr
	}
	b, sha, err := store.CanonicalHash(v)
	if err != nil {
		return "", err
	}
	f.puts++
	if _, ok := f.content[sha]; !ok {
		f.content[sha] = b
		f.kinds[sha] = kind
	}
	return sha, nil
}

func (f *fakeArtifactStore) InsertLLMCall(ctx context.Context, c store.LLMCall) (uuid.UUID, error) {
	if f.block {
		<-ctx.Done()
		return uuid.Nil, ctx.Err()
	}
	if c.RunID == uuid.Nil || c.StepID == uuid.Nil {
		return uuid.Nil, errors.New("fake: llm call without run or step")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callErr != nil {
		return uuid.Nil, f.callErr
	}
	if _, ok := f.content[c.PromptSHA256]; !ok {
		return uuid.Nil, errors.New("fake: prompt artifact missing")
	}
	if _, ok := f.content[c.ResponseSHA256]; !ok {
		return uuid.Nil, errors.New("fake: response artifact missing")
	}
	c.ID = uuid.New()
	f.calls = append(f.calls, c)
	return c.ID, nil
}

func (f *fakeArtifactStore) get(sha string) (toolSnapshot, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.content[sha]
	if !ok {
		return toolSnapshot{}, "", false
	}
	var s toolSnapshot
	_ = json.Unmarshal(b, &s)
	return s, f.kinds[sha], true
}

// ---- tests ----

func TestSnapshotAnnotateBankRec(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	in := synthInputs(SnapshotBooks(&synthBooks{}, snap), SnapshotEvidence(&synthEvidence{}, snap))

	findings, err := (&checks.BankRecCheck{}).Run(ctx, in)
	if err != nil {
		t.Fatalf("bankrec: %v", err)
	}
	wantTypes := map[string]int{checks.TypeUnrecordedBankCharge: 2, checks.TypeUnmatchedBankLine: 1, checks.TypeUnmatchedLedgerEntry: 1}
	gotTypes := map[string]int{}
	for _, f := range findings {
		gotTypes[f.Type]++
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("finding types %v, want %v", gotTypes, wantTypes)
	}
	for k, v := range wantTypes {
		if gotTypes[k] != v {
			t.Fatalf("finding types %v, want %v", gotTypes, wantTypes)
		}
	}

	if err := snap.Annotate(findings); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	refs := snap.Refs()
	if len(refs) != 3 {
		t.Errorf("Refs: %d distinct snapshots, want 3 (bank lines, gl entries, payments)", len(refs))
	}
	for _, f := range findings {
		if len(f.Evidence) == 0 {
			t.Fatalf("finding %s has no evidence", f.Type)
		}
		for _, ref := range f.Evidence {
			if ref.Artifact == "" {
				t.Fatalf("finding %s: empty artifact", f.Type)
			}
			s, kind, ok := fs.get(ref.Artifact)
			if !ok {
				t.Fatalf("finding %s: artifact %s not stored", f.Type, ref.Artifact)
			}
			if kind != store.ArtifactToolResult || s.Server != ref.Server || s.Tool != ref.Tool {
				t.Errorf("finding %s: artifact is %s %s/%s, ref is %s/%s", f.Type, kind, s.Server, s.Tool, ref.Server, ref.Tool)
			}
			if s.Args["company"] != synthCompany || s.Args["from_date"] != "2026-08-01" || s.Args["to_date"] != "2026-08-31" {
				t.Errorf("snapshot args %v", s.Args)
			}
			if !slices.Contains(refs, ref.Artifact) {
				t.Errorf("artifact %s missing from Refs", ref.Artifact)
			}
			raw, _ := json.Marshal(s.Result)
			for _, id := range ref.IDs {
				if !strings.Contains(string(raw), `"`+id+`"`) {
					t.Errorf("snapshot %s does not hold id %s", ref.Artifact, id)
				}
			}
		}
	}
}

func TestSnapshotAnnotateFailsClosed(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	ev := SnapshotEvidence(&synthEvidence{}, snap)
	if _, err := ev.BankLines(ctx, synthCompany, synthDay(1), synthDay(31)); err != nil {
		t.Fatal(err)
	}

	good := checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "txn_id": "TXN-0002"}, "TXN-0002")
	tests := []struct {
		name string
		ref  checks.EvidenceRef
		ok   bool
	}{
		{"matching ref", good, true},
		{"ref without args", checks.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"TXN-0004"}}, false},
		{"ref with neither args nor ids", checks.EvidenceRef{Server: "evidence", Tool: "list_bank_lines"}, false},
		{"ref without company", checks.EvidenceEvidence("list_bank_lines", map[string]string{"txn_id": "TXN-0002"}, "TXN-0002"), false},
		{"ref with the call's own args", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "from_date": "2026-08-01", "to_date": "2026-08-31"}), true},
		{"tool never called", checks.EvidenceEvidence("list_gstr2b_entries", map[string]string{"company": synthCompany}), false},
		{"wrong server", checks.BooksEvidence("list_bank_lines", map[string]string{"company": synthCompany}, "TXN-0002"), false},
		{"other company", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": "othercorp", "txn_id": "TXN-0002"}, "TXN-0002"), false},
		{"txn id not in result", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "txn_id": "TXN-9999"}, "TXN-0002"), false},
		{"id not in result", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany}, "TXN-9999"), false},
		{"different date range", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "from_date": "2026-07-01"}), false},
		{"unknown arg", checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "voucher_no": "TXN-0002"}), false},
		{"args not an object", checks.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", Args: json.RawMessage(`["TXN-0002"]`)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := []checks.Finding{
				{ID: uuid.New(), Type: checks.TypeUnrecordedBankCharge, Evidence: []checks.EvidenceRef{good}},
				{ID: uuid.New(), Type: checks.TypeUnmatchedBankLine, Evidence: []checks.EvidenceRef{tt.ref}},
			}
			err := snap.Annotate(findings)
			if tt.ok {
				if err != nil {
					t.Fatalf("Annotate: %v", err)
				}
				for _, f := range findings {
					if f.Evidence[0].Artifact == "" {
						t.Errorf("finding %s left without artifact", f.Type)
					}
				}
				return
			}
			if err == nil {
				t.Fatal("Annotate: expected an error")
			}
			for _, f := range findings {
				if f.Evidence[0].Artifact != "" {
					t.Errorf("a failed Annotate changed finding %s", f.Type)
				}
			}
		})
	}
}

func TestSnapshotReaders(t *testing.T) {
	ctx := context.Background()
	readerErr := errors.New("tool unavailable")
	storeErr := errors.New("db down")
	tests := []struct {
		name      string
		read      func(b checks.BooksReader, e checks.EvidenceReader) error
		tool      string
		readerErr error
		storeErr  error
	}{
		{"trial balance", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.TrialBalance(ctx, synthCompany, synthDay(1), synthDay(31))
			return err
		}, toolGetTrialBalance, nil, nil},
		{"purchase invoices", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.PurchaseInvoices(ctx, synthCompany, synthDay(1), synthDay(31))
			return err
		}, toolListPurchaseInvoices, nil, nil},
		{"sales invoices", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.SalesInvoices(ctx, synthCompany, synthDay(1), synthDay(31))
			return err
		}, toolListSalesInvoices, nil, nil},
		{"account history", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.AccountHistory(ctx, synthCompany, "Rent - TC", synthMonth, 6)
			return err
		}, toolGetAccountHistory, nil, nil},
		{"recurring suppliers", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.RecurringSuppliers(ctx, synthCompany, synthMonth, 6, 3, 10)
			return err
		}, toolListRecurringSuppliers, nil, nil},
		{"gstr2b", func(_ checks.BooksReader, e checks.EvidenceReader) error {
			_, err := e.GSTR2BEntries(ctx, synthCompany, synthMonth)
			return err
		}, toolListGSTR2BEntries, nil, nil},
		{"reader error is passed on, nothing stored", func(b checks.BooksReader, _ checks.EvidenceReader) error {
			_, err := b.GLEntries(ctx, synthCompany, synthDay(1), synthDay(31))
			return err
		}, toolListGLEntries, readerErr, nil},
		{"store error fails the read", func(_ checks.BooksReader, e checks.EvidenceReader) error {
			_, err := e.BankLines(ctx, synthCompany, synthDay(1), synthDay(31))
			return err
		}, toolListBankLines, nil, storeErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeArtifactStore()
			fs.putErr = tt.storeErr
			snap := NewSnapshots(fs, uuid.New(), uuid.New())
			b := SnapshotBooks(&synthBooks{err: tt.readerErr}, snap)
			e := SnapshotEvidence(&synthEvidence{err: tt.readerErr}, snap)
			err := tt.read(b, e)
			switch {
			case tt.readerErr != nil:
				if !errors.Is(err, tt.readerErr) || len(snap.Refs()) != 0 {
					t.Fatalf("err %v, refs %d", err, len(snap.Refs()))
				}
				return
			case tt.storeErr != nil:
				if !errors.Is(err, tt.storeErr) || len(snap.Refs()) != 0 {
					t.Fatalf("err %v, refs %d", err, len(snap.Refs()))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refs := snap.Refs()
			if len(refs) != 1 {
				t.Fatalf("refs %d, want 1", len(refs))
			}
			s, kind, ok := fs.get(refs[0])
			if !ok || kind != store.ArtifactToolResult || s.Tool != tt.tool || s.Args["company"] != synthCompany || s.Result == nil {
				t.Errorf("snapshot %+v kind %s", s, kind)
			}
		})
	}
}

func TestSnapshotSameResultOneArtifact(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	e := SnapshotEvidence(&synthEvidence{}, snap)
	for range 2 {
		if _, err := e.BankLines(ctx, synthCompany, synthDay(1), synthDay(31)); err != nil {
			t.Fatal(err)
		}
	}
	if fs.puts != 2 || len(fs.content) != 1 || len(snap.Refs()) != 1 {
		t.Errorf("puts %d, artifacts %d, refs %d; want 2, 1, 1", fs.puts, len(fs.content), len(snap.Refs()))
	}
}

// Amounts in snapshots stay exact int64 paise.
func TestSnapshotExactAmounts(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	e := SnapshotEvidence(&synthEvidence{}, snap)
	if _, err := e.BankLines(ctx, synthCompany, synthDay(1), synthDay(31)); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	b := fs.content[snap.Refs()[0]]
	fs.mu.Unlock()
	var s struct {
		Result []struct {
			TxnID  string      `json:"txn_id"`
			Amount money.Paise `json:"amount_paise"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Result) != 4 || s.Result[1].Amount != -29500 {
		t.Errorf("snapshot result %+v", s.Result)
	}
}

// A call for another company recorded first must never satisfy a ref.
func TestSnapshotAnnotateCrossCompany(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	ev := SnapshotEvidence(&synthEvidence{}, snap)
	// Same synthetic lines, other company: recorded first.
	if _, err := ev.BankLines(ctx, "othercorp", synthDay(1), synthDay(31)); err != nil {
		t.Fatal(err)
	}
	ref := checks.EvidenceEvidence("list_bank_lines", map[string]string{"company": synthCompany, "txn_id": "TXN-0002"}, "TXN-0002")
	findings := []checks.Finding{{ID: uuid.New(), Type: checks.TypeUnrecordedBankCharge, Evidence: []checks.EvidenceRef{ref}}}
	if err := snap.Annotate(findings); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("Annotate with only another company's call: %v, want ErrNoSnapshot", err)
	}
	if _, err := ev.BankLines(ctx, synthCompany, synthDay(1), synthDay(31)); err != nil {
		t.Fatal(err)
	}
	if err := snap.Annotate(findings); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	refs := snap.Refs()
	if len(refs) != 2 || findings[0].Evidence[0].Artifact != refs[1] {
		t.Fatalf("ref annotated with %s, want the %s call's %s (refs %v)", findings[0].Evidence[0].Artifact, synthCompany, refs[1], refs)
	}
	s, _, _ := fs.get(refs[1])
	if s.Args["company"] != synthCompany {
		t.Errorf("annotated snapshot is for company %v", s.Args["company"])
	}
}

// ---- the four checks whose refs carry month, window and descriptive args (CC-009) ----

const (
	closeAcct  = "Rent - TC"
	dupParty   = "Synthetic Supplies"
	dupAmount  = money.Paise(1180000)
	ruleInv    = "ACC-PINV-0101"
	gstrInv    = "ACC-PINV-0102"
	dupInv     = "ACC-PINV-0050"
	gstinRule  = "SYNTH-GSTIN-A"
	gstinOnly  = "SYNTH-GSTIN-B"
	gstinEarly = "SYNTH-GSTIN-C"
)

func checkDay(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func inWindow(d, from, to time.Time) bool { return !d.Before(from) && !d.After(to) }

// checkBooks is a synthetic BooksReader with data that makes
// PolicyRulesCheck, DuplicatePaymentCheck, GSTR2BCheck and VarianceCheck
// each raise findings for synthCompany in synthMonth. Range reads return
// only records inside the requested window, as the MCP readers do.
type checkBooks struct {
	synthBooks
	// invoiceVoucher posts the variance account's August GL entry from a
	// purchase invoice, so the variance finding cites list_purchase_invoices.
	invoiceVoucher bool
}

func (f *checkBooks) TrialBalance(_ context.Context, c string, _, _ time.Time) (ledger.TB, error) {
	return ledger.TB{Company: c, Rows: []ledger.TBRow{{Account: closeAcct, RootType: ledger.RootTypeExpense, Debit: 10300000}}}, nil
}

func (f *checkBooks) GLEntries(_ context.Context, _ string, from, to time.Time) ([]ledger.GLEntry, error) {
	vt, vn := "Journal Entry", "ACC-JV-0201"
	if f.invoiceVoucher {
		vt, vn = "Purchase Invoice", ruleInv
	}
	all := []ledger.GLEntry{
		{Name: "GLE-0201", Account: closeAcct, Debit: 10000000, PostingDate: checkDay(8, 3), VoucherType: vt, VoucherNo: vn},
		{Name: "GLE-0202", Account: synthBank, Credit: 10000000, PostingDate: checkDay(8, 3), VoucherType: vt, VoucherNo: vn},
	}
	var out []ledger.GLEntry
	for _, e := range all {
		if inWindow(e.PostingDate, from, to) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *checkBooks) PurchaseInvoices(_ context.Context, _ string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	all := []ledger.PurchaseInvoice{
		{Name: dupInv, Supplier: dupParty, BillNo: "SS/50", PostingDate: checkDay(7, 15), GrandTotal: dupAmount},
		{
			Name: ruleInv, Supplier: "Synthetic Computers", SupplierGSTIN: gstinRule, BillNo: "SC/101", PostingDate: checkDay(8, 10),
			Items: []ledger.PurchaseInvoiceItem{
				{Name: "PII-1", ExpenseAccount: "Repairs and Maintenance - TC", Description: "Laptop for the finance team", Amount: 8000000},
				{Name: "PII-2", ExpenseAccount: "Software - TC", Description: "Annual licence renewal", Amount: 6000000},
				{Name: "PII-3", ExpenseAccount: closeAcct, Description: "Store rent, service period: 01-09-2026 to 30-09-2026", Amount: 100000},
			},
			Taxes: []ledger.PurchaseTaxesAndCharges{{Name: "PTC-1", AccountHead: "Input Tax IGST - TC", TaxAmount: 2538000}},
		},
		{
			Name: gstrInv, Supplier: "Synthetic Courier", SupplierGSTIN: gstinEarly, BillNo: "C/5", PostingDate: checkDay(8, 12),
			Taxes: []ledger.PurchaseTaxesAndCharges{
				{Name: "PTC-2", AccountHead: "Input Tax CGST - TC", TaxAmount: 9000},
				{Name: "PTC-3", AccountHead: "Input Tax SGST - TC", TaxAmount: 9000},
			},
		},
	}
	var out []ledger.PurchaseInvoice
	for _, p := range all {
		if inWindow(p.PostingDate, from, to) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *checkBooks) PaymentEntries(_ context.Context, _ string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	all := []ledger.PaymentEntry{
		{
			Name: "ACC-PAY-0050", PaymentType: "Pay", PartyType: "Supplier", Party: dupParty, PaidAmount: dupAmount, PostingDate: checkDay(7, 20),
			References: []ledger.PaymentEntryReference{{ReferenceDoctype: "Purchase Invoice", ReferenceName: dupInv, AllocatedAmount: dupAmount}},
		},
		{Name: "ACC-PAY-0060", PaymentType: "Pay", PartyType: "Supplier", Party: dupParty, PaidAmount: dupAmount, UnallocatedAmount: dupAmount, PostingDate: checkDay(8, 5)},
	}
	var out []ledger.PaymentEntry
	for _, p := range all {
		if inWindow(p.PostingDate, from, to) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *checkBooks) AccountHistory(_ context.Context, _, _ string, through string, months int) ([]ledger.MonthTotal, error) {
	end, err := time.Parse("2006-01", through)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.MonthTotal, months)
	for i := range out {
		m := end.AddDate(0, i-months+1, 0).Format("2006-01")
		out[i] = ledger.MonthTotal{Month: m, Debit: 100000, Net: 100000}
	}
	out[months-1] = ledger.MonthTotal{Month: through, Debit: 10000000, Net: 10000000}
	return out, nil
}

// checkEvidence is the evidence side: both legs of the duplicate payment
// in the bank, and GSTR-2B entries for July and August.
type checkEvidence struct{}

func (checkEvidence) BankLines(_ context.Context, c string, from, to time.Time) ([]store.BankLine, error) {
	all := []store.BankLine{
		{CompanyID: c, TxnID: "TXN-0101", TxnDate: checkDay(7, 20), Narration: "NEFT SYNTHETIC SUPPLIES", AmountPaise: -dupAmount},
		{CompanyID: c, TxnID: "TXN-0102", TxnDate: checkDay(8, 5), Narration: "NEFT SYNTHETIC SUPPLIES", AmountPaise: -dupAmount},
	}
	var out []store.BankLine
	for _, l := range all {
		if inWindow(l.TxnDate, from, to) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (checkEvidence) GSTR2BEntries(_ context.Context, c string, period string) ([]store.GSTR2BEntry, error) {
	switch period {
	case "2026-07":
		return []store.GSTR2BEntry{{CompanyID: c, Period: period, SupplierGSTIN: gstinEarly, InvoiceNo: "C/5", InvoiceNoNorm: "C5", CGSTPaise: 9000, SGSTPaise: 9000, ITCAvailable: true}}, nil
	case "2026-08":
		return []store.GSTR2BEntry{{CompanyID: c, Period: period, SupplierGSTIN: gstinOnly, InvoiceNo: "B/9", InvoiceNoNorm: "B9", IGSTPaise: 90000, ITCAvailable: true}}, nil
	}
	return nil, nil
}

func checkRules() company.Rules {
	return company.Rules{
		Capitalisation: company.CapitalisationRules{ThresholdINR: 50000, AssetKeywords: []string{"laptop"}, WatchedExpenseAccounts: []string{"Repairs and Maintenance"}},
		Prepaid:        company.PrepaidRules{MinAmountINR: 50000, Keywords: []string{"annual"}},
		Variance:       company.VarianceRules{PctThreshold: 25, AbsThresholdINR: 50000},
		GSTR2B:         company.GSTR2BRules{AmountTolerancePaise: 100},
	}
}

// runCheckStep runs one check over its own snapshot recorder, as the
// workflow's check step does, and returns its findings and recorder.
func runCheckStep(t *testing.T, fs *fakeArtifactStore, books checks.BooksReader, c checks.Check) ([]checks.Finding, *Snapshots) {
	t.Helper()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	in := synthInputs(SnapshotBooks(books, snap), SnapshotEvidence(checkEvidence{}, snap))
	in.Rules = checkRules()
	findings, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("%s: %v", c.Name(), err)
	}
	return findings, snap
}

// Every finding of the four checks annotates against the step's own
// recorded reads, and each artifact is that tool's call for the company
// holding every cited ID.
func TestSnapshotAnnotateChecks(t *testing.T) {
	tests := []struct {
		check checks.Check
		want  []string // finding types, sorted
		tools []string // tools cited, sorted and distinct
	}{
		{&checks.PolicyRulesCheck{},
			[]string{checks.TypeMisclassifiedExpense, checks.TypePrepaidNotSpread, checks.TypeWrongPeriodPosting},
			[]string{toolListPurchaseInvoices}},
		{&checks.DuplicatePaymentCheck{},
			[]string{checks.TypeDuplicateVendorPayment},
			[]string{toolListBankLines, toolListPayments, toolListPurchaseInvoices}},
		{&checks.GSTR2BCheck{},
			[]string{checks.TypeGSTR2BMissingIn2B, checks.TypeGSTR2BMissingInBooks, checks.TypeGSTR2BWrongPeriod},
			[]string{toolListGSTR2BEntries, toolListPurchaseInvoices}},
		{&checks.VarianceCheck{},
			[]string{checks.TypeVariance},
			[]string{toolGetAccountHistory, toolListGLEntries}},
	}
	for _, tt := range tests {
		t.Run(tt.check.Name(), func(t *testing.T) {
			fs := newFakeArtifactStore()
			findings, snap := runCheckStep(t, fs, &checkBooks{}, tt.check)
			var types, tools []string
			for _, f := range findings {
				types = append(types, f.Type)
				for _, ref := range f.Evidence {
					if !slices.Contains(tools, ref.Tool) {
						tools = append(tools, ref.Tool)
					}
				}
			}
			slices.Sort(types)
			slices.Sort(tools)
			if !slices.Equal(types, tt.want) || !slices.Equal(tools, tt.tools) {
				t.Fatalf("findings %v citing %v, want %v citing %v", types, tools, tt.want, tt.tools)
			}
			if err := snap.Annotate(findings); err != nil {
				t.Fatalf("Annotate: %v", err)
			}
			refs := snap.Refs()
			for _, f := range findings {
				for _, ref := range f.Evidence {
					if ref.Artifact == "" || !slices.Contains(refs, ref.Artifact) {
						t.Fatalf("%s %s: artifact %q not among the step's refs", f.Type, ref.Tool, ref.Artifact)
					}
					s, kind, ok := fs.get(ref.Artifact)
					if !ok || kind != store.ArtifactToolResult || s.Server != ref.Server || s.Tool != ref.Tool || s.Args["company"] != synthCompany {
						t.Fatalf("%s %s: artifact is %s %s/%s for %v", f.Type, ref.Tool, kind, s.Server, s.Tool, s.Args["company"])
					}
					raw, _ := json.Marshal(s.Result)
					for _, id := range ref.IDs {
						if !strings.Contains(string(raw), `"`+id+`"`) {
							t.Errorf("%s: snapshot %s does not hold id %s", f.Type, ref.Tool, id)
						}
					}
				}
			}
		})
	}
}

// GSTR2BCheck reads three GSTR-2B periods; each 2B-side ref binds to the
// call for its own period, never a neighbour's.
func TestSnapshotAnnotateGSTR2BPeriod(t *testing.T) {
	fs := newFakeArtifactStore()
	findings, snap := runCheckStep(t, fs, &checkBooks{}, &checks.GSTR2BCheck{})
	if err := snap.Annotate(findings); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	n := 0
	for _, f := range findings {
		for _, ref := range f.Evidence {
			if ref.Tool != toolListGSTR2BEntries {
				continue
			}
			var args map[string]any
			if err := json.Unmarshal(ref.Args, &args); err != nil {
				t.Fatal(err)
			}
			s, _, _ := fs.get(ref.Artifact)
			if s.Args["period"] != args["period"] {
				t.Errorf("%s: ref for period %v bound to the %v call", f.Type, args["period"], s.Args["period"])
			}
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d GSTR-2B refs, want 2", n)
	}
}

// A variance on an account posted from purchase invoices cites
// list_purchase_invoices. VarianceCheck reads the month's invoices itself, so
// the ref has a snapshot inside a variance-only step and Annotate succeeds on
// the first call.
func TestSnapshotAnnotateVarianceInvoiceRef(t *testing.T) {
	fs := newFakeArtifactStore()
	books := &checkBooks{invoiceVoucher: true}
	findings, snap := runCheckStep(t, fs, books, &checks.VarianceCheck{})
	if len(findings) != 1 || len(findings[0].Evidence) != 3 || findings[0].Evidence[2].Tool != toolListPurchaseInvoices {
		t.Fatalf("variance findings %+v, want one citing purchase invoices", findings)
	}
	if err := snap.Annotate(findings); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	for i, ev := range findings[0].Evidence {
		if ev.Artifact == "" {
			t.Errorf("evidence %d (%s) has no artifact", i, ev.Tool)
		}
	}
}

// The new arg rules keep failing closed: another company, an unknown or
// misplaced arg, a month or range no recorded window covers, a period no
// call was made for, an identifier or ID not in the result.
func TestSnapshotAnnotateChecksFailClosed(t *testing.T) {
	ctx := context.Background()
	fs := newFakeArtifactStore()
	snap := NewSnapshots(fs, uuid.New(), uuid.New())
	books := SnapshotBooks(&checkBooks{}, snap)
	ev := SnapshotEvidence(checkEvidence{}, snap)
	// Another company's August invoices, recorded first.
	if _, err := books.PurchaseInvoices(ctx, "othercorp", checkDay(8, 1), checkDay(8, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err := books.PurchaseInvoices(ctx, synthCompany, checkDay(8, 1), checkDay(8, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err := ev.BankLines(ctx, synthCompany, checkDay(7, 2), checkDay(8, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err := ev.GSTR2BEntries(ctx, synthCompany, "2026-08"); err != nil {
		t.Fatal(err)
	}
	if _, err := books.AccountHistory(ctx, synthCompany, closeAcct, synthMonth, 4); err != nil {
		t.Fatal(err)
	}

	pi := func(args map[string]any, ids ...string) checks.EvidenceRef {
		return checks.BooksEvidence(toolListPurchaseInvoices, args, ids...)
	}
	g2b := func(args map[string]any, ids ...string) checks.EvidenceRef {
		return checks.EvidenceEvidence(toolListGSTR2BEntries, args, ids...)
	}
	month := func(extra map[string]any) map[string]any {
		m := map[string]any{"company": synthCompany, "month": synthMonth}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	good := pi(month(map[string]any{"rule": "capitalisation", "account": "Repairs and Maintenance - TC"}), ruleInv)

	tests := []struct {
		name string
		ref  checks.EvidenceRef
		ok   bool
	}{
		{"rules ref", good, true},
		{"rules cut-off ref", pi(month(map[string]any{"rule": "cutoff", "account": closeAcct, "service_period_start": "2026-09-01"}), ruleInv), true},
		{"books side from/to", pi(map[string]any{"company": synthCompany, "from": "2026-08-01", "to": "2026-08-31", "igst_paise": 2538000, "books_tax_paise": 2538000}, ruleInv), true},
		{"bank month inside a wider window", checks.EvidenceEvidence(toolListBankLines, map[string]string{"company": synthCompany, "month": synthMonth}, "TXN-0101", "TXN-0102"), true},
		{"2B month equals period", g2b(map[string]any{"company": synthCompany, "month": synthMonth}, "B9"), true},
		{"2B side", g2b(map[string]any{"company": synthCompany, "period": synthMonth, "supplier_gstin": gstinOnly, "gstr2b_tax_paise": 90000, "itc_available": true}, "B9"), true},
		{"history", checks.BooksEvidence(toolGetAccountHistory, map[string]any{"company": synthCompany, "account": closeAcct, "through_month": synthMonth, "months": 4}), true},

		{"company with no call", pi(map[string]any{"company": "thirdcorp", "month": synthMonth, "rule": "capitalisation"}, ruleInv), false},
		{"no company", pi(map[string]any{"month": synthMonth, "rule": "capitalisation"}, ruleInv), false},
		{"unknown arg", pi(month(map[string]any{"colour": "red"}), ruleInv), false},
		{"2B-only descriptive arg on invoices", pi(month(map[string]any{"itc_available": true}), ruleInv), false},
		{"invoice descriptive arg on payments", checks.EvidenceEvidence(toolListBankLines, map[string]any{"company": synthCompany, "month": synthMonth, "rule": "x"}, "TXN-0102"), false},
		{"month after the window", pi(month(map[string]any{"month": "2026-09"}), ruleInv), false},
		{"month partly outside the window", checks.EvidenceEvidence(toolListBankLines, map[string]string{"company": synthCompany, "month": "2026-07"}, "TXN-0101"), false},
		{"month not YYYY-MM", pi(month(map[string]any{"month": "2026-8"}), ruleInv), false},
		{"month not a string", pi(month(map[string]any{"month": 202608}), ruleInv), false},
		{"2B month without a call", g2b(map[string]any{"company": synthCompany, "month": "2026-07"}, "B9"), false},
		{"2B period without a call", g2b(map[string]any{"company": synthCompany, "period": "2026-07"}, "B9"), false},
		{"from without to", pi(map[string]any{"company": synthCompany, "from": "2026-08-01"}, ruleInv), false},
		{"to without from", pi(map[string]any{"company": synthCompany, "to": "2026-08-31"}, ruleInv), false},
		{"range outside the window", pi(map[string]any{"company": synthCompany, "from": "2026-07-01", "to": "2026-08-31"}, ruleInv), false},
		{"range reversed", pi(map[string]any{"company": synthCompany, "from": "2026-08-31", "to": "2026-08-01"}, ruleInv), false},
		{"range on a period call", g2b(map[string]any{"company": synthCompany, "from": "2026-08-01", "to": "2026-08-31"}, "B9"), false},
		{"account not on any item", pi(month(map[string]any{"account": "Travel - TC"}), ruleInv), false},
		{"id not in result", pi(month(nil), "ACC-PINV-9999"), false},
		{"id from outside the window", pi(month(nil), dupInv), false},
		{"history months differ", checks.BooksEvidence(toolGetAccountHistory, map[string]any{"company": synthCompany, "account": closeAcct, "through_month": synthMonth, "months": 3}), false},
		{"history account differs", checks.BooksEvidence(toolGetAccountHistory, map[string]any{"company": synthCompany, "account": "Travel - TC", "through_month": synthMonth, "months": 4}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := []checks.Finding{
				{ID: uuid.New(), Type: checks.TypeMisclassifiedExpense, Evidence: []checks.EvidenceRef{good}},
				{ID: uuid.New(), Type: checks.TypeWrongPeriodPosting, Evidence: []checks.EvidenceRef{tt.ref}},
			}
			err := snap.Annotate(findings)
			if tt.ok {
				if err != nil {
					t.Fatalf("Annotate: %v", err)
				}
				for _, f := range findings {
					s, _, _ := fs.get(f.Evidence[0].Artifact)
					if s.Args["company"] != synthCompany {
						t.Errorf("%s bound to company %v", f.Type, s.Args["company"])
					}
				}
				return
			}
			if !errors.Is(err, ErrNoSnapshot) {
				t.Fatalf("Annotate: %v, want ErrNoSnapshot", err)
			}
			for _, f := range findings {
				if f.Evidence[0].Artifact != "" {
					t.Errorf("a failed Annotate changed finding %s", f.Type)
				}
			}
		})
	}
}
