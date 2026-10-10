package books

// A fake ERPNext for the CC-502 tool tests. Unlike listServer in
// reports_test.go it evaluates the list filters, sort order and paging the
// way Frappe does, so the tests can show that drafts, cancelled documents,
// other companies and out-of-range dates never reach the tools' output,
// and that keyset paging walks the ledger exactly once.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
)

type doc = map[string]any

// fakeRequest is one request the fake served.
type fakeRequest struct {
	method  string
	doctype string
	name    string // set for a single-document GET
	fields  []string
	filters []any // decoded with UseNumber
	orderBy string
	start   int
	length  int
}

type fakeERP struct {
	t    *testing.T
	mu   sync.Mutex
	docs map[string][]doc // by DocType, each with its child tables
	reqs []fakeRequest
	// delay, when set, holds every response this long.
	delay time.Duration
	// fail, when set, may answer a request with status and body instead.
	fail func(r *http.Request) (status int, body string, ok bool)
}

// childTables maps a child DocType used in a 4-element filter to the
// parent's table field.
var childTables = map[string]string{
	"Payment Entry Reference": "references",
}

func newFakeERP(t *testing.T) *fakeERP {
	return &fakeERP{t: t, docs: map[string][]doc{}}
}

func (f *fakeERP) add(doctype string, d ...doc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[doctype] = append(f.docs[doctype], d...)
}

// requests returns a copy of the requests so far.
func (f *fakeERP) requests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// lists returns the list requests for doctype.
func (f *fakeERP) lists(doctype string) []fakeRequest {
	var out []fakeRequest
	for _, r := range f.requests() {
		if r.doctype == doctype && r.name == "" {
			out = append(out, r)
		}
	}
	return out
}

// client starts the fake on httptest and returns a bot-key client for it.
func (f *fakeERP) client() *frappe.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "botkey", config.NewSecret("botsecret"))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fakeERP) serve(w http.ResponseWriter, r *http.Request) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.fail != nil {
		if status, body, ok := f.fail(r); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/resource/")
	if !ok || r.Method != http.MethodGet {
		f.t.Errorf("fake ERPNext: unexpected %s %s (the books tools only read)", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	doctype, name, _ := strings.Cut(rest, "/")
	w.Header().Set("Content-Type", "application/json")
	if name != "" {
		f.record(fakeRequest{method: r.Method, doctype: doctype, name: name})
		f.get(w, doctype, name)
		return
	}
	req, err := parseList(r.URL.Query())
	if err != nil {
		f.t.Errorf("fake ERPNext: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.method, req.doctype = r.Method, doctype
	f.record(req)
	f.list(w, req)
}

func (f *fakeERP) record(r fakeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
}

func parseList(q url.Values) (fakeRequest, error) {
	var r fakeRequest
	if s := q.Get("fields"); s != "" {
		if err := json.Unmarshal([]byte(s), &r.fields); err != nil {
			return r, fmt.Errorf("fields %q: %w", s, err)
		}
	}
	if s := q.Get("filters"); s != "" {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&r.filters); err != nil {
			return r, fmt.Errorf("filters %q: %w", s, err)
		}
	}
	r.orderBy = q.Get("order_by")
	var err error
	if r.start, err = strconv.Atoi(q.Get("limit_start")); err != nil {
		return r, fmt.Errorf("limit_start: %w", err)
	}
	if r.length, err = strconv.Atoi(q.Get("limit_page_length")); err != nil {
		return r, fmt.Errorf("limit_page_length: %w", err)
	}
	return r, nil
}

func (f *fakeERP) get(w http.ResponseWriter, doctype, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.docs[doctype] {
		if d["name"] == name {
			writeJSON(f.t, w, map[string]any{"data": d})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	writeJSON(f.t, w, map[string]any{"exc_type": "DoesNotExistError"})
}

func (f *fakeERP) list(w http.ResponseWriter, req fakeRequest) {
	f.mu.Lock()
	var rows []doc
	for _, d := range f.docs[req.doctype] {
		if matchAll(f.t, d, req.filters) {
			rows = append(rows, d)
		}
	}
	f.mu.Unlock()
	sortRows(rows, req.orderBy)
	if req.start >= len(rows) {
		rows = nil
	} else {
		rows = rows[req.start:min(len(rows), req.start+req.length)]
	}
	out := make([]doc, 0, len(rows))
	for _, d := range rows {
		p := doc{}
		for _, fl := range req.fields {
			if v, ok := d[fl]; ok {
				p[fl] = v
			}
		}
		out = append(out, p)
	}
	writeJSON(f.t, w, map[string]any{"data": out})
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		t.Errorf("fake ERPNext: encode: %v", err)
	}
	_, _ = w.Write(buf.Bytes())
}

func matchAll(t *testing.T, d doc, filters []any) bool {
	for _, raw := range filters {
		f, ok := raw.([]any)
		if !ok {
			t.Errorf("fake ERPNext: filter %v is not a list", raw)
			return false
		}
		switch len(f) {
		case 3:
			if !matchOne(t, d, f[0].(string), f[1].(string), f[2]) {
				return false
			}
		case 4:
			table, ok := childTables[f[0].(string)]
			if !ok {
				t.Errorf("fake ERPNext: no child table for %v", f[0])
				return false
			}
			rows, _ := d[table].([]doc)
			hit := false
			for _, row := range rows {
				if matchOne(t, row, f[1].(string), f[2].(string), f[3]) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		default:
			t.Errorf("fake ERPNext: filter %v has %d elements", f, len(f))
			return false
		}
	}
	return true
}

func matchOne(t *testing.T, d doc, field, op string, val any) bool {
	got := d[field]
	switch op {
	case "=":
		return compare(got, val) == 0
	case "!=":
		return compare(got, val) != 0
	case ">":
		return compare(got, val) > 0
	case "<":
		return compare(got, val) < 0
	case ">=":
		return compare(got, val) >= 0
	case "<=":
		return compare(got, val) <= 0
	case "between":
		b := val.([]any)
		return compare(got, b[0]) >= 0 && compare(got, b[1]) <= 0
	case "in":
		return slices.ContainsFunc(val.([]any), func(v any) bool { return compare(got, v) == 0 })
	}
	t.Errorf("fake ERPNext: operator %q not supported", op)
	return false
}

// compare orders two filter operands: as integers when both are, as text
// otherwise (Frappe compares dates and names as text too).
func compare(a, b any) int {
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	ai, errA := strconv.ParseInt(as, 10, 64)
	bi, errB := strconv.ParseInt(bs, 10, 64)
	if errA == nil && errB == nil {
		return cmp.Compare(ai, bi)
	}
	return strings.Compare(as, bs)
}

func sortRows(rows []doc, orderBy string) {
	if orderBy == "" {
		return
	}
	type key struct {
		field string
		desc  bool
	}
	var keys []key
	for part := range strings.SplitSeq(orderBy, ",") {
		fs := strings.Fields(part)
		keys = append(keys, key{field: fs[0], desc: strings.EqualFold(fs[1], "desc")})
	}
	slices.SortStableFunc(rows, func(a, b doc) int {
		for _, k := range keys {
			c := compare(a[k.field], b[k.field])
			if k.desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
}

// ---- document builders (amounts in rupees, as ERPNext sends them) ----

func glDoc(name, company, account, debit, credit, date string, docstatus, cancelled int) doc {
	return doc{
		"name": name, "docstatus": docstatus, "company": company, "account": account,
		"debit": json.Number(debit), "credit": json.Number(credit), "posting_date": date,
		"voucher_type": "Journal Entry", "voucher_no": "JV-" + name, "party_type": "", "party": "",
		"is_cancelled": cancelled, "is_opening": "No", "fiscal_year": "2026-2027",
		"against": "", "remarks": "",
	}
}

func accountDoc(name, company, rootType string) doc {
	return doc{
		"name": name, "docstatus": 0, "account_name": strings.TrimSuffix(name, " - TT"), "company": company,
		"parent_account": "", "is_group": 0, "root_type": rootType, "account_type": "", "account_currency": "INR",
	}
}

func taxRow(name, head, rate, amount, gstType string) doc {
	return doc{
		"name": name, "account_head": head, "tax_amount": json.Number(amount), "charge_type": "Actual",
		"rate": json.Number(rate), "add_deduct_tax": "Add", "category": "Total", "description": head,
		"gst_tax_type": gstType,
	}
}

func purchaseDoc(name, company, supplier, date string, docstatus int, net, grand string, taxes []doc) doc {
	return doc{
		"name": name, "docstatus": docstatus, "company": company, "supplier": supplier,
		"supplier_name": supplier + " Pvt Ltd", "bill_no": "B-" + name, "bill_date": "", "posting_date": date,
		"remarks": "", "credit_to": "Creditors - TT", "net_total": json.Number(net),
		"grand_total": json.Number(grand), "outstanding_amount": json.Number(grand), "is_return": 0,
		"supplier_gstin": "27AAAAA0000A1Z5", "company_gstin": "27BBBBB0000B1Z5", "place_of_supply": "27-Maharashtra",
		"items": []doc{{
			"name": name + "-item", "item_code": "SERVICE", "description": "Monthly service",
			"expense_account": "Rent - TT", "amount": json.Number(net),
		}},
		"taxes": taxes,
	}
}

func salesDoc(name, company, customer, date string, docstatus int, grand, outstanding string) doc {
	return doc{
		"name": name, "docstatus": docstatus, "company": company, "customer": customer,
		"customer_name": customer, "posting_date": date, "net_total": json.Number(grand),
		"grand_total": json.Number(grand), "outstanding_amount": json.Number(outstanding),
		"debit_to": "Debtors - TT", "remarks": "",
		"items": []doc{{
			"name": name + "-item", "item_code": "GOODS", "description": "Goods",
			"income_account": "Sales - TT", "amount": json.Number(grand),
		}},
		"taxes": []doc{},
	}
}

func paymentDoc(name, company, typ, party, date string, docstatus int, amount, paidTo string, refs ...doc) doc {
	partyType := "Supplier"
	if typ == "Receive" {
		partyType = "Customer"
	}
	return doc{
		"name": name, "docstatus": docstatus, "company": company, "payment_type": typ,
		"party_type": partyType, "party": party, "party_name": party,
		"paid_amount": json.Number(amount), "received_amount": json.Number(amount),
		"paid_from": "Bank - TT", "paid_to": paidTo, "reference_no": "UTR" + name,
		"reference_date": date, "posting_date": date, "remarks": "", "unallocated_amount": json.Number("0"),
		"references": refs,
	}
}

func refRow(name, doctype, invoice, amount string) doc {
	return doc{
		"name": name, "reference_doctype": doctype, "reference_name": invoice,
		"allocated_amount": json.Number(amount), "total_amount": json.Number(amount),
		"outstanding_amount": json.Number(amount),
	}
}
