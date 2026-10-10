package books

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

const (
	toolCompanyID = "test"
	otherCompany  = "Other Traders Pvt Ltd"
	hostile       = "x') or 1=1 --"
)

var toolNow = time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC)

// toolSession serves the books tools over a real MCP client session backed
// by f, logging nowhere.
func toolSession(t *testing.T, f *fakeERP) *mcp.ClientSession {
	t.Helper()
	return toolSessionLog(t, f, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// toolSessionLog is toolSession with the tools' logger.
func toolSessionLog(t *testing.T, f *fakeERP, log *slog.Logger) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "books-test", Version: "0"}, nil)
	RegisterTools(s, ToolDeps{
		Logger: log,
		Client: f.client(),
		Companies: ProfileCompanies([]seed.Profile{
			{ID: toolCompanyID, ERPCompany: testCompany},
			{ID: "other", ERPCompany: otherCompany},
		}),
		Now: func() time.Time { return toolNow },
	})
	return connect(t, s)
}

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and decodes its structured output into out, failing
// the test on a tool error.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", tool, resultText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal structured content: %v", tool, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, b, err)
	}
}

// callErr invokes a tool that must fail and returns its error text.
func callErr(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s(%v): want a tool error, got %+v", tool, args, res.StructuredContent)
	}
	return resultText(res)
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func september() map[string]any {
	return map[string]any{"company": toolCompanyID, "from_date": "2026-09-01", "to_date": "2026-09-30"}
}

func with(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		k := kv[i].(string)
		if kv[i+1] == nil {
			delete(out, k)
			continue
		}
		out[k] = kv[i+1]
	}
	return out
}

// filterFields returns the field position of every filter in a request:
// f[0] for [field, op, value] and f[1] for [doctype, field, op, value].
func filterFields(t *testing.T, r fakeRequest) []string {
	t.Helper()
	var out []string
	for _, raw := range r.filters {
		f := raw.([]any)
		if len(f) == 4 {
			out = append(out, f[1].(string))
		} else {
			out = append(out, f[0].(string))
		}
	}
	return out
}

// hasFilter reports whether r has the filter [field, op, value] with value
// exactly as given.
func hasFilter(r fakeRequest, field, op string, value any) bool {
	for _, raw := range r.filters {
		f := raw.([]any)
		if len(f) == 3 && f[0] == field && f[1] == op && reflect.DeepEqual(f[2], value) {
			return true
		}
	}
	return false
}

// ---- trial balance ----

func trialBalanceERP(t *testing.T) *fakeERP {
	f := newFakeERP(t)
	f.add("Account",
		accountDoc("Bank - TT", testCompany, "Asset"),
		accountDoc("Capital - TT", testCompany, "Equity"),
		accountDoc("Bank Charges - TT", testCompany, "Expense"),
	)
	f.add("GL Entry",
		glDoc("GLE-0001", testCompany, "Bank - TT", "1000.00", "0", "2026-08-01", 1, 0),
		glDoc("GLE-0002", testCompany, "Capital - TT", "0", "1000.00", "2026-08-01", 1, 0),
		glDoc("GLE-0003", testCompany, "Bank Charges - TT", "118.50", "0", "2026-09-14", 1, 0),
		glDoc("GLE-0004", testCompany, "Bank - TT", "0", "118.50", "2026-09-14", 1, 0),
		// Cancelled pair: never counts.
		glDoc("GLE-0005", testCompany, "Bank Charges - TT", "500", "0", "2026-09-20", 1, 1),
		glDoc("GLE-0006", testCompany, "Bank - TT", "0", "500", "2026-09-20", 1, 1),
		// After the period.
		glDoc("GLE-0007", testCompany, "Bank Charges - TT", "7", "0", "2026-10-01", 1, 0),
		glDoc("GLE-0008", testCompany, "Bank - TT", "0", "7", "2026-10-01", 1, 0),
	)
	return f
}

func TestToolsTrialBalance(t *testing.T) {
	cs := toolSession(t, trialBalanceERP(t))
	var got TrialBalanceOutput
	call(t, cs, ToolGetTrialBalance, september(), &got)
	want := TrialBalanceOutput{
		Company: toolCompanyID, FromDate: "2026-09-01", ToDate: "2026-09-30",
		Rows: []TBRow{
			{Account: "Bank - TT", AccountName: "Bank", RootType: "Asset", Opening: 100000, Credit: 11850, Closing: 88150},
			{Account: "Capital - TT", AccountName: "Capital", RootType: "Equity", Opening: -100000, Closing: -100000},
			{Account: "Bank Charges - TT", AccountName: "Bank Charges", RootType: "Expense", Debit: 11850, Closing: 11850},
		},
		Totals: TBTotals{Debit: 11850, Credit: 11850},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trial balance:\n got %+v\nwant %+v", got, want)
	}
}

// ---- GL entries and paging ----

func TestToolsGLEntriesPaging(t *testing.T) {
	f := newFakeERP(t)
	const n = 2*GLPageSize + 250
	var want []string
	for i := range n {
		name := fmt.Sprintf("GLE-%05d", i)
		f.add("GL Entry", glDoc(name, testCompany, "Bank - TT", "12.34", "0", "2026-09-10", 1, 0))
		want = append(want, name)
		// Noise interleaved by name: none of it may appear.
		if i%100 == 0 {
			f.add("GL Entry",
				glDoc(name+"-cancelled", testCompany, "Bank - TT", "1", "0", "2026-09-10", 1, 1),
				glDoc(name+"-draft", testCompany, "Bank - TT", "1", "0", "2026-09-10", 0, 0),
				glDoc(name+"-other", otherCompany, "Bank - TT", "1", "0", "2026-09-10", 1, 0),
				glDoc(name+"-august", testCompany, "Bank - TT", "1", "0", "2026-08-31", 1, 0),
			)
		}
	}
	cs := toolSession(t, f)

	var got []string
	var cursors []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 3 {
			t.Fatal("more than three pages")
		}
		args := september()
		if cursor != "" {
			args["cursor"] = cursor
		}
		var out GLEntriesOutput
		call(t, cs, ToolListGLEntries, args, &out)
		for _, e := range out.Entries {
			got = append(got, e.Name)
			if e.Debit != 1234 || e.Credit != 0 || e.PostingDate != "2026-09-10" || e.Account != "Bank - TT" {
				t.Fatalf("entry %+v: want debit 1234 paise on 2026-09-10", e)
			}
		}
		if out.NextCursor == "" {
			if len(out.Entries) != 250 {
				t.Errorf("last page has %d entries, want 250", len(out.Entries))
			}
			break
		}
		if len(out.Entries) != GLPageSize {
			t.Errorf("page %d has %d entries, want %d", page, len(out.Entries), GLPageSize)
		}
		cursor = out.NextCursor
		cursors = append(cursors, cursor)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pages hold %d entries, want the %d in name order exactly once", len(got), len(want))
	}
	if len(cursors) != 2 {
		t.Fatalf("got %d cursors, want 2", len(cursors))
	}

	// Each page after the first is a keyset query on name, ordered by name.
	reqs := f.lists("GL Entry")
	var keyset []string
	for _, r := range reqs {
		if r.orderBy != "name asc" {
			t.Errorf("order_by = %q, want name asc", r.orderBy)
		}
		for _, raw := range r.filters {
			if fl := raw.([]any); fl[0] == "name" && fl[1] == ">" {
				keyset = append(keyset, fl[2].(string))
			}
		}
	}
	if len(keyset) == 0 || !slices.Contains(keyset, want[GLPageSize-1]) || !slices.Contains(keyset, want[2*GLPageSize-1]) {
		t.Errorf("keyset filters %v don't start after %s and %s", slices.Compact(keyset), want[GLPageSize-1], want[2*GLPageSize-1])
	}

	// A cursor is stable when rows are inserted before it.
	f.add("GL Entry", glDoc("GLE-00000-late", testCompany, "Bank - TT", "1", "0", "2026-09-10", 1, 0))
	var again GLEntriesOutput
	call(t, cs, ToolListGLEntries, with(september(), "cursor", cursors[1]), &again)
	if len(again.Entries) != 250 || again.Entries[0].Name != want[2*GLPageSize] {
		t.Errorf("after an insert, page 3 starts at %v with %d entries", again.Entries[0].Name, len(again.Entries))
	}
}

func TestToolsGLEntriesFilters(t *testing.T) {
	f := trialBalanceERP(t)
	party := glDoc("GLE-0100", testCompany, "Creditors - TT", "0", "250", "2026-09-05", 1, 0)
	party["party_type"], party["party"] = "Supplier", "SUP-A"
	f.add("GL Entry", party)
	cs := toolSession(t, f)

	var byAccount GLEntriesOutput
	call(t, cs, ToolListGLEntries, with(september(), "account", "Bank Charges - TT"), &byAccount)
	if len(byAccount.Entries) != 1 || byAccount.Entries[0].Name != "GLE-0003" || byAccount.Entries[0].Debit != 11850 {
		t.Errorf("account filter: got %+v, want only GLE-0003 with 11850 paise", byAccount.Entries)
	}
	if byAccount.NextCursor != "" {
		t.Errorf("single page: next_cursor = %q, want empty", byAccount.NextCursor)
	}

	var byParty GLEntriesOutput
	call(t, cs, ToolListGLEntries, with(september(), "party", "SUP-A"), &byParty)
	if len(byParty.Entries) != 1 || byParty.Entries[0].Party != "SUP-A" || byParty.Entries[0].Credit != 25000 {
		t.Errorf("party filter: got %+v", byParty.Entries)
	}

	reqs := f.lists("GL Entry")
	if !hasFilter(reqs[0], "account", "=", "Bank Charges - TT") {
		t.Errorf("account filter not sent as a filter value: %v", reqs[0].filters)
	}
}

// ---- purchase invoices ----

func purchaseERP(t *testing.T) *fakeERP {
	f := newFakeERP(t)
	intra := []doc{
		taxRow("tax-c", "Input Tax CGST - TT", "9", "4500.00", "cgst"),
		taxRow("tax-s", "Input Tax SGST - TT", "9", "4500.00", "sgst"),
	}
	inter := []doc{taxRow("tax-i", "Input Tax IGST - TT", "18", "1800.18", "igst")}
	billed := purchaseDoc("PINV-0006", testCompany, "SUP-B", "2026-09-20", 1, "10001.00", "11801.18", inter)
	billed["bill_date"] = "2026-09-18"
	f.add("Purchase Invoice",
		purchaseDoc("PINV-0001", testCompany, "SUP-A", "2026-09-05", 1, "50000.00", "59000.00", intra),
		purchaseDoc("PINV-0002", testCompany, "SUP-A", "2026-09-06", 0, "1.00", "1.00", nil),  // draft
		purchaseDoc("PINV-0003", testCompany, "SUP-A", "2026-09-07", 2, "2.00", "2.00", nil),  // cancelled
		purchaseDoc("PINV-0004", otherCompany, "SUP-A", "2026-09-08", 1, "3.00", "3.00", nil), // other company
		purchaseDoc("PINV-0005", testCompany, "SUP-A", "2026-08-31", 1, "4.00", "4.00", nil),  // August
		billed,
	)
	return f
}

func TestToolsPurchaseInvoices(t *testing.T) {
	f := purchaseERP(t)
	cs := toolSession(t, f)
	var got PurchaseInvoicesOutput
	call(t, cs, ToolListPurchaseInvoices, september(), &got)
	if len(got.Invoices) != 2 {
		t.Fatalf("got %d invoices, want 2 (drafts, cancelled, other companies and August excluded): %+v", len(got.Invoices), got.Invoices)
	}
	a, b := got.Invoices[0], got.Invoices[1]
	if a.Name != "PINV-0001" || a.Taxable != 5000000 || a.CGST != 450000 || a.SGST != 450000 || a.IGST != 0 || a.GrandTotal != 5900000 {
		t.Errorf("PINV-0001 = %+v, want taxable 5000000, CGST and SGST 450000, grand 5900000 paise", a)
	}
	if a.PostingDate != "2026-09-05" || a.BillDate != "" || a.SupplierGSTIN != "27AAAAA0000A1Z5" || a.BillNo != "B-PINV-0001" {
		t.Errorf("PINV-0001 header = %+v", a)
	}
	if len(a.Lines) != 1 || a.Lines[0].Account != "Rent - TT" || a.Lines[0].Amount != 5000000 || a.Lines[0].Description != "Monthly service" {
		t.Errorf("PINV-0001 lines = %+v", a.Lines)
	}
	if len(a.Taxes) != 2 || a.Taxes[0].Rate != "9" || a.Taxes[0].TaxAmount != 450000 || a.Taxes[0].GSTTaxType != "cgst" {
		t.Errorf("PINV-0001 taxes = %+v", a.Taxes)
	}
	if b.Name != "PINV-0006" || b.IGST != 180018 || b.Taxable != 1000100 || b.GrandTotal != 1180118 || b.BillDate != "2026-09-18" {
		t.Errorf("PINV-0006 = %+v, want IGST 180018 paise and bill date 2026-09-18", b)
	}

	var bySupplier PurchaseInvoicesOutput
	call(t, cs, ToolListPurchaseInvoices, with(september(), "supplier", "SUP-B"), &bySupplier)
	if len(bySupplier.Invoices) != 1 || bySupplier.Invoices[0].Name != "PINV-0006" {
		t.Errorf("supplier filter: got %+v", bySupplier.Invoices)
	}
}

func TestToolsGSTSplit(t *testing.T) {
	cases := []struct {
		gstType, head string
		want          string
	}{
		{"igst", "Input Tax IGST - TT", "igst"},
		{"CGST", "anything", "cgst"},
		{"", "Input Tax SGST - TT", "sgst"},
		{"cess", "Input Tax IGST - TT", ""},
		{"", "Freight - TT", ""},
	}
	for _, c := range cases {
		if got := gstKind(c.gstType, c.head); got != c.want {
			t.Errorf("gstKind(%q, %q) = %q, want %q", c.gstType, c.head, got, c.want)
		}
	}
}

// ---- hostile filter values ----

// TestToolsHostileFilterValue sends an SQL-looking value through every
// free-text filter and shows it reaches Frappe only as a filter value:
// the field position of every filter is a constant, and fields and
// order_by never carry model text.
func TestToolsHostileFilterValue(t *testing.T) {
	cases := []struct {
		tool, arg, field, doctype string
		allowed                   []string
	}{
		{ToolListPurchaseInvoices, "supplier", "supplier", "Purchase Invoice", []string{"company", "docstatus", "posting_date", "supplier"}},
		{ToolListPayments, "party", "party", "Payment Entry", []string{"company", "docstatus", "posting_date", "party"}},
		{ToolListGLEntries, "account", "account", "GL Entry", []string{"company", "docstatus", "is_cancelled", "posting_date", "account"}},
		{ToolListGLEntries, "party", "party", "GL Entry", []string{"company", "docstatus", "is_cancelled", "posting_date", "party"}},
	}
	for _, c := range cases {
		t.Run(c.tool+"/"+c.arg, func(t *testing.T) {
			f := purchaseERP(t)
			cs := toolSession(t, f)
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: c.tool, Arguments: with(september(), c.arg, hostile)})
			if err != nil || res.IsError {
				t.Fatalf("hostile value should be a harmless filter value, got err %v / %s", err, resultText(res))
			}
			reqs := f.lists(c.doctype)
			if len(reqs) == 0 {
				t.Fatal("no list request")
			}
			for _, r := range reqs {
				if !hasFilter(r, c.field, "=", hostile) {
					t.Errorf("filters %v lack [%q, \"=\", %q]", r.filters, c.field, hostile)
				}
				for _, fl := range filterFields(t, r) {
					if !slices.Contains(c.allowed, fl) {
						t.Errorf("filter field %q is not one of %v", fl, c.allowed)
					}
				}
				for _, fl := range r.fields {
					if strings.Contains(fl, "'") || strings.Contains(fl, " ") {
						t.Errorf("field %q carries model text", fl)
					}
				}
				if strings.Contains(r.orderBy, "'") || strings.Contains(r.orderBy, "1=1") {
					t.Errorf("order_by %q carries model text", r.orderBy)
				}
			}
		})
	}
}

// ---- sales invoices ----

func TestToolsSalesInvoices(t *testing.T) {
	f := newFakeERP(t)
	f.add("Sales Invoice",
		salesDoc("SINV-0001", testCompany, "CUST-1", "2026-09-02", 1, "1180.00", "0"),
		salesDoc("SINV-0002", testCompany, "CUST-2", "2026-09-03", 1, "2360.50", "0"),
		salesDoc("SINV-0003", testCompany, "CUST-3", "2026-09-04", 1, "590.00", "590.00"),
		salesDoc("SINV-0004", testCompany, "CUST-3", "2026-09-05", 0, "1.00", "1.00"), // draft
		salesDoc("SINV-0005", testCompany, "CUST-3", "2026-09-06", 2, "1.00", "1.00"), // cancelled
	)
	f.add("Payment Entry",
		paymentDoc("PE-R1", testCompany, "Receive", "CUST-1", "2026-09-02", 1, "1180.00", "Payment Gateway Clearing - TT",
			refRow("r1", "Sales Invoice", "SINV-0001", "1180.00")),
		paymentDoc("PE-R2", testCompany, "Receive", "CUST-2", "2026-10-03", 1, "2360.50", "Bank - TT",
			refRow("r2", "Sales Invoice", "SINV-0002", "2360.50")),
		// A cancelled receipt does not collect SINV-0003.
		paymentDoc("PE-R3", testCompany, "Receive", "CUST-3", "2026-09-10", 2, "590.00", "Bank - TT",
			refRow("r3", "Sales Invoice", "SINV-0003", "590.00")),
	)
	cs := toolSession(t, f)
	var got SalesInvoicesOutput
	call(t, cs, ToolListSalesInvoices, september(), &got)
	if len(got.Invoices) != 3 {
		t.Fatalf("got %d invoices, want 3: %+v", len(got.Invoices), got.Invoices)
	}
	wantVia := map[string]string{"SINV-0001": "Payment Gateway Clearing - TT", "SINV-0002": "Bank - TT", "SINV-0003": ""}
	for _, s := range got.Invoices {
		if s.CollectedVia != wantVia[s.Name] {
			t.Errorf("%s collected_via = %q, want %q", s.Name, s.CollectedVia, wantVia[s.Name])
		}
	}
	if s := got.Invoices[1]; s.GrandTotal != 236050 || s.OutstandingAmount != 0 || s.Customer != "CUST-2" || s.PostingDate != "2026-09-03" || len(s.Lines) != 1 || s.Lines[0].Account != "Sales - TT" {
		t.Errorf("SINV-0002 = %+v, want grand total 236050 paise", s)
	}
	// The receipt lookup filters on the child table with the invoice
	// names as a value.
	pe := f.lists("Payment Entry")
	if len(pe) == 0 || !slices.Contains(filterFields(t, pe[0]), "reference_name") {
		t.Errorf("receipt lookup filters = %v", pe)
	}
}

// ---- payments ----

func TestToolsPayments(t *testing.T) {
	f := newFakeERP(t)
	f.add("Payment Entry",
		paymentDoc("PE-0001", testCompany, "Pay", "SUP-A", "2026-09-10", 1, "59000.00", "Creditors - TT",
			refRow("p1", "Purchase Invoice", "PINV-0001", "59000.00")),
		paymentDoc("PE-0002", testCompany, "Receive", "CUST-1", "2026-09-11", 1, "1180.25", "Bank - TT"),
		paymentDoc("PE-0003", testCompany, "Pay", "SUP-A", "2026-09-12", 0, "1", "Creditors - TT"), // draft
		paymentDoc("PE-0004", testCompany, "Pay", "SUP-A", "2026-09-13", 2, "1", "Creditors - TT"), // cancelled
		paymentDoc("PE-0005", otherCompany, "Pay", "SUP-A", "2026-09-14", 1, "1", "Creditors - TT"),
	)
	cs := toolSession(t, f)
	var got PaymentsOutput
	call(t, cs, ToolListPayments, september(), &got)
	if len(got.Payments) != 2 {
		t.Fatalf("got %d payments, want 2: %+v", len(got.Payments), got.Payments)
	}
	p := got.Payments[0]
	if p.Name != "PE-0001" || p.Amount != 5900000 || p.ReferenceNo != "UTRPE-0001" || p.PostingDate != "2026-09-10" || p.PaymentType != "Pay" {
		t.Errorf("PE-0001 = %+v", p)
	}
	if len(p.Invoices) != 1 || p.Invoices[0].ReferenceName != "PINV-0001" || p.Invoices[0].AllocatedAmount != 5900000 {
		t.Errorf("PE-0001 invoices = %+v", p.Invoices)
	}
	if r := got.Payments[1]; r.Amount != 118025 || r.Invoices == nil || len(r.Invoices) != 0 {
		t.Errorf("PE-0002 = %+v, want amount 118025 paise and an empty invoices list", r)
	}

	var byParty PaymentsOutput
	call(t, cs, ToolListPayments, with(september(), "party", "CUST-1"), &byParty)
	if len(byParty.Payments) != 1 || byParty.Payments[0].Name != "PE-0002" {
		t.Errorf("party filter: got %+v", byParty.Payments)
	}
}

// ---- account history ----

func TestToolsAccountHistory(t *testing.T) {
	cs := toolSession(t, trialBalanceERP(t))
	var got AccountHistoryOutput
	call(t, cs, ToolGetAccountHistory, map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 2}, &got)
	want := AccountHistoryOutput{
		Account: "Bank - TT", ThroughMonth: "2026-09",
		History: []MonthTotal{
			{Month: "2026-08", Debit: 100000, Net: 100000},
			{Month: "2026-09", Credit: 11850, Net: -11850},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default through_month (from Now):\n got %+v\nwant %+v", got, want)
	}

	var oct AccountHistoryOutput
	call(t, cs, ToolGetAccountHistory, map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 1, "through_month": "2026-10"}, &oct)
	if len(oct.History) != 1 || oct.History[0].Credit != 700 || oct.ThroughMonth != "2026-10" {
		t.Errorf("through_month 2026-10: %+v", oct)
	}
}

// ---- recurring suppliers ----

func TestToolsRecurringSuppliers(t *testing.T) {
	f := newFakeERP(t)
	for i, m := range []string{"2026-06", "2026-07", "2026-08"} {
		f.add("Purchase Invoice",
			purchaseDoc(fmt.Sprintf("PINV-R%d", i), testCompany, "SUP-RENT", m+"-05", 1, "50000.00", "59000.00", nil),
			purchaseDoc(fmt.Sprintf("PINV-X%d", i), testCompany, "SUP-X", m+"-20", 1, "1", fmt.Sprint(1000*(i+1)), nil),
		)
	}
	f.add("Purchase Invoice",
		purchaseDoc("PINV-RD", testCompany, "SUP-RENT", "2026-07-06", 0, "1", "1", nil), // draft: ignored
		purchaseDoc("PINV-R9", testCompany, "SUP-RENT", "2026-09-05", 1, "1", "1", nil), // the closing month: not read
		purchaseDoc("PINV-R5", testCompany, "SUP-RENT", "2026-05-05", 1, "1", "1", nil), // before the lookback
	)
	cs := toolSession(t, f)
	args := map[string]any{"company": toolCompanyID, "before_month": "2026-09", "lookback_months": 3, "min_occurrences": 3}
	var got RecurringSuppliersOutput
	call(t, cs, ToolListRecurringSuppliers, args, &got)
	want := []RecurringSupplier{{Supplier: "SUP-RENT", MedianAmount: 5900000, TypicalDay: 5, MonthsSeen: []string{"2026-06", "2026-07", "2026-08"}}}
	if !reflect.DeepEqual(got.Suppliers, want) {
		t.Errorf("suppliers:\n got %+v\nwant %+v", got.Suppliers, want)
	}
	reqs := f.lists("Purchase Invoice")
	if len(reqs) == 0 || !hasFilter(reqs[0], "posting_date", "between", []any{"2026-06-01", "2026-08-31"}) {
		t.Errorf("window filter = %v, want 2026-06-01..2026-08-31", reqs)
	}

	var wide RecurringSuppliersOutput
	call(t, cs, ToolListRecurringSuppliers, with(args, "amount_band_pct", 100), &wide)
	if len(wide.Suppliers) != 2 {
		t.Errorf("amount_band_pct 100: got %+v, want SUP-RENT and SUP-X", wide.Suppliers)
	}

	var none RecurringSuppliersOutput
	call(t, cs, ToolListRecurringSuppliers, with(args, "min_occurrences", 4), &none)
	if none.Suppliers == nil || len(none.Suppliers) != 0 {
		t.Errorf("min_occurrences 4: got %#v, want an empty list", none.Suppliers)
	}
}

func TestRecurringSuppliersRule(t *testing.T) {
	if got := RecurringSuppliers(nil, 1, 20); got != nil {
		t.Errorf("no invoices: got %v, want nil", got)
	}
}

// ---- validation ----

func TestToolsValidation(t *testing.T) {
	long := strings.Repeat("a", MaxFilterLen+1)
	hist := map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 3}
	rec := map[string]any{"company": toolCompanyID, "before_month": "2026-09", "lookback_months": 3, "min_occurrences": 2}
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown company", ToolGetTrialBalance, with(september(), "company", "acme"), `company unknown company ID "acme"`},
		{"company name not ID", ToolListGLEntries, with(september(), "company", testCompany), "company unknown company ID"},
		{"empty company", ToolListPayments, with(september(), "company", ""), "company is required"},
		{"missing company", ToolListPayments, with(september(), "company", nil), "company"},
		{"bad from_date", ToolListGLEntries, with(september(), "from_date", "2026-9-01"), "from_date must be a date in YYYY-MM-DD form"},
		{"bad to_date", ToolListSalesInvoices, with(september(), "to_date", "30/09/2026"), "to_date must be a date in YYYY-MM-DD form"},
		{"impossible date", ToolGetTrialBalance, with(september(), "to_date", "2026-09-31"), "to_date must be a date"},
		{"empty from_date", ToolListPurchaseInvoices, with(september(), "from_date", ""), "from_date is required"},
		{"from after to", ToolListPayments, with(september(), "from_date", "2026-10-01"), "from_date 2026-10-01 must not be after to_date"},
		{"range too long", ToolListGLEntries, with(september(), "from_date", "2025-08-26"), "covers 401 days; the most is 400"},
		{"control char filter", ToolListPurchaseInvoices, with(september(), "supplier", "SUP\nA"), "supplier must be printable text"},
		{"long filter", ToolListGLEntries, with(september(), "account", long), "account must be at most 140 characters"},
		{"control char party", ToolListPayments, with(september(), "party", "a\x00b"), "party must be printable text"},
		{"raw order_by refused", ToolListGLEntries, with(september(), "order_by", "name desc"), "order_by"},
		{"raw fields refused", ToolListPurchaseInvoices, with(september(), "fields", []string{"name"}), "fields"},
		{"malformed cursor", ToolListGLEntries, with(september(), "cursor", "!!not-base64!!"), "cursor is not a next_cursor"},
		{"padded cursor", ToolListGLEntries, with(september(), "cursor", "R0xFLTAwMDAx=="), "cursor is not a next_cursor"},
		{"cursor of control chars", ToolListGLEntries, with(september(), "cursor", encodeCursor("\x01\x02")), "cursor is not a next_cursor"},
		{"months zero", ToolGetAccountHistory, with(hist, "months", 0), "months must be 1 to 12, got 0"},
		{"months 13", ToolGetAccountHistory, with(hist, "months", 13), "months must be 1 to 12, got 13"},
		{"empty account", ToolGetAccountHistory, with(hist, "account", ""), "account is required"},
		{"bad through_month", ToolGetAccountHistory, with(hist, "through_month", "2026-9"), "through_month must be a month in YYYY-MM form"},
		{"bad before_month", ToolListRecurringSuppliers, with(rec, "before_month", "2026-09-01"), "before_month must be a month in YYYY-MM form"},
		{"lookback zero", ToolListRecurringSuppliers, with(rec, "lookback_months", 0), "lookback_months must be 1 to 12, got 0"},
		{"lookback 13", ToolListRecurringSuppliers, with(rec, "lookback_months", 13), "lookback_months must be 1 to 12"},
		{"min_occurrences 13", ToolListRecurringSuppliers, with(rec, "min_occurrences", 13), "min_occurrences must be 1 to 12, got 13"},
		{"band 101", ToolListRecurringSuppliers, with(rec, "amount_band_pct", 101), "amount_band_pct must be 0 to 100"},
		{"unknown company recurring", ToolListRecurringSuppliers, with(rec, "company", "acme"), "unknown company ID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeERP(t)
			cs := toolSession(t, f)
			msg := callErr(t, cs, c.tool, c.args)
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q, want it to contain %q", msg, c.want)
			}
			if n := len(f.requests()); n != 0 {
				t.Errorf("%d requests reached ERPNext before validation failed", n)
			}
		})
	}
}

func TestToolsRangeLimitInclusive(t *testing.T) {
	// 400 days, both ends included, is allowed.
	if _, _, err := dateRange("2025-08-27", "2026-09-30"); err != nil {
		t.Errorf("400-day range refused: %v", err)
	}
	if _, _, err := dateRange("2026-09-30", "2026-09-30"); err != nil {
		t.Errorf("one-day range refused: %v", err)
	}
}

func TestToolsCursorRoundTrip(t *testing.T) {
	for _, name := range []string{"GLE-00001", "ACC-GLE-2026-00042", "a/b c", "नाम"} {
		got, err := decodeCursor(encodeCursor(name))
		if err != nil || got != name {
			t.Errorf("round trip %q: got %q, %v", name, got, err)
		}
	}
}

func TestToolsMissingDeps(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "books-test", Version: "0"}, nil)
	var logs bytes.Buffer
	RegisterTools(s, ToolDeps{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	cs := connect(t, s)
	if msg := callErr(t, cs, ToolGetTrialBalance, september()); msg != MsgUnavailable {
		t.Errorf("nil client: %q, want %q", msg, MsgUnavailable)
	}
	if !strings.Contains(logs.String(), "no ERPNext client") {
		t.Errorf("log lacks the real error: %s", logs.String())
	}
}

func TestProfileCompanies(t *testing.T) {
	r := ProfileCompanies([]seed.Profile{{ID: "sharma", ERPCompany: "Sharma Traders Pvt Ltd"}})
	if got, err := r.ERPCompany("sharma"); err != nil || got != "Sharma Traders Pvt Ltd" {
		t.Errorf("sharma: %q, %v", got, err)
	}
	if _, err := r.ERPCompany("Sharma Traders Pvt Ltd"); err == nil {
		t.Error("an ERPNext name is not a company ID")
	}
}

// TestToolsPaiseOutput pins the paise conversion of a rupee amount with
// paise through the whole tool path.
func TestToolsPaiseOutput(t *testing.T) {
	f := newFakeERP(t)
	f.add("GL Entry", glDoc("GLE-1", testCompany, "Bank - TT", "0", "1234567.89", "2026-09-01", 1, 0))
	cs := toolSession(t, f)
	var out GLEntriesOutput
	call(t, cs, ToolListGLEntries, september(), &out)
	if len(out.Entries) != 1 || out.Entries[0].Credit != money.Paise(123456789) {
		t.Errorf("credit = %+v, want 123456789 paise", out.Entries)
	}
}
