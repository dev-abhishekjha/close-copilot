//go:build integration

package books

// Read-only integration test for the CC-502 books tools against the local
// erp.localhost site, which holds Sharma Traders' books for 2026-08 and
// 2026-09 (CC-304). Each tool, called through a real MCP client, must
// return exactly what the direct internal/frappe and internal/books calls
// return. It never writes, so it takes no ERPNext lock. Run with the bot
// keys in the environment, never on argv:
//
//	sh -c 'set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/books/... -run TestToolsIntegration -count=1 -v'

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

const (
	sharmaID   = "sharma"
	seedHint   = "seed it first: go run ./cmd/seed books --company sharma --month 2026-09 --small (and 2026-08)"
	intProfile = "../../config/companies"
)

func integrationSession(t *testing.T, c *frappe.Client) *mcp.ClientSession {
	t.Helper()
	profiles, err := seed.LoadProfiles(intProfile)
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "books-int", Version: "0"}, nil)
	RegisterTools(s, ToolDeps{Client: c, Companies: ProfileCompanies(profiles), Now: time.Now})
	return connect(t, s)
}

// sameJSONValue compares two values by their JSON forms, so nil and empty
// slices of omitempty fields don't count as differences.
func sameJSONValue(t *testing.T, name string, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gv, wv any
	_ = json.Unmarshal(g, &gv)
	_ = json.Unmarshal(w, &wv)
	if !reflect.DeepEqual(gv, wv) {
		t.Errorf("%s: tool output differs from the direct call\n tool:   %.2000s\n direct: %.2000s", name, g, w)
	}
}

// directSubmitted lists and fetches submitted documents of doctype the
// plain way: List for names, GetMany for the documents with child tables.
func directSubmitted[R any](t *testing.T, c *frappe.Client, doctype string, filters [][]any) []R {
	t.Helper()
	rows, err := frappe.List[named](t.Context(), c, doctype, frappe.Query{
		Fields: []string{"name"}, Filters: filters, OrderBy: "posting_date asc, name asc",
	})
	if err != nil {
		t.Fatalf("list %s: %v", doctype, err)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	docs, err := frappe.GetMany[R](t.Context(), c, doctype, names)
	if err != nil {
		t.Fatalf("get %s: %v", doctype, err)
	}
	return docs
}

func septFilters(extra ...[]any) [][]any {
	f := [][]any{
		{"company", "=", stplCompany},
		{"docstatus", "=", 1},
		{"posting_date", "between", []string{"2026-09-01", "2026-09-30"}},
	}
	return append(f, extra...)
}

func TestToolsIntegration(t *testing.T) {
	cfg := integrationConfig(t)
	l := botLedger(t, cfg)
	cs := integrationSession(t, l.C)
	ctx := t.Context()
	sept := map[string]any{"company": sharmaID, "from_date": "2026-09-01", "to_date": "2026-09-30"}
	from, to := day("2026-09-01"), day("2026-09-30")

	direct, err := l.GLEntries(ctx, stplCompany, from, to)
	if err != nil {
		t.Fatalf("direct GL entries: %v", err)
	}
	if len(direct) == 0 {
		t.Fatalf("no GL Entries for sharma 2026-09 in ERPNext; %s", seedHint)
	}

	t.Run(ToolGetTrialBalance, func(t *testing.T) {
		tb, err := TrialBalance(ctx, l, stplCompany, from, to)
		if err != nil {
			t.Fatal(err)
		}
		var got TrialBalanceOutput
		call(t, cs, ToolGetTrialBalance, sept, &got)
		sameJSONValue(t, ToolGetTrialBalance, got, TrialBalanceOutput{
			Company: sharmaID, FromDate: "2026-09-01", ToDate: "2026-09-30", Rows: tb.Rows, Totals: tb.Totals,
		})
	})

	t.Run(ToolListGLEntries, func(t *testing.T) {
		walk := func(args map[string]any) []GLEntryOut {
			var all []GLEntryOut
			cursor := ""
			for range 1000 {
				a := with(args)
				if cursor != "" {
					a["cursor"] = cursor
				}
				var page GLEntriesOutput
				call(t, cs, ToolListGLEntries, a, &page)
				all = append(all, page.Entries...)
				if page.NextCursor == "" {
					return all
				}
				cursor = page.NextCursor
			}
			t.Fatal("list_gl_entries did not reach a last page")
			return nil
		}
		want := make([]GLEntryOut, 0, len(direct))
		var bank []GLEntryOut
		for _, g := range direct {
			if g.Docstatus != 1 {
				continue
			}
			want = append(want, glOut(g))
			if g.Account == stplBank {
				bank = append(bank, glOut(g))
			}
		}
		sameJSONValue(t, "list_gl_entries (all pages)", walk(sept), want)
		if len(bank) == 0 {
			t.Fatalf("no entries on %s in 2026-09; %s", stplBank, seedHint)
		}
		sameJSONValue(t, "list_gl_entries (account filter)", walk(with(sept, "account", stplBank)), bank)
	})

	t.Run(ToolListPurchaseInvoices, func(t *testing.T) {
		raws := directSubmitted[frappe.PurchaseInvoiceRaw](t, l.C, frappe.DocTypePurchaseInvoice, septFilters())
		if len(raws) == 0 {
			t.Fatalf("no purchase invoices for sharma 2026-09; %s", seedHint)
		}
		want := make([]PurchaseInvoiceOut, 0, len(raws))
		var gst int
		for _, r := range raws {
			p, err := r.Domain()
			if err != nil {
				t.Fatal(err)
			}
			o := purchaseOut(p)
			if o.IGST+o.CGST+o.SGST != 0 {
				gst++
			}
			want = append(want, o)
		}
		var got PurchaseInvoicesOutput
		call(t, cs, ToolListPurchaseInvoices, sept, &got)
		sameJSONValue(t, ToolListPurchaseInvoices, got.Invoices, want)
		if gst == 0 {
			t.Error("no September purchase invoice has IGST, CGST or SGST; check the gst_tax_type mapping")
		}

		supplier := want[0].Supplier
		var one PurchaseInvoicesOutput
		call(t, cs, ToolListPurchaseInvoices, with(sept, "supplier", supplier), &one)
		var wantOne []PurchaseInvoiceOut
		for _, p := range want {
			if p.Supplier == supplier {
				wantOne = append(wantOne, p)
			}
		}
		sameJSONValue(t, "list_purchase_invoices (supplier filter)", one.Invoices, wantOne)
	})

	t.Run(ToolListSalesInvoices, func(t *testing.T) {
		raws := directSubmitted[frappe.SalesInvoiceRaw](t, l.C, frappe.DocTypeSalesInvoice, septFilters())
		if len(raws) == 0 {
			t.Fatalf("no sales invoices for sharma 2026-09; %s", seedHint)
		}
		// collected_via, worked out independently of the child-table
		// filter: every submitted receipt from September on, in full.
		receipts := directSubmitted[frappe.PaymentEntryRaw](t, l.C, frappe.DocTypePaymentEntry, [][]any{
			{"company", "=", stplCompany},
			{"docstatus", "=", 1},
			{"payment_type", "=", "Receive"},
			{"posting_date", ">=", "2026-09-01"},
		})
		via := map[string][]string{}
		for _, r := range receipts {
			for _, ref := range r.References {
				if ref.ReferenceDoctype == frappe.DocTypeSalesInvoice && !slices.Contains(via[ref.ReferenceName], r.PaidTo) {
					via[ref.ReferenceName] = append(via[ref.ReferenceName], r.PaidTo)
				}
			}
		}
		want := make([]SalesInvoiceOut, 0, len(raws))
		collected := 0
		for _, r := range raws {
			s, err := r.Domain()
			if err != nil {
				t.Fatal(err)
			}
			accs := via[s.Name]
			slices.Sort(accs)
			if len(accs) > 0 {
				collected++
			}
			want = append(want, salesOut(s, strings.Join(accs, "; ")))
		}
		var got SalesInvoicesOutput
		call(t, cs, ToolListSalesInvoices, sept, &got)
		sameJSONValue(t, ToolListSalesInvoices, got.Invoices, want)
		if collected == 0 {
			t.Error("no September sales invoice has a receipt; collected_via is untested")
		}
	})

	t.Run(ToolListPayments, func(t *testing.T) {
		raws := directSubmitted[frappe.PaymentEntryRaw](t, l.C, frappe.DocTypePaymentEntry, septFilters())
		if len(raws) == 0 {
			t.Fatalf("no payment entries for sharma 2026-09; %s", seedHint)
		}
		want := make([]PaymentOut, 0, len(raws))
		for _, r := range raws {
			p, err := r.Domain()
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, paymentOut(p))
		}
		var got PaymentsOutput
		call(t, cs, ToolListPayments, sept, &got)
		sameJSONValue(t, ToolListPayments, got.Payments, want)

		party := ""
		for _, p := range want {
			if p.Party != "" {
				party = p.Party
				break
			}
		}
		if party == "" {
			t.Fatal("no September payment has a party")
		}
		var wantParty []PaymentOut
		for _, p := range want {
			if p.Party == party {
				wantParty = append(wantParty, p)
			}
		}
		var byParty PaymentsOutput
		call(t, cs, ToolListPayments, with(sept, "party", party), &byParty)
		sameJSONValue(t, "list_payments (party filter)", byParty.Payments, wantParty)
	})

	t.Run(ToolGetAccountHistory, func(t *testing.T) {
		hist, err := AccountHistory(ctx, l, stplCompany, stplBank, "2026-09", 2)
		if err != nil {
			t.Fatal(err)
		}
		var got AccountHistoryOutput
		call(t, cs, ToolGetAccountHistory, map[string]any{
			"company": sharmaID, "account": stplBank, "months": 2, "through_month": "2026-09",
		}, &got)
		sameJSONValue(t, ToolGetAccountHistory, got, AccountHistoryOutput{Account: stplBank, ThroughMonth: "2026-09", History: hist})
	})

	t.Run(ToolListRecurringSuppliers, func(t *testing.T) {
		raws := directSubmitted[frappe.PurchaseInvoiceRaw](t, l.C, frappe.DocTypePurchaseInvoice, [][]any{
			{"company", "=", stplCompany},
			{"docstatus", "=", 1},
			{"posting_date", "between", []string{"2026-08-01", "2026-09-30"}},
		})
		invs := make([]frappe.PurchaseInvoice, 0, len(raws))
		for _, r := range raws {
			p, err := r.Domain()
			if err != nil {
				t.Fatal(err)
			}
			invs = append(invs, p)
		}
		want := RecurringSuppliers(invs, 2, DefaultAmountBandPct)
		if len(want) == 0 {
			t.Fatalf("no supplier billed in both 2026-08 and 2026-09; %s", seedHint)
		}
		var got RecurringSuppliersOutput
		call(t, cs, ToolListRecurringSuppliers, map[string]any{
			"company": sharmaID, "before_month": "2026-10", "lookback_months": 2, "min_occurrences": 2,
		}, &got)
		sameJSONValue(t, ToolListRecurringSuppliers, got.Suppliers, want)
	})
}
