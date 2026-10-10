package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

type fakeEvidenceStore struct {
	bankLines []store.BankLine
	gstr2b    []store.GSTR2BEntry
}

func (f *fakeEvidenceStore) ListBankLines(_ context.Context, filter store.BankLineFilter) ([]store.BankLine, error) {
	var out []store.BankLine
	for _, l := range f.bankLines {
		if filter.CompanyID != "" && l.CompanyID != filter.CompanyID {
			continue
		}
		if filter.FromDate != nil && l.TxnDate.Before(*filter.FromDate) {
			continue
		}
		if filter.ToDate != nil && l.TxnDate.After(*filter.ToDate) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeEvidenceStore) ListGSTR2BEntries(_ context.Context, companyID, period string) ([]store.GSTR2BEntry, error) {
	var out []store.GSTR2BEntry
	for _, e := range f.gstr2b {
		if e.CompanyID == companyID && e.Period == period {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestDirectEvidence(t *testing.T) {
	ctx := context.Background()
	date1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	fakeStore := &fakeEvidenceStore{
		bankLines: []store.BankLine{
			{CompanyID: "sharma", TxnID: "TXN-1", TxnDate: date1, AmountPaise: 1000},
			{CompanyID: "sharma", TxnID: "TXN-2", TxnDate: date2, AmountPaise: 2000},
			{CompanyID: "other", TxnID: "TXN-3", TxnDate: date1, AmountPaise: 3000},
		},
		gstr2b: []store.GSTR2BEntry{
			{CompanyID: "sharma", Period: "2026-09", InvoiceNo: "INV-1"},
			{CompanyID: "sharma", Period: "2026-08", InvoiceNo: "INV-OLD"},
		},
	}

	reader := &DirectEvidence{Store: fakeStore}

	lines, err := reader.BankLines(ctx, "sharma", date1, date2)
	if err != nil {
		t.Fatalf("BankLines: %v", err)
	}
	if len(lines) != 2 {
		t.Errorf("BankLines len = %d, want 2", len(lines))
	}

	entries, err := reader.GSTR2BEntries(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatalf("GSTR2BEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].InvoiceNo != "INV-1" {
		t.Errorf("GSTR2BEntries got %+v, want INV-1", entries)
	}
}

func TestDirectEvidenceNilStore(t *testing.T) {
	reader := &DirectEvidence{Store: nil}
	ctx := context.Background()

	if _, err := reader.BankLines(ctx, "sharma", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil store")
	}
	if _, err := reader.GSTR2BEntries(ctx, "sharma", "2026-09"); err == nil {
		t.Error("expected error with nil store")
	}
}

func TestComputeRecurringSuppliers(t *testing.T) {
	d1 := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)

	invoices := []frappe.PurchaseInvoice{
		// Landlord: 3 months, ₹50,000 each month (5000000 paise) -> recurring
		{Supplier: "Landlord", PostingDate: d1, GrandTotal: money.Paise(5000000)},
		{Supplier: "Landlord", PostingDate: d2, GrandTotal: money.Paise(5000000)},
		{Supplier: "Landlord", PostingDate: d3, GrandTotal: money.Paise(5000000)},

		// Fluctuating supplier: 3 months, but swing > 20% from median (1000000 vs 5000000)
		{Supplier: "Fluctuating", PostingDate: d1, GrandTotal: money.Paise(1000000)},
		{Supplier: "Fluctuating", PostingDate: d2, GrandTotal: money.Paise(5000000)},
		{Supplier: "Fluctuating", PostingDate: d3, GrandTotal: money.Paise(9000000)},

		// One-off supplier: 1 month -> below minOccurrences 3
		{Supplier: "OneOff", PostingDate: d1, GrandTotal: money.Paise(5000000)},
	}

	got := computeRecurringSuppliers(invoices, 3, 20)
	if len(got) != 1 {
		t.Fatalf("computeRecurringSuppliers len = %d, want 1", len(got))
	}

	rec := got[0]
	if rec.Supplier != "Landlord" {
		t.Errorf("Supplier = %q, want Landlord", rec.Supplier)
	}
	if rec.MedianAmount != 5000000 {
		t.Errorf("MedianAmount = %d, want 5000000", rec.MedianAmount)
	}
	if rec.TypicalDay != 5 {
		t.Errorf("TypicalDay = %d, want 5", rec.TypicalDay)
	}
	wantMonths := []string{"2026-06", "2026-07", "2026-08"}
	if len(rec.MonthsSeen) != 3 || rec.MonthsSeen[0] != wantMonths[0] || rec.MonthsSeen[1] != wantMonths[1] || rec.MonthsSeen[2] != wantMonths[2] {
		t.Errorf("MonthsSeen = %v, want %v", rec.MonthsSeen, wantMonths)
	}
}

func TestDirectBooksNilClient(t *testing.T) {
	ctx := context.Background()
	reader := &DirectBooks{Client: nil, Companies: fakeDirectory{"c": {ID: "c", ERPCompany: "C Pvt Ltd"}}}

	if _, err := reader.TrialBalance(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.GLEntries(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.PurchaseInvoices(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.SalesInvoices(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.PaymentEntries(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.AccountHistory(ctx, "c", "acc", "2026-09", 3); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.RecurringSuppliers(ctx, "c", "2026-09", 3, 3, 20); err == nil {
		t.Error("expected error with nil client")
	}
}

// ---- DirectBooks against a fake ERPNext ----

// Synthetic company: the ID the checks use and the name ERPNext knows.
const (
	testCompanyID  = "testco"
	testERPCompany = "Testco Traders Pvt Ltd"
)

var errUnknownCompany = errors.New("unknown company")

// fakeDirectory is an in-memory CompanyDirectory.
type fakeDirectory map[string]store.Company

func (f fakeDirectory) GetCompany(_ context.Context, id string) (store.Company, error) {
	c, ok := f[id]
	if !ok {
		return store.Company{}, fmt.Errorf("%w: %s", errUnknownCompany, id)
	}
	return c, nil
}

// fakeERP is a tiny ERPNext: it serves list and get requests for the docs
// it holds, applies "=" and "!=" filters on scalar fields, and records the
// filters of every list request.
type fakeERP struct {
	docs map[string][]map[string]any // doctype -> documents

	mu      sync.Mutex
	filters map[string][][]any // doctype -> filters of the last list request
}

func (f *fakeERP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/resource/")
	if !ok || r.Method != http.MethodGet {
		http.Error(w, "unexpected request", http.StatusNotFound)
		return
	}
	doctype, name, isGet := strings.Cut(rest, "/")
	w.Header().Set("Content-Type", "application/json")

	if isGet {
		for _, d := range f.docs[doctype] {
			if d["name"] == name {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": d})
				return
			}
		}
		http.Error(w, `{"exc_type":"DoesNotExistError"}`, http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	var filters [][]any
	if raw := q.Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	f.mu.Lock()
	f.filters[doctype] = filters
	f.mu.Unlock()

	rows := []map[string]any{}
	if q.Get("limit_start") == "0" {
		for _, d := range f.docs[doctype] {
			if matches(d, filters) {
				rows = append(rows, d)
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
}

func matches(doc map[string]any, filters [][]any) bool {
	for _, flt := range filters {
		if len(flt) != 3 {
			continue
		}
		field, _ := flt[0].(string)
		op, _ := flt[1].(string)
		got, want := fmt.Sprint(doc[field]), fmt.Sprint(flt[2])
		switch op {
		case "=":
			if got != want {
				return false
			}
		case "!=":
			if got == want {
				return false
			}
		}
	}
	return true
}

// filter returns the value of the first filter on field for doctype.
func (f *fakeERP) filter(doctype, field string) (op string, val any, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, flt := range f.filters[doctype] {
		if len(flt) == 3 && flt[0] == field {
			op, _ = flt[1].(string)
			return op, flt[2], true
		}
	}
	return "", nil, false
}

func (f *fakeERP) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.filters))
	for k := range f.filters {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func purchaseInvoice(name, company string, docstatus int) map[string]any {
	return map[string]any{
		"name": name, "docstatus": docstatus, "company": company, "supplier": "SUP-Test",
		"posting_date": "2026-09-05", "net_total": "1000", "grand_total": "1180",
		"outstanding_amount": "0", "is_return": 0,
	}
}

func salesInvoice(name, company string, docstatus int) map[string]any {
	return map[string]any{
		"name": name, "docstatus": docstatus, "company": company, "customer": "CUST-Test",
		"posting_date": "2026-09-06", "net_total": "2000", "grand_total": "2360",
		"outstanding_amount": "0",
	}
}

func paymentEntry(name, company string, docstatus int) map[string]any {
	return map[string]any{
		"name": name, "docstatus": docstatus, "company": company, "payment_type": "Pay",
		"posting_date": "2026-09-07", "paid_amount": "1180", "received_amount": "1180",
		"unallocated_amount": "0",
	}
}

// docsFor returns, for each doctype, a submitted doc of the ERP company, a
// draft and a cancelled one, and a submitted doc filed under the company ID
// (which a reader passing the ID straight through would return).
func docsFor(mk func(name, company string, docstatus int) map[string]any, prefix string) []map[string]any {
	return []map[string]any{
		mk(prefix+"-SUBMITTED", testERPCompany, 1),
		mk(prefix+"-DRAFT", testERPCompany, 0),
		mk(prefix+"-CANCELLED", testERPCompany, 2),
		mk(prefix+"-BY-ID", testCompanyID, 1),
	}
}

func newFakeERP(t *testing.T) (*fakeERP, *frappe.Client) {
	t.Helper()
	f := &fakeERP{
		docs: map[string][]map[string]any{
			frappe.DocTypePurchaseInvoice: docsFor(purchaseInvoice, "PINV"),
			frappe.DocTypeSalesInvoice:    docsFor(salesInvoice, "SINV"),
			frappe.DocTypePaymentEntry:    docsFor(paymentEntry, "PE"),
			frappe.DocTypeAccount: {{
				"name": "Bank Charges - TT", "docstatus": 0, "account_name": "Bank Charges",
				"company": testERPCompany, "parent_account": "Indirect Expenses - TT",
				"is_group": 0, "root_type": "Expense", "account_type": "", "account_currency": "INR",
			}},
		},
		filters: map[string][][]any{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "testkey", config.NewSecret("testsecret"))
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestDirectBooksResolvesCompanyAndReadsSubmitted(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	names := func(n int, name func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = name(i)
		}
		return out
	}

	tests := []struct {
		name      string
		doctype   string
		docstatus bool // the list must filter docstatus = 1
		call      func(ctx context.Context, d *DirectBooks) ([]string, error)
		want      []string
	}{
		{
			name: "purchase invoices", doctype: frappe.DocTypePurchaseInvoice, docstatus: true,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.PurchaseInvoices(ctx, testCompanyID, from, to)
				return names(len(got), func(i int) string { return got[i].Name }), err
			},
			want: []string{"PINV-SUBMITTED"},
		},
		{
			name: "sales invoices", doctype: frappe.DocTypeSalesInvoice, docstatus: true,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.SalesInvoices(ctx, testCompanyID, from, to)
				return names(len(got), func(i int) string { return got[i].Name }), err
			},
			want: []string{"SINV-SUBMITTED"},
		},
		{
			name: "payment entries", doctype: frappe.DocTypePaymentEntry, docstatus: true,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.PaymentEntries(ctx, testCompanyID, from, to)
				return names(len(got), func(i int) string { return got[i].Name }), err
			},
			want: []string{"PE-SUBMITTED"},
		},
		{
			name: "recurring suppliers read submitted purchase invoices", doctype: frappe.DocTypePurchaseInvoice, docstatus: true,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				// PINV-SUBMITTED is dated 2026-09; one month seen meets minOccurrences 1.
				got, err := d.RecurringSuppliers(ctx, testCompanyID, "2026-10", 1, 1, 20)
				return names(len(got), func(i int) string { return got[i].Supplier }), err
			},
			want: []string{"SUP-Test"},
		},
		{
			name: "gl entries", doctype: frappe.DocTypeGLEntry,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.GLEntries(ctx, testCompanyID, from, to)
				return names(len(got), func(i int) string { return got[i].Name }), err
			},
			want: []string{},
		},
		{
			name: "trial balance", doctype: frappe.DocTypeAccount,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.TrialBalance(ctx, testCompanyID, from, to)
				return names(len(got.Rows), func(i int) string { return got.Rows[i].Account }), err
			},
			want: []string{},
		},
		{
			name: "account history", doctype: frappe.DocTypeAccount,
			call: func(ctx context.Context, d *DirectBooks) ([]string, error) {
				got, err := d.AccountHistory(ctx, testCompanyID, "Bank Charges - TT", "2026-09", 2)
				return names(len(got), func(i int) string { return got[i].Month }), err
			},
			want: []string{"2026-08", "2026-09"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			erp, client := newFakeERP(t)
			d := &DirectBooks{
				Client:    client,
				Companies: fakeDirectory{testCompanyID: {ID: testCompanyID, ERPCompany: testERPCompany}},
			}

			got, err := tc.call(t.Context(), d)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}

			requested := erp.requested()
			if len(requested) == 0 {
				t.Fatal("no list request reached ERPNext")
			}
			for _, doctype := range requested {
				op, val, ok := erp.filter(doctype, "company")
				if !ok || op != "=" || val != testERPCompany {
					t.Errorf("%s company filter = %v %v (present %v), want = %q", doctype, op, val, ok, testERPCompany)
				}
			}
			if !slices.Contains(requested, tc.doctype) {
				t.Errorf("requested doctypes %v, want %s among them", requested, tc.doctype)
			}
			if tc.docstatus {
				op, val, ok := erp.filter(tc.doctype, "docstatus")
				if !ok || op != "=" || fmt.Sprint(val) != "1" {
					t.Errorf("%s docstatus filter = %v %v (present %v), want = 1", tc.doctype, op, val, ok)
				}
			}
		})
	}
}

func TestDirectBooksCompanyResolutionErrors(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		companies CompanyDirectory
		company   string
		wantIs    error
	}{
		{name: "nil directory", companies: nil, company: testCompanyID},
		{name: "unknown company ID", companies: fakeDirectory{}, company: testCompanyID, wantIs: errUnknownCompany},
		{name: "empty ERPNext name", companies: fakeDirectory{testCompanyID: {ID: testCompanyID}}, company: testCompanyID},
		{name: "empty company ID", companies: fakeDirectory{testCompanyID: {ID: testCompanyID, ERPCompany: testERPCompany}}, company: ""},
	}

	calls := map[string]func(ctx context.Context, d *DirectBooks, company string) error{
		"TrialBalance": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.TrialBalance(ctx, c, from, to)
			return err
		},
		"GLEntries": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.GLEntries(ctx, c, from, to)
			return err
		},
		"PurchaseInvoices": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.PurchaseInvoices(ctx, c, from, to)
			return err
		},
		"SalesInvoices": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.SalesInvoices(ctx, c, from, to)
			return err
		},
		"PaymentEntries": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.PaymentEntries(ctx, c, from, to)
			return err
		},
		"AccountHistory": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.AccountHistory(ctx, c, "Bank Charges - TT", "2026-09", 2)
			return err
		},
		"RecurringSuppliers": func(ctx context.Context, d *DirectBooks, c string) error {
			_, err := d.RecurringSuppliers(ctx, c, "2026-10", 3, 3, 20)
			return err
		},
	}

	for _, tc := range tests {
		for method, call := range calls {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				erp, client := newFakeERP(t)
				d := &DirectBooks{Client: client, Companies: tc.companies}
				err := call(t.Context(), d, tc.company)
				if err == nil {
					t.Fatal("expected error")
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Errorf("err = %v, want it to wrap %v", err, tc.wantIs)
				}
				if got := erp.requested(); len(got) != 0 {
					t.Errorf("ERPNext was queried (%v) although the company did not resolve", got)
				}
			})
		}
	}
}
