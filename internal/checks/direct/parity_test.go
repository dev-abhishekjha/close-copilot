package direct

// Direct-versus-MCP parity (CC-702). One fake ERPNext is read twice: by
// direct.Books through the frappe client, and by agent.MCPBooks through
// the books MCP server (books.RegisterTools over a frappe client pointed at
// the same fake), served over HTTP with mcpkit.BuildHandler. Every
// BooksReader method must give deep-equal results both ways. This lives
// here because only a package outside internal/agent may import both
// paths.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
)

// Synthetic company: the seeded ID and a made-up ERPNext name.
const (
	parityCompanyID = "sharma"
	parityERP       = "Sharma Traders Pvt Ltd"
	parityOther     = "Kaveri Foods Pvt Ltd"
	parityToken     = "parity-agent-token"
	// Checksum-valid, made-up GSTINs.
	parityGSTIN    = "27AAACS1234A1Z2"
	paritySupGSTIN = "27AABCV5678B1Z8"

	bankAccount = "HDFC Current 0001 - STPL"
	// parityCashSales is the number of cash-sale journals in September:
	// with two GL Entries each, September spans more than one 500-row page.
	parityCashSales = 260
)

// doc is one ERPNext document as the REST API returns it.
type doc = map[string]any

// fakeLedgerERP is a fake ERPNext with enough of the REST API for both
// readers: list with filters (=, !=, <, <=, >, >=, between, in, and the
// child-table form [doctype, field, op, value]), order_by on any fields,
// limit_start and limit_page_length; and get by name.
type fakeLedgerERP struct {
	docs map[string][]doc // doctype -> documents
}

func (f *fakeLedgerERP) add(doctype string, d ...doc) {
	f.docs[doctype] = append(f.docs[doctype], d...)
}

func (f *fakeLedgerERP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"exc_type":"DoesNotExistError"}`))
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
	var rows []doc
	for _, d := range f.docs[doctype] {
		if parityMatch(d, filters) {
			rows = append(rows, listRow(d))
		}
	}
	sortRows(rows, q.Get("order_by"))
	start, _ := strconv.Atoi(q.Get("limit_start"))
	n, _ := strconv.Atoi(q.Get("limit_page_length"))
	if start > len(rows) {
		start = len(rows)
	}
	end := len(rows)
	if n > 0 && start+n < end {
		end = start + n
	}
	page := rows[start:end]
	if page == nil {
		page = []doc{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
}

// listRow drops child tables, as Frappe's list API does.
func listRow(d doc) doc {
	out := doc{}
	for k, v := range d {
		if _, isTable := v.([]doc); !isTable {
			out[k] = v
		}
	}
	return out
}

func sortRows(rows []doc, orderBy string) {
	var keys []string
	for part := range strings.SplitSeq(orderBy, ",") {
		if f := strings.Fields(part); len(f) > 0 {
			keys = append(keys, f[0])
		}
	}
	slices.SortStableFunc(rows, func(a, b doc) int {
		for _, k := range keys {
			if c := cmp.Compare(fmt.Sprint(a[k]), fmt.Sprint(b[k])); c != 0 {
				return c
			}
		}
		return 0
	})
}

func parityMatch(d doc, filters [][]any) bool {
	for _, flt := range filters {
		switch len(flt) {
		case 3:
			field, _ := flt[0].(string)
			op, _ := flt[1].(string)
			if !matchOp(fmt.Sprint(d[field]), op, flt[2]) {
				return false
			}
		case 4:
			// A child-table filter: some row of the table matches.
			field, _ := flt[1].(string)
			op, _ := flt[2].(string)
			rows, _ := d[childTable(flt[0])].([]doc)
			if !slices.ContainsFunc(rows, func(r doc) bool { return matchOp(fmt.Sprint(r[field]), op, flt[3]) }) {
				return false
			}
		}
	}
	return true
}

func childTable(doctype any) string {
	if doctype == "Payment Entry Reference" {
		return "references"
	}
	return ""
}

func matchOp(got, op string, want any) bool {
	switch op {
	case "=":
		return got == fmt.Sprint(want)
	case "!=":
		return got != fmt.Sprint(want)
	case "<":
		return got < fmt.Sprint(want)
	case "<=":
		return got <= fmt.Sprint(want)
	case ">":
		return got > fmt.Sprint(want)
	case ">=":
		return got >= fmt.Sprint(want)
	case "between":
		lim, _ := want.([]any)
		return len(lim) == 2 && got >= fmt.Sprint(lim[0]) && got <= fmt.Sprint(lim[1])
	case "in":
		vals, _ := want.([]any)
		return slices.ContainsFunc(vals, func(v any) bool { return fmt.Sprint(v) == got })
	}
	return false
}

// ---- the synthetic month ----

type glBuilder struct {
	seq     int
	entries []doc
}

// post adds a balanced voucher: each leg is account, debit, credit (in
// rupees text) and party.
func (g *glBuilder) post(company, date, vtype, vno, remarks string, opening bool, cancelled bool, legs ...[4]string) {
	isOpening := "No"
	if opening {
		isOpening = "Yes"
	}
	canc := 0
	if cancelled {
		canc = 1
	}
	for _, l := range legs {
		g.seq++
		e := doc{
			"name": fmt.Sprintf("ACC-GLE-2026-%05d", g.seq), "docstatus": 1, "company": company,
			"account": l[0], "debit": l[1], "credit": l[2], "posting_date": date,
			"voucher_type": vtype, "voucher_no": vno, "party_type": "", "party": l[3],
			"is_cancelled": canc, "is_opening": isOpening, "fiscal_year": fiscalYear(date),
			"against": "", "remarks": remarks,
		}
		if l[3] != "" {
			e["party_type"] = "Supplier"
			if strings.HasPrefix(l[3], "CUST") {
				e["party_type"] = "Customer"
			}
		}
		g.entries = append(g.entries, e)
	}
}

func fiscalYear(date string) string {
	d, _ := time.Parse(time.DateOnly, date)
	y := d.Year()
	if d.Month() < time.April {
		y--
	}
	return fmt.Sprintf("%d-%d", y, y+1)
}

func leg(account, debit, credit, party string) [4]string {
	return [4]string{account, debit, credit, party}
}

func account(name, rootType string, group bool) doc {
	g := 0
	if group {
		g = 1
	}
	base, _, _ := strings.Cut(name, " - ")
	return doc{"name": name, "docstatus": 0, "account_name": base, "company": parityERP, "parent_account": "",
		"is_group": g, "root_type": rootType, "account_type": "", "account_currency": "INR"}
}

func rentInvoice(name, date string, docstatus int, company string) doc {
	return doc{
		"name": name, "docstatus": docstatus, "company": company, "supplier": "SUP-RENT", "supplier_name": "Vardhan Estates",
		"bill_no": "VE/" + name, "bill_date": date[:8] + "01", "posting_date": date, "remarks": "Office rent",
		"credit_to": "Creditors - STPL", "net_total": "50000.00", "grand_total": "59000.00", "outstanding_amount": "0",
		"is_return": 0, "supplier_gstin": paritySupGSTIN, "company_gstin": parityGSTIN, "place_of_supply": "27-Maharashtra",
		"items": []doc{{"name": name + "-i1", "item_code": "RENT", "description": "Office rent", "expense_account": "Rent - STPL", "amount": "50000.00"}},
		"taxes": []doc{
			{"name": name + "-t1", "account_head": "Input Tax CGST - STPL", "tax_amount": "4500.00", "charge_type": "On Net Total",
				"rate": json.Number("9"), "add_deduct_tax": "Add", "category": "Total", "description": "CGST", "gst_tax_type": "cgst"},
			{"name": name + "-t2", "account_head": "Input Tax SGST - STPL", "tax_amount": "4500.00", "charge_type": "On Net Total",
				"rate": json.Number("9"), "add_deduct_tax": "Add", "category": "Total", "description": "SGST", "gst_tax_type": "sgst"},
		},
	}
}

// cashSale is the amount of cash sale i, in rupees text.
func cashSale(i int) string {
	p := 100000 + int64(i)*733 + int64(i%7)*25 // paise
	return fmt.Sprintf("%d.%02d", p/100, p%100)
}

func cashSaleDate(i int) string {
	return fmt.Sprintf("2026-09-%02d", 1+i%29)
}

// newParityERP builds the synthetic month: a chart of accounts, an
// opening, four months of rent bills from one supplier (a recurring
// supplier), September sales, a receipt and a payment, 260 cash-sale
// journals (more than one page of GL Entries), bank charges in July and
// August, and noise the filters must drop: a draft, a cancelled document,
// a cancelled GL pair and another company's documents.
func newParityERP(t *testing.T) (*fakeLedgerERP, *frappe.Client) {
	t.Helper()
	f := &fakeLedgerERP{docs: map[string][]doc{}}
	f.add(frappe.DocTypeAccount,
		account("Application of Funds - STPL", "Asset", true),
		account(bankAccount, "Asset", false),
		account("Debtors - STPL", "Asset", false),
		account("Input Tax CGST - STPL", "Asset", false),
		account("Input Tax SGST - STPL", "Asset", false),
		account("Creditors - STPL", "Liability", false),
		account("Output Tax IGST - STPL", "Liability", false),
		account("Capital - STPL", "Equity", false),
		account("Sales - STPL", "Income", false),
		account("Rent - STPL", "Expense", false),
		account("Bank Charges - STPL", "Expense", false),
	)

	g := &glBuilder{}
	g.post(parityERP, "2026-04-01", "Journal Entry", "ACC-JV-2026-00001", "Opening", true, false,
		leg(bankAccount, "5000000.00", "0", ""), leg("Capital - STPL", "0", "5000000.00", ""))
	for i, m := range []string{"06", "07", "08", "09"} {
		pinv := fmt.Sprintf("ACC-PINV-2026-%05d", 30+i)
		f.add(frappe.DocTypePurchaseInvoice, rentInvoice(pinv, "2026-"+m+"-05", 1, parityERP))
		g.post(parityERP, "2026-"+m+"-05", "Purchase Invoice", pinv, "Office rent", false, false,
			leg("Rent - STPL", "50000.00", "0", ""), leg("Input Tax CGST - STPL", "4500.00", "0", ""),
			leg("Input Tax SGST - STPL", "4500.00", "0", ""), leg("Creditors - STPL", "0", "59000.00", "SUP-RENT"))
	}
	for _, m := range []string{"07-31", "08-31"} {
		g.post(parityERP, "2026-"+m, "Journal Entry", "ACC-JV-BC-"+m[:2], "SMS charges", false, false,
			leg("Bank Charges - STPL", "17.70", "0", ""), leg(bankAccount, "0", "17.70", ""))
	}
	// Noise the readers must not return.
	f.add(frappe.DocTypePurchaseInvoice,
		rentInvoice("ACC-PINV-2026-00090", "2026-09-07", 0, parityERP),
		rentInvoice("ACC-PINV-2026-00091", "2026-09-08", 2, parityERP),
		rentInvoice("ACC-PINV-2026-00092", "2026-09-08", 1, parityOther),
	)
	g.post(parityERP, "2026-09-15", "Journal Entry", "ACC-JV-2026-00500", "Reversed", false, true,
		leg("Bank Charges - STPL", "999.00", "0", ""), leg(bankAccount, "0", "999.00", ""))
	g.post(parityOther, "2026-09-15", "Journal Entry", "ACC-JV-OT-00001", "Other company", false, false,
		leg(bankAccount, "10.00", "0", ""), leg("Sales - STPL", "0", "10.00", ""))

	// September: rent paid, a sale collected through the bank.
	f.add(frappe.DocTypePaymentEntry, doc{
		"name": "ACC-PAY-2026-00012", "docstatus": 1, "company": parityERP, "payment_type": "Pay",
		"party_type": "Supplier", "party": "SUP-RENT", "party_name": "Vardhan Estates",
		"paid_amount": "59000.00", "received_amount": "59000.00", "paid_from": bankAccount, "paid_to": "Creditors - STPL",
		"reference_no": "UTR2026091000123", "reference_date": "2026-09-10", "posting_date": "2026-09-10",
		"remarks": "Rent September", "unallocated_amount": "0",
		"references": []doc{{"name": "per-1", "reference_doctype": "Purchase Invoice", "reference_name": "ACC-PINV-2026-00033",
			"allocated_amount": "59000.00", "total_amount": "59000.00", "outstanding_amount": "59000.00"}},
	}, doc{
		"name": "ACC-PAY-2026-00013", "docstatus": 1, "company": parityERP, "payment_type": "Receive",
		"party_type": "Customer", "party": "CUST-TATVA", "party_name": "Tatva Retail",
		"paid_amount": "1180.00", "received_amount": "1180.00", "paid_from": "Debtors - STPL", "paid_to": bankAccount,
		"reference_no": "", "reference_date": "", "posting_date": "2026-09-12", "remarks": "Receipt", "unallocated_amount": "0",
		"references": []doc{{"name": "per-2", "reference_doctype": "Sales Invoice", "reference_name": "ACC-SINV-2026-00044",
			"allocated_amount": "1180.00", "total_amount": "1180.00", "outstanding_amount": "1180.00"}},
	}, doc{
		"name": "ACC-PAY-2026-00099", "docstatus": 0, "company": parityERP, "payment_type": "Pay",
		"paid_amount": "1.00", "received_amount": "1.00", "posting_date": "2026-09-12", "unallocated_amount": "1.00",
		"references": []doc{},
	})
	g.post(parityERP, "2026-09-10", "Payment Entry", "ACC-PAY-2026-00012", "Rent September", false, false,
		leg("Creditors - STPL", "59000.00", "0", "SUP-RENT"), leg(bankAccount, "0", "59000.00", ""))
	f.add(frappe.DocTypeSalesInvoice, doc{
		"name": "ACC-SINV-2026-00044", "docstatus": 1, "company": parityERP, "customer": "CUST-TATVA", "customer_name": "Tatva Retail",
		"posting_date": "2026-09-02", "net_total": "1000.00", "grand_total": "1180.00", "outstanding_amount": "0",
		"debit_to": "Debtors - STPL", "remarks": "",
		"items": []doc{{"name": "sii-1", "item_code": "WIDGET", "description": "Widget", "income_account": "Sales - STPL", "amount": "1000.00"}},
		"taxes": []doc{{"name": "stc-1", "account_head": "Output Tax IGST - STPL", "tax_amount": "180.00", "charge_type": "On Net Total",
			"rate": json.Number("18"), "description": "IGST", "gst_tax_type": "igst"}},
	}, doc{
		"name": "ACC-SINV-2026-00045", "docstatus": 1, "company": parityERP, "customer": "CUST-ANAND", "customer_name": "Anand Stores",
		"posting_date": "2026-09-20", "net_total": "2500.50", "grand_total": "2950.59", "outstanding_amount": "2950.59",
		"debit_to": "Debtors - STPL", "remarks": "On credit",
		"items": []doc{{"name": "sii-2", "item_code": "GADGET", "description": "Gadget", "income_account": "Sales - STPL", "amount": "2500.50"}},
		"taxes": []doc{{"name": "stc-2", "account_head": "Output Tax IGST - STPL", "tax_amount": "450.09", "charge_type": "On Net Total",
			"rate": json.Number("18"), "description": "IGST", "gst_tax_type": "igst"}},
	})
	g.post(parityERP, "2026-09-02", "Sales Invoice", "ACC-SINV-2026-00044", "", false, false,
		leg("Debtors - STPL", "1180.00", "0", "CUST-TATVA"), leg("Sales - STPL", "0", "1000.00", ""), leg("Output Tax IGST - STPL", "0", "180.00", ""))
	g.post(parityERP, "2026-09-20", "Sales Invoice", "ACC-SINV-2026-00045", "On credit", false, false,
		leg("Debtors - STPL", "2950.59", "0", "CUST-ANAND"), leg("Sales - STPL", "0", "2500.50", ""), leg("Output Tax IGST - STPL", "0", "450.09", ""))
	g.post(parityERP, "2026-09-12", "Payment Entry", "ACC-PAY-2026-00013", "Receipt", false, false,
		leg(bankAccount, "1180.00", "0", ""), leg("Debtors - STPL", "0", "1180.00", "CUST-TATVA"))
	for i := range parityCashSales {
		amt := cashSale(i)
		g.post(parityERP, cashSaleDate(i), "Journal Entry", fmt.Sprintf("ACC-JV-2026-%05d", 1000+i), "Cash sale", false, false,
			leg(bankAccount, amt, "0", ""), leg("Sales - STPL", "0", amt, ""))
	}
	f.add(frappe.DocTypeGLEntry, g.entries...)

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "testkey", config.NewSecret("testsecret"))
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

// companyResolver maps the company ID to the ERPNext name for the books
// tools.
type companyResolver map[string]string

func (c companyResolver) ERPCompany(id string) (string, error) {
	if n, ok := c[id]; ok {
		return n, nil
	}
	return "", fmt.Errorf("%w %q", books.ErrUnknownCompany, id)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serveMCP serves s at /mcp behind the agent token and returns the URL.
func serveMCP(t *testing.T, s *mcp.Server) string {
	t.Helper()
	srv := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, parityToken)}, discard()))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// booksMCP serves the real books tools over client.
func booksMCP(t *testing.T, client *frappe.Client) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-books", Version: "parity"}, nil)
	books.RegisterTools(s, books.ToolDeps{
		Client:    client,
		Companies: companyResolver{parityCompanyID: parityERP, "kaveri": parityOther},
		Logger:    discard(),
	})
	return serveMCP(t, s)
}

// registry connects an agent registry to the two servers.
func registry(t *testing.T, booksURL, evidenceURL string) *agent.Registry {
	t.Helper()
	cfg := config.Config{BooksMCPURL: booksURL, EvidenceMCPURL: evidenceURL, MCPTokenAgent: config.NewSecret(parityToken)}
	r, err := agent.NewRegistry(t.Context(), cfg, discard())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// stubEvidence is an evidence server with a read-only list_bank_lines that
// returns nothing, so the registry can start without Postgres.
func stubEvidence(t *testing.T) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "stub-evidence", Version: "parity"}, nil)
	type in struct {
		Company  string `json:"company"`
		FromDate string `json:"from_date"`
		ToDate   string `json:"to_date"`
	}
	type line struct {
		TxnID string `json:"txn_id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "list_bank_lines", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, []line, error) {
			return nil, nil, nil
		})
	return serveMCP(t, s)
}

// withoutFiscalYear clears GLEntry.FiscalYear: list_gl_entries's contract
// (CC-502, pinned by its schema golden) doesn't carry it and no check
// reads it, so the MCP reader leaves it empty. Every other field must
// match.
func withoutFiscalYear(es []frappe.GLEntry) []frappe.GLEntry {
	out := slices.Clone(es)
	for i := range out {
		out[i].FiscalYear = ""
	}
	return out
}

func TestParityBooks(t *testing.T) {
	_, client := newParityERP(t)
	dir := fakeDirectory{parityCompanyID: {ID: parityCompanyID, ERPCompany: parityERP}}
	direct := &Books{Client: client, Companies: dir}
	viaMCP := &agent.MCPBooks{Registry: registry(t, booksMCP(t, client), stubEvidence(t)), Companies: dir}
	var _ checks.BooksReader = viaMCP

	ctx := t.Context()
	sepFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sepTo := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	junFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	two := func(name string, d, m func() (any, error), check func(t *testing.T, v any)) {
		t.Run(name, func(t *testing.T) {
			dv, derr := d()
			mv, merr := m()
			if derr != nil || merr != nil {
				t.Fatalf("direct err %v, MCP err %v", derr, merr)
			}
			if !reflect.DeepEqual(dv, mv) {
				t.Errorf("direct and MCP differ\ndirect: %+v\nMCP:    %+v", dv, mv)
			}
			check(t, dv)
		})
	}

	two("TrialBalance",
		func() (any, error) { return direct.TrialBalance(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.TrialBalance(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			tb := v.(books.TB)
			if len(tb.Rows) < 8 || tb.Totals.Debit != tb.Totals.Credit || tb.Totals.Debit == 0 {
				t.Errorf("trial balance looks empty or unbalanced: %d rows, totals %+v", len(tb.Rows), tb.Totals)
			}
		})

	two("GLEntries",
		func() (any, error) {
			es, err := direct.GLEntries(ctx, parityCompanyID, sepFrom, sepTo)
			return withoutFiscalYear(es), err
		},
		func() (any, error) { return viaMCP.GLEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			es := v.([]frappe.GLEntry)
			if len(es) <= books.GLPageSize {
				t.Errorf("%d GL Entries; the month must span more than one %d-row page", len(es), books.GLPageSize)
			}
			for _, e := range es {
				if e.Company != parityERP || e.IsCancelled {
					t.Fatalf("filtered entry returned: %+v", e)
				}
			}
		})

	for _, rng := range []struct {
		name     string
		from, to time.Time
	}{{"September", sepFrom, sepTo}, {"June to September", junFrom, sepTo}} {
		two("PurchaseInvoices/"+rng.name,
			func() (any, error) { return direct.PurchaseInvoices(ctx, parityCompanyID, rng.from, rng.to) },
			func() (any, error) { return viaMCP.PurchaseInvoices(ctx, parityCompanyID, rng.from, rng.to) },
			func(t *testing.T, v any) {
				if len(v.([]frappe.PurchaseInvoice)) == 0 {
					t.Error("no purchase invoices")
				}
			})
	}

	two("SalesInvoices",
		func() (any, error) { return direct.SalesInvoices(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.SalesInvoices(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			if len(v.([]frappe.SalesInvoice)) != 2 {
				t.Errorf("sales invoices = %d, want 2", len(v.([]frappe.SalesInvoice)))
			}
		})

	two("PaymentEntries",
		func() (any, error) { return direct.PaymentEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.PaymentEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			if len(v.([]frappe.PaymentEntry)) != 2 {
				t.Errorf("payment entries = %d, want 2", len(v.([]frappe.PaymentEntry)))
			}
		})

	two("PaymentEntries/none",
		func() (any, error) {
			return direct.PaymentEntries(ctx, parityCompanyID, junFrom, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
		},
		func() (any, error) {
			return viaMCP.PaymentEntries(ctx, parityCompanyID, junFrom, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
		},
		func(t *testing.T, v any) {
			if v.([]frappe.PaymentEntry) != nil {
				t.Errorf("June payments = %v, want none", v)
			}
		})

	two("AccountHistory",
		func() (any, error) {
			return direct.AccountHistory(ctx, parityCompanyID, "Bank Charges - STPL", "2026-09", 4)
		},
		func() (any, error) {
			return viaMCP.AccountHistory(ctx, parityCompanyID, "Bank Charges - STPL", "2026-09", 4)
		},
		func(t *testing.T, v any) {
			h := v.([]books.MonthTotal)
			if len(h) != 4 || h[1].Debit != 1770 || h[2].Debit != 1770 {
				t.Errorf("history = %+v, want 17.70 in July and August", h)
			}
		})

	two("RecurringSuppliers",
		func() (any, error) { return direct.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 3, 20) },
		func() (any, error) { return viaMCP.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 3, 20) },
		func(t *testing.T, v any) {
			rs := v.([]checks.RecurringSupplier)
			if len(rs) != 1 || rs[0].Supplier != "SUP-RENT" || rs[0].MedianAmount != 5900000 {
				t.Errorf("recurring = %+v, want SUP-RENT at 59,000.00", rs)
			}
		})

	two("RecurringSuppliers/none",
		func() (any, error) { return direct.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 4, 20) },
		func() (any, error) { return viaMCP.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 4, 20) },
		func(t *testing.T, v any) {
			if v.([]checks.RecurringSupplier) != nil {
				t.Errorf("recurring = %v, want none", v)
			}
		})
}
