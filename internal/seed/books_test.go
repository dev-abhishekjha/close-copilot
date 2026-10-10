package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// bookDocTypes is the order the golden file lists DocTypes in.
var bookDocTypes = []string{DocSalesInvoice, DocPurchaseInvoice, DocPaymentEntry, DocJournalEntry}

// bookSeries are the fake's naming series, as the dev site names them.
var bookSeries = map[string]string{
	DocSalesInvoice:    "SINV-26-",
	DocPurchaseInvoice: "PINV-26-",
	DocPaymentEntry:    "ACC-PAY-2026-",
	DocJournalEntry:    "ACC-JV-2026-",
}

// fakeBooks is an in-memory Frappe for the book writer: list, get and
// insert on /api/resource for the four voucher DocTypes, and
// frappe.client.submit. It names documents from per-DocType series, checks
// links to parties, accounts and referenced invoices, computes the totals
// WriteBooks checks after submit, and logs every write.
type fakeBooks struct {
	mu      sync.Mutex
	rep     BootstrapReport
	docs    map[string]map[string]doc // doctype -> name -> document
	seq     map[string]int
	writes  []bookWrite
	inserts map[string]int // ExtID -> inserts
	// failInsert and failSubmit make the insert or submit of a document
	// with this ExtID fail with the message.
	failInsert map[string]string
	failSubmit map[string]string
}

type bookWrite struct {
	doctype string
	line    string
}

func newFakeBooks(rep BootstrapReport) *fakeBooks {
	f := &fakeBooks{
		rep:        rep,
		docs:       map[string]map[string]doc{},
		seq:        map[string]int{},
		inserts:    map[string]int{},
		failInsert: map[string]string{},
		failSubmit: map[string]string{},
	}
	for _, dt := range bookDocTypes {
		f.docs[dt] = map[string]doc{}
	}
	return f
}

func (f *fakeBooks) client(t *testing.T) *frappe.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL, ERPSite: "erp.localhost"}, "seed-key", config.NewSecret("seed-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeBooks) nextName(dt string) string {
	f.seq[dt]++
	return fmt.Sprintf("%s%05d", bookSeries[dt], f.seq[dt])
}

// add stores a document as if an earlier run had posted it.
func (f *fakeBooks) add(dt string, ev Event, docstatus int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := f.nextName(dt)
	f.docs[dt][name] = doc{
		"name": name, "doctype": dt, "company": f.rep.Company, "posting_date": ev.Date,
		ExtIDField: ev.ExtID, "docstatus": docstatus, totalField[dt]: json.Number(ev.Gross.Rupees()),
	}
	return name
}

func (f *fakeBooks) log() []bookWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// state is every stored document as "doctype name ext docstatus".
func (f *fakeBooks) state() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, dt := range bookDocTypes {
		for name, d := range f.docs[dt] {
			out = append(out, fmt.Sprintf("%s %s %s %s", dt, name, str(d[ExtIDField]), str(d["docstatus"])))
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeBooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/method/frappe.client.submit" && r.Method == http.MethodPost {
		f.submit(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/resource/")
	if !ok {
		fail(w, http.StatusNotFound, "PageDoesNotExistError", "no such path")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	dt, _ := url.PathUnescape(parts[0])
	if _, known := f.docs[dt]; !known {
		fail(w, http.StatusNotFound, "DoesNotExistError", "the fake has no "+dt)
		return
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		f.list(w, r, dt)
	case r.Method == http.MethodGet:
		name, _ := url.PathUnescape(parts[1])
		d, ok := f.docs[dt][name]
		if !ok {
			fail(w, http.StatusNotFound, "DoesNotExistError", dt+" "+name+" not found")
			return
		}
		reply(w, d)
	case r.Method == http.MethodPost && len(parts) == 1:
		f.insert(w, r, dt)
	default:
		fail(w, http.StatusMethodNotAllowed, "", "method not allowed")
	}
}

func (f *fakeBooks) list(w http.ResponseWriter, r *http.Request, dt string) {
	q := r.URL.Query()
	var fields []string
	if err := json.Unmarshal([]byte(q.Get("fields")), &fields); err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", "fields: "+err.Error())
		return
	}
	dec := json.NewDecoder(strings.NewReader(q.Get("filters")))
	dec.UseNumber()
	var filters [][]any
	if err := dec.Decode(&filters); err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", "filters: "+err.Error())
		return
	}
	start, _ := strconv.Atoi(q.Get("limit_start"))
	length, _ := strconv.Atoi(q.Get("limit_page_length"))
	names := make([]string, 0, len(f.docs[dt]))
	for n := range f.docs[dt] {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := []doc{}
	for _, n := range names {
		d := f.docs[dt][n]
		ok, err := bookFilters(d, filters)
		if err != nil {
			fail(w, http.StatusBadRequest, "ValidationError", err.Error())
			return
		}
		if !ok {
			continue
		}
		row := doc{}
		for _, fl := range fields {
			row[fl] = d[fl]
		}
		rows = append(rows, row)
	}
	rows = rows[min(start, len(rows)):]
	if length > 0 && length < len(rows) {
		rows = rows[:length]
	}
	reply(w, rows)
}

// bookFilters applies the list filters WriteBooks sends: =, between, is
// set and in.
func bookFilters(d doc, filters [][]any) (bool, error) {
	for _, flt := range filters {
		if len(flt) != 3 {
			return false, fmt.Errorf("filter %v", flt)
		}
		field, _ := flt[0].(string)
		op, _ := flt[1].(string)
		got := str(d[field])
		switch op {
		case "=":
			if got != str(flt[2]) {
				return false, nil
			}
		case "between":
			r, _ := flt[2].([]any)
			if len(r) != 2 || got < str(r[0]) || got > str(r[1]) {
				return false, nil
			}
		case "is":
			if str(flt[2]) != "set" {
				return false, fmt.Errorf("is %v", flt[2])
			}
			if got == "" {
				return false, nil
			}
		case "in":
			vals, _ := flt[2].([]any)
			if !slices.ContainsFunc(vals, func(v any) bool { return str(v) == got }) {
				return false, nil
			}
		default:
			return false, fmt.Errorf("operator %q", op)
		}
	}
	return true, nil
}

func rupees(v any) (money.Paise, error) { return paiseField(v) }

func rows(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// validate checks what ERPNext would refuse and returns the total the
// submitted document reports.
func (f *fakeBooks) validate(dt string, d doc) (money.Paise, error) {
	if str(d["company"]) != f.rep.Company {
		return 0, fmt.Errorf("company %v", d["company"])
	}
	if str(d[ExtIDField]) == "" || str(d["posting_date"]) == "" {
		return 0, errors.New("no ext id or posting date")
	}
	accounts := map[string]bool{}
	for _, a := range f.rep.Accounts {
		accounts[a] = true
	}
	for _, g := range []GSTAccounts{f.rep.InputGST, f.rep.OutputGST} {
		accounts[g.CGST], accounts[g.SGST], accounts[g.IGST] = true, true, true
	}
	needAccount := func(fields ...any) error {
		for _, a := range fields {
			if !accounts[str(a)] {
				return fmt.Errorf("no account %q", a)
			}
		}
		return nil
	}
	invoiceTotal := func(itemAccount string) (money.Paise, error) {
		var total money.Paise
		for _, it := range rows(d["items"]) {
			rate, err := rupees(it["rate"])
			if err != nil || str(it["qty"]) != "1" {
				return 0, fmt.Errorf("item rate %v qty %v", it["rate"], it["qty"])
			}
			if err := needAccount(it[itemAccount]); err != nil {
				return 0, err
			}
			total += rate
		}
		for _, tx := range rows(d["taxes"]) {
			amt, err := rupees(tx["tax_amount"])
			if err != nil || tx["charge_type"] != "Actual" {
				return 0, fmt.Errorf("tax row %v", tx)
			}
			if err := needAccount(tx["account_head"]); err != nil {
				return 0, err
			}
			total += amt
		}
		return total, nil
	}
	switch dt {
	case DocSalesInvoice:
		if !slices.Contains(f.rep.Customers, str(d["customer"])) {
			return 0, fmt.Errorf("no customer %v", d["customer"])
		}
		if err := needAccount(d["debit_to"]); err != nil {
			return 0, err
		}
		return invoiceTotal("income_account")
	case DocPurchaseInvoice:
		if !slices.Contains(mapValues(f.rep.Suppliers), str(d["supplier"])) {
			return 0, fmt.Errorf("no supplier %v", d["supplier"])
		}
		if err := needAccount(d["credit_to"]); err != nil {
			return 0, err
		}
		return invoiceTotal("expense_account")
	case DocPaymentEntry:
		if err := needAccount(d["paid_from"], d["paid_to"]); err != nil {
			return 0, err
		}
		paid, err := rupees(d["paid_amount"])
		if err != nil {
			return 0, err
		}
		for _, ref := range rows(d["references"]) {
			inv, ok := f.docs[str(ref["reference_doctype"])][str(ref["reference_name"])]
			if !ok || str(inv["docstatus"]) != "1" {
				return 0, fmt.Errorf("reference %v %v is not a submitted document", ref["reference_doctype"], ref["reference_name"])
			}
		}
		return paid, nil
	case DocJournalEntry:
		var dr, cr money.Paise
		for _, l := range rows(d["accounts"]) {
			if err := needAccount(l["account"]); err != nil {
				return 0, err
			}
			a, err1 := rupees(l["debit_in_account_currency"])
			b, err2 := rupees(l["credit_in_account_currency"])
			if err := errors.Join(err1, err2); err != nil {
				return 0, err
			}
			dr += a
			cr += b
		}
		if dr != cr {
			return 0, fmt.Errorf("debits %s, credits %s", dr.Rupees(), cr.Rupees())
		}
		return dr, nil
	}
	return 0, fmt.Errorf("doctype %s", dt)
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func (f *fakeBooks) insert(w http.ResponseWriter, r *http.Request, dt string) {
	body, err := decodeBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", err.Error())
		return
	}
	b, _ := json.Marshal(body)
	f.writes = append(f.writes, bookWrite{dt, "INSERT " + string(b)})
	ext := str(body[ExtIDField])
	f.inserts[ext]++
	if msg, ok := f.failInsert[ext]; ok {
		fail(w, http.StatusExpectationFailed, "ValidationError", msg)
		return
	}
	total, err := f.validate(dt, body)
	if err != nil {
		fail(w, http.StatusExpectationFailed, "ValidationError", err.Error())
		return
	}
	name := f.nextName(dt)
	d := doc{}
	for k, v := range body {
		d[k] = v
	}
	d["name"] = name
	d["doctype"] = dt
	d["docstatus"] = 0
	d[totalField[dt]] = json.Number(total.Rupees())
	f.docs[dt][name] = d
	reply(w, d)
}

func (f *fakeBooks) submit(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", err.Error())
		return
	}
	sent, _ := body["doc"].(map[string]any)
	dt, name := str(sent["doctype"]), str(sent["name"])
	d, ok := f.docs[dt][name]
	if !ok {
		fail(w, http.StatusNotFound, "DoesNotExistError", dt+" "+name+" not found")
		return
	}
	f.writes = append(f.writes, bookWrite{dt, "SUBMIT " + name})
	if msg, ok := f.failSubmit[str(d[ExtIDField])]; ok {
		fail(w, http.StatusExpectationFailed, "ValidationError", msg)
		return
	}
	if str(d["docstatus"]) != "0" {
		fail(w, http.StatusExpectationFailed, "ValidationError", "only a draft can be submitted")
		return
	}
	d["docstatus"] = 1
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"message": d})
}

// sharmaBooks returns sharma's profile, a Bootstrap report from the
// bootstrap fake, and the --small world of month.
func sharmaBooks(t *testing.T, month string) (Profile, BootstrapReport, World) {
	t.Helper()
	p := loadSharma(t)
	rep, err := Bootstrap(context.Background(), newSiteFake(p).client(t), p)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return p, rep, mustGenerate(t, p, month, Options{Small: true})
}

func writeBooks(t *testing.T, f *fakeBooks, rep BootstrapReport, w World, opt BooksOptions) BooksResult {
	t.Helper()
	res, err := WriteBooks(context.Background(), f.client(t), rep, w, opt)
	if err != nil {
		t.Fatalf("WriteBooks: %v", err)
	}
	return res
}

// inserted returns the body of every insert by ExtID.
func inserted(t *testing.T, f *fakeBooks) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, wr := range f.log() {
		js, ok := strings.CutPrefix(wr.line, "INSERT ")
		if !ok {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(js))
		dec.UseNumber()
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		d["_doctype"] = wr.doctype
		out[str(d[ExtIDField])] = d
	}
	return out
}

func mustPaise(t *testing.T, v any) money.Paise {
	t.Helper()
	p, err := rupees(v)
	if err != nil {
		t.Fatalf("amount %v: %v", v, err)
	}
	return p
}

func countsOf(r BooksResult) map[string]BookCounts {
	out := map[string]BookCounts{}
	for dt, c := range r.Counts {
		out[dt] = *c
	}
	return out
}

func mapsEqual(a, b ERPMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

const (
	goldenBooksWrites = "testdata/books/writes-sharma-2026-09-small.txt"
	goldenBooksMap    = "testdata/books/erp_map-sharma-2026-09-small.json"
)

func checkBooksGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; run go test ./internal/seed -run TestBooksGolden -update and review the diff", path)
	}
}

// writesByDocType renders the write log grouped by DocType, each in the
// order it was sent (workers run DocTypes in parallel, so only the order
// within a DocType is fixed).
func writesByDocType(log []bookWrite) []byte {
	var b bytes.Buffer
	for _, dt := range bookDocTypes {
		fmt.Fprintf(&b, "== %s\n", dt)
		for _, wr := range log {
			if wr.doctype == dt {
				b.WriteString(wr.line)
				b.WriteByte('\n')
			}
		}
	}
	return b.Bytes()
}

func TestBooksGolden(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	var logs [][]byte
	var maps [][]byte
	for range 2 {
		f := newFakeBooks(rep)
		res := writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
		logs = append(logs, writesByDocType(f.log()))
		m, err := res.Map.JSON()
		if err != nil {
			t.Fatal(err)
		}
		maps = append(maps, m)
	}
	if !bytes.Equal(logs[0], logs[1]) || !bytes.Equal(maps[0], maps[1]) {
		t.Error("two runs on fresh sites sent different writes or got different maps")
	}
	checkBooksGolden(t, goldenBooksWrites, logs[0])
	checkBooksGolden(t, goldenBooksMap, maps[0])
}

func TestBookDocType(t *testing.T) {
	want := map[string]string{
		EventSale:                DocSalesInvoice,
		EventReceipt:             DocPaymentEntry,
		EventGatewayReceipt:      DocPaymentEntry,
		EventGatewaySettlement:   "",
		EventPurchase:            DocPurchaseInvoice,
		EventVendorPayment:       DocPaymentEntry,
		EventPayroll:             DocJournalEntry,
		EventBankCharge:          DocJournalEntry,
		EventInterest:            DocJournalEntry,
		EventPrepaidAmortisation: DocJournalEntry,
	}
	for _, k := range EventKinds {
		got, ok := want[k]
		if !ok {
			t.Errorf("kind %q has no expectation", k)
			continue
		}
		if BookDocType(k) != got {
			t.Errorf("BookDocType(%q) = %q, want %q", k, BookDocType(k), got)
		}
	}
	if BookDocType("bogus") != "" {
		t.Error("unknown kind maps to a DocType")
	}
}

// TestBooksMapsEveryKind checks each kind's DocType and fields on
// September (sales, bills, an annual bill, payments, interest) and October
// (the laptop).
func TestBooksMapsEveryKind(t *testing.T) {
	kinds := map[string]bool{}
	for _, month := range []string{"2026-09", "2026-10"} {
		p, rep, w := sharmaBooks(t, month)
		f := newFakeBooks(rep)
		res := writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
		ins := inserted(t, f)
		suppliers := map[string]Supplier{}
		for _, s := range p.Suppliers {
			suppliers[s.ID] = s
		}
		bank := rep.BankAccount
		for _, ev := range w.Events {
			kinds[ev.Kind] = true
			d, posted := ins[ev.ExtID]
			if ev.Kind == EventGatewaySettlement {
				if posted {
					t.Errorf("%s: a gateway settlement was posted", ev.ExtID)
				}
				if _, ok := res.Map[ev.ExtID]; ok {
					t.Errorf("%s: a gateway settlement is in the map", ev.ExtID)
				}
				continue
			}
			if !posted {
				t.Errorf("%s (%s) was not posted", ev.ExtID, ev.Kind)
				continue
			}
			dt := str(d["_doctype"])
			if dt != BookDocType(ev.Kind) || res.Map[ev.ExtID].DocType != dt {
				t.Errorf("%s (%s) posted as %s, mapped as %s", ev.ExtID, ev.Kind, dt, res.Map[ev.ExtID].DocType)
			}
			if d["posting_date"] != ev.Date || str(d["set_posting_time"]) != "1" || d["company"] != rep.Company {
				t.Errorf("%s: posting_date %v set_posting_time %v company %v", ev.ExtID, d["posting_date"], d["set_posting_time"], d["company"])
			}
			ref := func(i int) string { return res.Map[ev.Refs[i]].Name }
			switch ev.Kind {
			case EventSale:
				items := rows(d["items"])
				if d["customer"] != ev.Party || d["debit_to"] != rep.Accounts[AccountDebtors] || d["remarks"] != ev.Narration ||
					len(items) != 1 || items[0]["item_code"] != ItemSales || items[0]["income_account"] != rep.Accounts[AccountSales] ||
					mustPaise(t, items[0]["rate"]) != ev.Taxable || str(items[0]["qty"]) != "1" {
					t.Errorf("%s: sales invoice %v", ev.ExtID, d)
				}
				due := mustDate(t, ev.Date).AddDate(0, 0, 15).Format(dateLayout)
				if d["due_date"] != due {
					t.Errorf("%s: due %v, want %s", ev.ExtID, d["due_date"], due)
				}
			case EventPurchase:
				items := rows(d["items"])
				s := suppliers[ev.Meta.SupplierID]
				wantItem := ItemFor(EventPurchase, s.Kind, ev.Account == AccountOfficeEquipment)
				if d["supplier"] != s.Name || d["bill_no"] != ev.InvoiceNo || d["bill_date"] != ev.Date ||
					d["credit_to"] != rep.Accounts[AccountCreditors] || d["remarks"] != ev.Narration || len(items) != 1 ||
					items[0]["item_code"] != wantItem || items[0]["expense_account"] != rep.Accounts[ev.Account] ||
					mustPaise(t, items[0]["rate"]) != ev.Taxable {
					t.Errorf("%s: purchase invoice %v", ev.ExtID, d)
				}
				desc := str(items[0]["description"])
				if !strings.Contains(desc, ev.Meta.Description) {
					t.Errorf("%s: description %q lacks %q", ev.ExtID, desc, ev.Meta.Description)
				}
				if ev.Meta.ServiceFrom != "" && !strings.Contains(desc, "Service period") {
					t.Errorf("%s: description %q has no service period", ev.ExtID, desc)
				}
				if ev.Account == AccountOfficeEquipment && wantItem != ItemLaptop {
					t.Errorf("%s: the laptop bill uses %s", ev.ExtID, wantItem)
				}
			case EventReceipt, EventGatewayReceipt:
				to := bank
				if ev.Kind == EventGatewayReceipt {
					to = rep.Accounts[AccountPaymentGatewayClearing]
				}
				refs := rows(d["references"])
				if d["payment_type"] != "Receive" || d["party_type"] != PartyCustomer || d["party"] != ev.Party ||
					d["paid_from"] != rep.Accounts[AccountDebtors] || d["paid_to"] != to ||
					mustPaise(t, d["paid_amount"]) != ev.Gross || d["reference_no"] != bankRef(ev) || d["reference_date"] != ev.Date ||
					d["remarks"] != ev.Narration || len(refs) != 1 || refs[0]["reference_doctype"] != DocSalesInvoice ||
					refs[0]["reference_name"] != ref(0) || mustPaise(t, refs[0]["allocated_amount"]) != ev.Gross {
					t.Errorf("%s: receipt %v", ev.ExtID, d)
				}
			case EventVendorPayment:
				refs := rows(d["references"])
				if d["payment_type"] != "Pay" || d["party_type"] != PartySupplier || d["party"] != ev.Party ||
					d["paid_from"] != bank || d["paid_to"] != rep.Accounts[AccountCreditors] ||
					mustPaise(t, d["paid_amount"]) != ev.Gross || d["reference_no"] != ev.BankRef || d["reference_date"] != ev.Date ||
					len(refs) != 1 || refs[0]["reference_doctype"] != DocPurchaseInvoice || refs[0]["reference_name"] != ref(0) ||
					mustPaise(t, refs[0]["allocated_amount"]) != ev.Gross {
					t.Errorf("%s: vendor payment %v", ev.ExtID, d)
				}
			default:
				lines := map[string][2]money.Paise{}
				for _, l := range rows(d["accounts"]) {
					lines[str(l["account"])] = [2]money.Paise{mustPaise(t, l["debit_in_account_currency"]), mustPaise(t, l["credit_in_account_currency"])}
				}
				var want map[string][2]money.Paise
				switch ev.Kind {
				case EventPayroll:
					want = map[string][2]money.Paise{rep.Accounts[AccountSalaries]: {ev.Gross, 0}, bank: {0, ev.Gross}}
				case EventBankCharge:
					want = map[string][2]money.Paise{
						rep.Accounts[AccountBankCharges]: {ev.Taxable, 0}, rep.InputGST.CGST: {ev.CGST, 0},
						rep.InputGST.SGST: {ev.SGST, 0}, bank: {0, ev.Gross},
					}
				case EventInterest:
					want = map[string][2]money.Paise{bank: {ev.Gross, 0}, rep.Accounts[AccountInterestIncome]: {0, ev.Gross}}
				case EventPrepaidAmortisation:
					want = map[string][2]money.Paise{rep.Accounts[ev.Account]: {ev.Gross, 0}, rep.Accounts[AccountPrepaidExpenses]: {0, ev.Gross}}
				}
				if fmt.Sprint(lines) != fmt.Sprint(want) {
					t.Errorf("%s (%s): lines %v, want %v", ev.ExtID, ev.Kind, lines, want)
				}
				if d["voucher_type"] != "Journal Entry" || d["user_remark"] != ev.Narration {
					t.Errorf("%s: voucher_type %v, user_remark %v", ev.ExtID, d["voucher_type"], d["user_remark"])
				}
				if _, hits := want[bank]; hits {
					if d["cheque_no"] != bankRef(ev) || d["cheque_date"] != ev.Date {
						t.Errorf("%s: cheque_no %v, cheque_date %v", ev.ExtID, d["cheque_no"], d["cheque_date"])
					}
				} else if _, ok := d["cheque_no"]; ok {
					t.Errorf("%s: a journal that doesn't hit the bank has cheque_no %v", ev.ExtID, d["cheque_no"])
				}
			}
		}
	}
	for _, k := range EventKinds {
		if !kinds[k] {
			t.Errorf("no %s event in the test months", k)
		}
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestBooksInvoiceTaxes checks that every invoice with GST carries the
// rate's Item Tax Template and Actual rows equal to the event's taxes, and
// that an unregistered supplier's bill has neither.
func TestBooksInvoiceTaxes(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	f := newFakeBooks(rep)
	writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
	ins := inserted(t, f)
	unregistered, igst := 0, 0
	for _, ev := range w.Events {
		if ev.Kind != EventSale && ev.Kind != EventPurchase {
			continue
		}
		d := ins[ev.ExtID]
		item := rows(d["items"])[0]
		heads := rep.OutputGST
		if ev.Kind == EventPurchase {
			heads = rep.InputGST
		}
		got := map[string]money.Paise{}
		for _, tx := range rows(d["taxes"]) {
			if tx["charge_type"] != "Actual" {
				t.Errorf("%s: charge_type %v", ev.ExtID, tx["charge_type"])
			}
			got[str(tx["account_head"])] += mustPaise(t, tx["tax_amount"])
			if ev.Kind == EventPurchase && (tx["category"] != "Total" || tx["add_deduct_tax"] != "Add") {
				t.Errorf("%s: purchase tax row %v", ev.ExtID, tx)
			}
		}
		want := map[string]money.Paise{}
		for head, amt := range map[string]money.Paise{heads.IGST: ev.IGST, heads.CGST: ev.CGST, heads.SGST: ev.SGST} {
			if amt != 0 {
				want[head] = amt
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: tax rows %v, want %v", ev.ExtID, got, want)
		}
		if ev.IGST != 0 {
			igst++
		}
		if ev.Kind == EventPurchase && ev.Meta.SupplierGSTIN == "" {
			unregistered++
			if len(rows(d["taxes"])) != 0 {
				t.Errorf("%s: unregistered supplier's bill has tax rows", ev.ExtID)
			}
			if _, ok := item["item_tax_template"]; ok {
				t.Errorf("%s: unregistered supplier's bill has an item tax template", ev.ExtID)
			}
			if _, ok := d["supplier_address"]; ok {
				t.Errorf("%s: unregistered supplier's bill has a supplier address", ev.ExtID)
			}
			continue
		}
		wantTmpl := rep.ItemTaxTemplates[strconv.Itoa(ev.Meta.GSTRate)]
		if wantTmpl == "" || item["item_tax_template"] != wantTmpl {
			t.Errorf("%s: item_tax_template %v, want %q (GST %d%%)", ev.ExtID, item["item_tax_template"], wantTmpl, ev.Meta.GSTRate)
		}
		if ev.Kind == EventPurchase && d["supplier_address"] != rep.SupplierAddresses[ev.Meta.SupplierID] {
			t.Errorf("%s: supplier_address %v", ev.ExtID, d["supplier_address"])
		}
	}
	if unregistered == 0 || igst == 0 {
		t.Errorf("the month has %d unregistered bills and %d IGST invoices; the test needs both", unregistered, igst)
	}
}

func TestBooksJournalsBalance(t *testing.T) {
	for _, month := range []string{"2026-08", "2026-09"} {
		p, rep, w := sharmaBooks(t, month)
		f := newFakeBooks(rep)
		writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
		n := 0
		for ext, d := range inserted(t, f) {
			if d["_doctype"] != DocJournalEntry {
				continue
			}
			n++
			var dr, cr money.Paise
			for _, l := range rows(d["accounts"]) {
				dr += mustPaise(t, l["debit_in_account_currency"])
				cr += mustPaise(t, l["credit_in_account_currency"])
			}
			if dr != cr || dr == 0 {
				t.Errorf("%s: debits %s, credits %s", ext, dr.Rupees(), cr.Rupees())
			}
		}
		if n == 0 {
			t.Errorf("%s: no journals", month)
		}
	}
}

// TestBooksOrder checks that names within a DocType follow (Date, ExtID).
func TestBooksOrder(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	f := newFakeBooks(rep)
	res := writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
	byExt := map[string]Event{}
	for _, ev := range w.Events {
		byExt[ev.ExtID] = ev
	}
	for _, dt := range bookDocTypes {
		var exts []string
		for ext, ref := range res.Map {
			if ref.DocType == dt {
				exts = append(exts, ext)
			}
		}
		slices.SortFunc(exts, func(a, b string) int { return strings.Compare(res.Map[a].Name, res.Map[b].Name) })
		for i := 1; i < len(exts); i++ {
			a, b := byExt[exts[i-1]], byExt[exts[i]]
			if a.Date > b.Date || (a.Date == b.Date && a.ExtID > b.ExtID) {
				t.Errorf("%s: %s (%s %s) is named before %s (%s %s)", dt, res.Map[a.ExtID].Name, a.Date, a.ExtID, res.Map[b.ExtID].Name, b.Date, b.ExtID)
			}
		}
		if len(exts) == 0 {
			t.Errorf("no %s posted", dt)
		}
	}
	// Every name is ERPNext's and every ExtID was inserted once.
	for ext, n := range f.inserts {
		if n != 1 {
			t.Errorf("%s inserted %d times", ext, n)
		}
	}
}

func TestBooksResume(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	sales := eventsOf(w, EventSale)
	done, draft := sales[0], sales[1]
	f := newFakeBooks(rep)
	doneName := f.add(DocSalesInvoice, done, 1)
	draftName := f.add(DocSalesInvoice, draft, 0)

	res := writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
	if f.inserts[done.ExtID] != 0 || f.inserts[draft.ExtID] != 0 {
		t.Errorf("inserted the submitted one %d times and the draft %d times", f.inserts[done.ExtID], f.inserts[draft.ExtID])
	}
	for ext, n := range f.inserts {
		if n != 1 {
			t.Errorf("%s inserted %d times", ext, n)
		}
	}
	c := res.Counts[DocSalesInvoice]
	if c.AlreadyPresent != 1 || c.SubmittedFromDraft != 1 || c.Created != len(sales)-2 {
		t.Errorf("Sales Invoice counts %+v", *c)
	}
	if res.Map[done.ExtID].Name != doneName || res.Map[draft.ExtID].Name != draftName {
		t.Errorf("map %v %v", res.Map[done.ExtID], res.Map[draft.ExtID])
	}
	if str(f.docs[DocSalesInvoice][draftName]["docstatus"]) != "1" {
		t.Error("the draft was not submitted")
	}
	// The receipts of both link to the names already in ERPNext.
	ins := inserted(t, f)
	for _, ev := range w.Events {
		if (ev.Kind == EventReceipt || ev.Kind == EventGatewayReceipt) && (ev.Refs[0] == done.ExtID || ev.Refs[0] == draft.ExtID) {
			got := rows(ins[ev.ExtID]["references"])[0]["reference_name"]
			if got != res.Map[ev.Refs[0]].Name {
				t.Errorf("%s references %v, want %s", ev.ExtID, got, res.Map[ev.Refs[0]].Name)
			}
		}
	}

	again := writeBooks(t, f, rep, w, BooksOptions{Suppliers: p.Suppliers})
	if again.Changes() != 0 {
		t.Errorf("a rerun made %d changes: %v", again.Changes(), countsOf(again))
	}
	if !mapsEqual(again.Map, res.Map) {
		t.Error("the rerun's map differs")
	}
}

func TestBooksCancelledIsAnError(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	f := newFakeBooks(rep)
	ev := eventsOf(w, EventPurchase)[0]
	name := f.add(DocPurchaseInvoice, ev, 2)
	_, err := WriteBooks(context.Background(), f.client(t), rep, w, BooksOptions{Suppliers: p.Suppliers})
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "cancelled") || !strings.Contains(err.Error(), ev.ExtID) {
		t.Fatalf("err = %v, want one naming the cancelled %s", err, name)
	}
	if len(f.log()) != 0 {
		t.Errorf("wrote %d times before failing", len(f.log()))
	}
}

func TestBooksStopAfterThenRerun(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	opt := BooksOptions{Suppliers: p.Suppliers}

	one := newFakeBooks(rep)
	want := writeBooks(t, one, rep, w, opt)

	for _, stop := range []int{1, 7, 30, 45} {
		f := newFakeBooks(rep)
		opt.StopAfter = stop
		res, err := WriteBooks(context.Background(), f.client(t), rep, w, opt)
		if !errors.Is(err, ErrStopped) || !res.Stopped {
			t.Fatalf("StopAfter %d: err %v, stopped %v", stop, err, res.Stopped)
		}
		if res.Changes() != stop {
			t.Errorf("StopAfter %d: %d changes", stop, res.Changes())
		}
		opt.StopAfter = 0
		got := writeBooks(t, f, rep, w, opt)
		if got.Changes()+res.Changes() != want.Changes() {
			t.Errorf("StopAfter %d: %d + %d changes, one run made %d", stop, res.Changes(), got.Changes(), want.Changes())
		}
		if !mapsEqual(got.Map, want.Map) {
			t.Errorf("StopAfter %d: the final map differs from one run's", stop)
		}
		if !slices.Equal(f.state(), one.state()) {
			t.Errorf("StopAfter %d: the final documents differ from one run's", stop)
		}
		for ext, n := range f.inserts {
			if n != 1 {
				t.Errorf("StopAfter %d: %s inserted %d times", stop, ext, n)
			}
		}
	}
}

func TestBooksSubmitFailureLeavesDraft(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	opt := BooksOptions{Suppliers: p.Suppliers}
	f := newFakeBooks(rep)
	bad := eventsOf(w, EventPurchase)[2]
	f.failSubmit[bad.ExtID] = "Supplier Invoice No exists in Purchase Invoice PINV-26-00001"
	_, err := WriteBooks(context.Background(), f.client(t), rep, w, opt)
	if err == nil || !strings.Contains(err.Error(), "Supplier Invoice No exists") || !strings.Contains(err.Error(), bad.ExtID) {
		t.Fatalf("err = %v, want the ERPNext message and the ExtID", err)
	}
	for _, wr := range f.log() {
		if wr.doctype == DocPaymentEntry || wr.doctype == DocJournalEntry {
			t.Fatalf("stage 2 ran after a stage 1 failure: %s %s", wr.doctype, wr.line)
		}
	}
	delete(f.failSubmit, bad.ExtID)
	res := writeBooks(t, f, rep, w, opt)
	if c := res.Counts[DocPurchaseInvoice]; c.SubmittedFromDraft != 1 {
		t.Errorf("Purchase Invoice counts %+v, want the draft submitted", *c)
	}
	if f.inserts[bad.ExtID] != 1 {
		t.Errorf("%s inserted %d times", bad.ExtID, f.inserts[bad.ExtID])
	}
}

func TestBooksInsertFailureStopsAllWorkers(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	f := newFakeBooks(rep)
	bad := eventsOf(w, EventSale)[0]
	f.failInsert[bad.ExtID] = "Customer CUST-999 not found"
	_, err := WriteBooks(context.Background(), f.client(t), rep, w, BooksOptions{Suppliers: p.Suppliers, Workers: 1})
	if err == nil || !strings.Contains(err.Error(), "Customer CUST-999 not found") || !strings.Contains(err.Error(), "insert Sales Invoice for "+bad.ExtID) {
		t.Fatalf("err = %v", err)
	}
	// With one worker the Sales Invoices go first; the failure stops the
	// Purchase Invoice worker before it starts.
	for _, wr := range f.log() {
		if wr.doctype != DocSalesInvoice {
			t.Errorf("wrote %s after the failure", wr.doctype)
		}
	}
	if n := len(f.log()); n != 1 {
		t.Errorf("%d writes, want only the failed insert", n)
	}
}

func TestBooksMappingErrorsBeforeAnyWrite(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	noRent := func(r BootstrapReport) BootstrapReport {
		r.Accounts = maps2(r.Accounts)
		delete(r.Accounts, AccountRent)
		return r
	}
	noTemplate := func(r BootstrapReport) BootstrapReport {
		r.ItemTaxTemplates = maps2(r.ItemTaxTemplates)
		delete(r.ItemTaxTemplates, "5")
		return r
	}
	orphan := w
	orphan.Events = slices.DeleteFunc(slices.Clone(w.Events), func(ev Event) bool { return ev.ExtID == eventsOf(w, EventReceipt)[0].Refs[0] })
	cases := []struct {
		name string
		rep  BootstrapReport
		w    World
		opt  BooksOptions
		want string
	}{
		{"no suppliers", rep, w, BooksOptions{}, "is not in BooksOptions.Suppliers"},
		{"no rent account", noRent(rep), w, BooksOptions{Suppliers: p.Suppliers}, `account "Rent" is not in the bootstrap report`},
		{"no 5% template", noTemplate(rep), w, BooksOptions{Suppliers: p.Suppliers}, "no Item Tax Template for GST 5%"},
		{"orphan receipt", rep, orphan, BooksOptions{Suppliers: p.Suppliers}, "neither in this world nor in the prior months' maps"},
		{"no ext id field", func() BootstrapReport { r := rep; r.ExtIDField = ""; return r }(), w, BooksOptions{Suppliers: p.Suppliers}, "run Bootstrap first"},
		{"negative workers", rep, w, BooksOptions{Suppliers: p.Suppliers, Workers: -1}, "must not be negative"},
		{"bad month", rep, World{Company: "sharma", Month: "2026-13"}, BooksOptions{}, "not a YYYY-MM month"},
	}
	for _, tc := range cases {
		f := newFakeBooks(tc.rep)
		_, err := WriteBooks(context.Background(), f.client(t), tc.rep, tc.w, tc.opt)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if len(f.log()) != 0 {
			t.Errorf("%s: %d writes before failing", tc.name, len(f.log()))
		}
	}
	if _, err := WriteBooks(context.Background(), nil, rep, w, BooksOptions{}); err == nil {
		t.Error("nil client: no error")
	}
}

func maps2(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestBooksPriorRefs resolves a receipt whose sale was posted in an earlier
// month through BooksOptions.Prior.
func TestBooksPriorRefs(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	rcpt := eventsOf(w, EventReceipt)[0]
	sale := rcpt.Refs[0]
	var saleEv Event
	for _, ev := range w.Events {
		if ev.ExtID == sale {
			saleEv = ev
		}
	}
	chained := w
	chained.Events = slices.DeleteFunc(slices.Clone(w.Events), func(ev Event) bool { return ev.ExtID == sale })

	f := newFakeBooks(rep)
	prev := saleEv
	prev.Date = "2026-08-20"
	prev.ExtID = sale
	name := f.add(DocSalesInvoice, prev, 1)
	res := writeBooks(t, f, rep, chained, BooksOptions{Suppliers: p.Suppliers, Prior: ERPMap{sale: {DocType: DocSalesInvoice, Name: name}}})
	got := rows(inserted(t, f)[rcpt.ExtID]["references"])[0]["reference_name"]
	if got != name {
		t.Errorf("receipt references %v, want %s", got, name)
	}
	if _, ok := res.Map[sale]; ok {
		t.Error("the prior month's sale is in this month's map")
	}

	_, err := WriteBooks(context.Background(), f.client(t), rep, chained, BooksOptions{Suppliers: p.Suppliers, Prior: ERPMap{sale: {DocType: DocPurchaseInvoice, Name: name}}})
	if err == nil || !strings.Contains(err.Error(), "want a Sales Invoice") {
		t.Errorf("a prior ref of the wrong DocType: err = %v", err)
	}
}

func TestBooksRejectsDuplicateAndWrongDocType(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	ev := eventsOf(w, EventSale)[0]

	f := newFakeBooks(rep)
	f.add(DocSalesInvoice, ev, 1)
	f.add(DocSalesInvoice, ev, 1)
	_, err := WriteBooks(context.Background(), f.client(t), rep, w, BooksOptions{Suppliers: p.Suppliers})
	if err == nil || !strings.Contains(err.Error(), "is on both") {
		t.Errorf("duplicate: err = %v", err)
	}

	f = newFakeBooks(rep)
	f.add(DocJournalEntry, ev, 1)
	_, err = WriteBooks(context.Background(), f.client(t), rep, w, BooksOptions{Suppliers: p.Suppliers})
	if err == nil || !strings.Contains(err.Error(), "posts as a Sales Invoice") {
		t.Errorf("wrong DocType: err = %v", err)
	}

	dup := w
	dup.Events = append(slices.Clone(w.Events), ev)
	_, err = WriteBooks(context.Background(), newFakeBooks(rep).client(t), rep, dup, BooksOptions{Suppliers: p.Suppliers})
	if err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("duplicate ExtID in the world: err = %v", err)
	}
}

func TestBooksAmountMismatchIsAnError(t *testing.T) {
	// A submitted document whose total differs from the event's gross (as
	// if ERPNext had recomputed it) fails the run.
	p, rep, w := sharmaBooks(t, "2026-09")
	f := newFakeBooks(rep)
	ev := eventsOf(w, EventPayroll)[0]
	name := f.add(DocJournalEntry, ev, 0)
	f.mu.Lock()
	f.docs[DocJournalEntry][name][totalField[DocJournalEntry]] = json.Number("1.00")
	f.mu.Unlock()
	_, err := WriteBooks(context.Background(), f.client(t), rep, w, BooksOptions{Suppliers: p.Suppliers})
	if err == nil || !strings.Contains(err.Error(), "total_debit is 1.00") {
		t.Errorf("err = %v", err)
	}
}

func TestBooksResultJSON(t *testing.T) {
	p, rep, w := sharmaBooks(t, "2026-09")
	res := writeBooks(t, newFakeBooks(rep), rep, w, BooksOptions{Suppliers: p.Suppliers})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"skipped":{"gateway_settlement":`, `"Sales Invoice":{"created":`, `"submitted_from_draft":0`, `"already_present":0`, `"stopped":false`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("result JSON %s lacks %s", b, want)
		}
	}
	if bytes.Contains(b, []byte("EVT-")) {
		t.Error("the result JSON carries the map")
	}
	if res.Skipped[EventGatewaySettlement] != len(eventsOf(w, EventGatewaySettlement)) {
		t.Errorf("skipped %v", res.Skipped)
	}
}
