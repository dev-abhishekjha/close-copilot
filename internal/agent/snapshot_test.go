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
