package seed

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
)

// fakeERP is an in-memory Frappe REST API: list, get, insert and update on
// /api/resource, with the naming rules Bootstrap depends on. It is not a
// full Frappe; it implements what Bootstrap calls.
type fakeERP struct {
	mu   sync.Mutex
	docs map[string]map[string]map[string]any // doctype -> name -> document
	// writes logs every insert and update as "METHOD doctype[/name] body".
	writes []string
	// customPrefix makes Custom Field inserts store "custom_" + fieldname,
	// as some Frappe versions do.
	customPrefix bool
}

func newFakeERP() *fakeERP {
	return &fakeERP{docs: map[string]map[string]map[string]any{}}
}

func (f *fakeERP) put(doctype string, d map[string]any) {
	if f.docs[doctype] == nil {
		f.docs[doctype] = map[string]map[string]any{}
	}
	f.docs[doctype][str(d["name"])] = d
}

func (f *fakeERP) doc(doctype, name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docs[doctype][name]
}

func (f *fakeERP) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// addCompany adds a company with ERPNext's standard Indian chart (the
// parts Bootstrap uses), its taxable Item Tax Templates and India
// Compliance's Company custom fields.
func (f *fakeERP) addCompany(name, abbr string) {
	f.put(dtCompany, map[string]any{"name": name, "company_name": name, "abbr": abbr, "country": countryIndia, "gst_category": gstUnregistered})
	acc := func(accountName, parent, root, accountType string, group int) {
		n := ERPAccount(accountName, abbr)
		p := any(nil)
		if parent != "" {
			p = ERPAccount(parent, abbr)
		}
		f.put(dtAccount, map[string]any{
			"name": n, "account_name": accountName, "company": name, "parent_account": p,
			"root_type": root, "account_type": accountType, "is_group": group,
		})
	}
	acc("Application of Funds (Assets)", "", rootAsset, "", 1)
	acc(groupCurrentAssets, "Application of Funds (Assets)", rootAsset, "", 1)
	acc("Accounts Receivable", groupCurrentAssets, rootAsset, "", 1)
	acc(AccountDebtors, "Accounts Receivable", rootAsset, "Receivable", 0)
	acc(groupBankAccounts, groupCurrentAssets, rootAsset, "Bank", 1)
	acc(groupTaxAssets, groupCurrentAssets, rootAsset, "", 1)
	acc(groupFixedAssets, "Application of Funds (Assets)", rootAsset, "", 1)
	acc("Office Equipments", groupFixedAssets, rootAsset, "Fixed Asset", 0)
	acc("Expenses", "", rootExpense, "", 1)
	acc(groupDirectExpense, "Expenses", rootExpense, "", 1)
	acc("Stock Expenses", groupDirectExpense, rootExpense, "", 1)
	acc(AccountCostOfGoodsSold, "Stock Expenses", rootExpense, "Cost of Goods Sold", 0)
	acc(groupIndirectExpense, "Expenses", rootExpense, "", 1)
	acc("Income", "", rootIncome, "", 1)
	acc("Direct Income", "Income", rootIncome, "Income Account", 1)
	acc(AccountSales, "Direct Income", rootIncome, "Income Account", 0)
	acc(groupIndirectIncome, "Income", rootIncome, "Income Account", 1)
	acc("Source of Funds (Liabilities)", "", rootLiability, "", 1)
	acc("Current Liabilities", "Source of Funds (Liabilities)", rootLiability, "", 1)
	acc("Accounts Payable", "Current Liabilities", rootLiability, "", 1)
	acc(AccountCreditors, "Accounts Payable", rootLiability, "Payable", 0)
	acc(groupDutiesAndTaxes, "Current Liabilities", rootLiability, "", 1)
	for _, r := range []int{5, 12, 18, 28} {
		n := fmt.Sprintf("GST %d%% - %s", r, abbr)
		f.put(dtItemTaxTemplate, map[string]any{"name": n, "company": name, "gst_rate": json.Number(strconv.Itoa(r) + ".0"), "gst_treatment": "Taxable", "disabled": 0})
	}
	f.put(dtItemTaxTemplate, map[string]any{"name": "Exempted - " + abbr, "company": name, "gst_rate": json.Number("0.0"), "gst_treatment": "Exempted", "disabled": 0})
}

// newSiteFake is a fake of a fresh site: company, fiscal year, chart,
// groups, HSN codes and an empty GST Settings table. No masters.
func newSiteFake(p Profile) *fakeERP {
	f := newFakeERP()
	f.addCompany(p.ERPCompany, p.Abbr)
	f.put(dtFiscalYear, map[string]any{"name": "2026-2027", "year_start_date": fiscalYearStart, "year_end_date": fiscalYearEnd, "disabled": 0, "companies": []any{}})
	for _, fn := range []string{"gstin", "gst_category", "pan"} {
		f.put(dtCustomField, map[string]any{"name": "Company-" + fn, "dt": dtCompany, "fieldname": fn, "label": fn, "fieldtype": "Data"})
	}
	f.put(dtCustomField, map[string]any{"name": "Purchase Invoice-supplier_gstin", "dt": "Purchase Invoice", "fieldname": "supplier_gstin", "label": "Supplier GSTIN", "fieldtype": "Data"})
	f.put(dtGSTSettings, map[string]any{"name": dtGSTSettings, "gst_accounts": []any{}})
	tree := func(doctype string, leaves ...string) {
		f.put(doctype, map[string]any{"name": "All " + doctype + "s", "is_group": 1})
		for _, l := range leaves {
			f.put(doctype, map[string]any{"name": l, "is_group": 0})
		}
	}
	tree(dtSupplierGroup, "Local", "Services")
	tree(dtCustomerGroup, "Commercial", "Individual")
	tree(dtItemGroup, "Products", "Services")
	f.put(dtTerritory, map[string]any{"name": "All Territories", "is_group": 1})
	f.put(dtTerritory, map[string]any{"name": countryIndia, "is_group": 0})
	for _, it := range itemSpecs {
		f.put(dtGSTHSNCode, map[string]any{"name": it.HSN, "hsn_code": it.HSN})
	}
	return f
}

func (f *fakeERP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/resource/")
	if !ok {
		fail(w, http.StatusNotFound, "PageDoesNotExistError", "no such path")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	doctype, err := url.PathUnescape(parts[0])
	if err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", err.Error())
		return
	}
	name := ""
	if len(parts) == 2 {
		if name, err = url.PathUnescape(parts[1]); err != nil {
			fail(w, http.StatusBadRequest, "ValidationError", err.Error())
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && name == "":
		f.list(w, r, doctype)
	case r.Method == http.MethodGet:
		d, ok := f.docs[doctype][name]
		if !ok {
			fail(w, http.StatusNotFound, "DoesNotExistError", doctype+" "+name+" not found")
			return
		}
		reply(w, d)
	case r.Method == http.MethodPost && name == "":
		f.insert(w, r, doctype)
	case r.Method == http.MethodPut && name != "":
		f.update(w, r, doctype, name)
	default:
		fail(w, http.StatusMethodNotAllowed, "", "method not allowed")
	}
}

func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func fail(w http.ResponseWriter, status int, excType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"exc_type": excType, "exception": msg})
}

func decodeBody(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func (f *fakeERP) list(w http.ResponseWriter, r *http.Request, doctype string) {
	q := r.URL.Query()
	var fields []string
	if s := q.Get("fields"); s != "" {
		if err := json.Unmarshal([]byte(s), &fields); err != nil {
			fail(w, http.StatusBadRequest, "ValidationError", "fields: "+err.Error())
			return
		}
	}
	var filters [][]any
	if s := q.Get("filters"); s != "" {
		if err := json.Unmarshal([]byte(s), &filters); err != nil {
			fail(w, http.StatusBadRequest, "ValidationError", "filters: "+err.Error())
			return
		}
	}
	start, _ := strconv.Atoi(q.Get("limit_start"))
	length, _ := strconv.Atoi(q.Get("limit_page_length"))
	names := make([]string, 0, len(f.docs[doctype]))
	for n := range f.docs[doctype] {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := []map[string]any{}
	for _, n := range names {
		d := f.docs[doctype][n]
		if !matches(d, filters) {
			continue
		}
		row := map[string]any{"name": n}
		for _, fl := range fields {
			row[fl] = d[fl]
		}
		rows = append(rows, row)
	}
	if start > len(rows) {
		start = len(rows)
	}
	rows = rows[start:]
	if length > 0 && length < len(rows) {
		rows = rows[:length]
	}
	reply(w, rows)
}

// matches applies Frappe list filters: =, in, <= and >=.
func matches(d map[string]any, filters [][]any) bool {
	for _, flt := range filters {
		if len(flt) != 3 {
			return false
		}
		field, _ := flt[0].(string)
		op, _ := flt[1].(string)
		got := str(d[field])
		switch op {
		case "=":
			if got != str(flt[2]) {
				return false
			}
		case "in":
			vals, _ := flt[2].([]any)
			found := false
			for _, v := range vals {
				if str(v) == got {
					found = true
				}
			}
			if !found {
				return false
			}
		case "<=":
			if got > str(flt[2]) {
				return false
			}
		case ">=":
			if got < str(flt[2]) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (f *fakeERP) logWrite(method, target string, body map[string]any) {
	b, _ := json.Marshal(body)
	f.writes = append(f.writes, method+" "+target+" "+string(b))
}

func (f *fakeERP) insert(w http.ResponseWriter, r *http.Request, doctype string) {
	body, err := decodeBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", err.Error())
		return
	}
	f.logWrite("POST", doctype, body)
	d := map[string]any{}
	for k, v := range body {
		d[k] = v
	}
	var name string
	switch doctype {
	case dtAccount:
		co, ok := f.docs[dtCompany][str(d["company"])]
		if !ok {
			fail(w, http.StatusExpectationFailed, "LinkValidationError", "no company")
			return
		}
		parent, ok := f.docs[dtAccount][str(d["parent_account"])]
		if !ok || str(parent["is_group"]) != "1" || parent["company"] != d["company"] {
			fail(w, http.StatusExpectationFailed, "ValidationError", "bad parent account")
			return
		}
		d["root_type"] = parent["root_type"]
		name = ERPAccount(str(d["account_name"]), str(co["abbr"]))
	case dtCustomField:
		fn := str(d["fieldname"])
		if f.customPrefix && !strings.HasPrefix(fn, "custom_") {
			fn = "custom_" + fn
		}
		d["fieldname"] = fn
		name = str(d["dt"]) + "-" + fn
	case dtAddress:
		name = str(d["address_title"]) + "-" + str(d["address_type"])
		links, _ := d["links"].([]any)
		for _, l := range links {
			m, _ := l.(map[string]any)
			if _, ok := f.docs[str(m["link_doctype"])][str(m["link_name"])]; !ok {
				fail(w, http.StatusExpectationFailed, "LinkValidationError", "address link to a missing record")
				return
			}
		}
	case dtSupplier:
		name = str(d["supplier_name"])
	case dtCustomer:
		name = str(d["customer_name"])
	case dtItem:
		name = str(d["item_code"])
		if _, ok := f.docs[dtGSTHSNCode][str(d["gst_hsn_code"])]; !ok {
			fail(w, http.StatusExpectationFailed, "LinkValidationError", "no such GST HSN Code")
			return
		}
	default:
		fail(w, http.StatusExpectationFailed, "ValidationError", "the fake can't insert "+doctype)
		return
	}
	if _, dup := f.docs[doctype][name]; dup {
		fail(w, http.StatusConflict, "DuplicateEntryError", name+" exists")
		return
	}
	d["name"] = name
	f.put(doctype, d)
	reply(w, d)
}

func (f *fakeERP) update(w http.ResponseWriter, r *http.Request, doctype, name string) {
	d, ok := f.docs[doctype][name]
	if !ok {
		fail(w, http.StatusNotFound, "DoesNotExistError", doctype+" "+name+" not found")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "ValidationError", err.Error())
		return
	}
	f.logWrite("PUT", doctype+"/"+name, body)
	for k, v := range body {
		d[k] = v
	}
	reply(w, d)
}

// client starts an httptest server for f and returns a frappe client.
func (f *fakeERP) client(t *testing.T) *frappe.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL, ERPSite: "erp.localhost"}, "seed-key", config.NewSecret("seed-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loadSharma(t *testing.T) Profile {
	t.Helper()
	p, err := LoadProfile(filepath.Join("..", "..", "config", "companies", "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func registeredCount(p Profile) int {
	n := 0
	for _, s := range p.Suppliers {
		if s.IsRegistered() {
			n++
		}
	}
	return n
}

const (
	goldenBootstrapReport = "testdata/bootstrap/report-sharma.json"
	goldenBootstrapWrites = "testdata/bootstrap/writes-sharma.txt"
)

func checkGolden(t *testing.T, path string, got []byte) {
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
		t.Errorf("output differs from %s; run go test ./internal/seed -run TestBootstrapGolden -update and review the diff", path)
	}
}

func reportJSON(t *testing.T, rep BootstrapReport) []byte {
	t.Helper()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestBootstrapCreatesEverything(t *testing.T) {
	p := loadSharma(t)
	f := newSiteFake(p)
	rep, err := Bootstrap(context.Background(), f.client(t), p)
	if err != nil {
		t.Fatal(err)
	}

	wantCreated := map[string]int{
		dtAccount:     len(Accounts) - 4 + 1 + 6, // all but Sales, Debtors, Creditors, COGS; the bank; six GST accounts
		dtAddress:     1 + registeredCount(p),
		dtCustomField: len(extIDDocTypes),
		dtSupplier:    len(p.Suppliers),
		dtCustomer:    p.Customers,
		dtItem:        len(itemSpecs),
	}
	for dt, n := range wantCreated {
		c := rep.Counts[dt]
		if c == nil || c.Created != n || c.Updated != 0 {
			t.Errorf("%s: counts %+v, want %d created", dt, c, n)
		}
	}
	if c := rep.Counts[dtCompany]; c == nil || c.Updated != 1 {
		t.Errorf("Company counts %+v, want 1 updated", c)
	}
	if c := rep.Counts[dtGSTSettings]; c == nil || c.Updated != 1 {
		t.Errorf("GST Settings counts %+v, want 1 updated", c)
	}

	// Company GST fields.
	co := f.doc(dtCompany, p.ERPCompany)
	gstin, _ := p.GSTIN()
	if co["gstin"] != gstin || co["gst_category"] != gstRegisteredRegular || co["pan"] != p.PAN {
		t.Errorf("company GST fields %v %v %v", co["gstin"], co["gst_category"], co["pan"])
	}

	// Accounts resolve to "<base> - <abbr>" and sit where the table says.
	for _, base := range append(slices.Clone(Accounts), p.Bank.Account) {
		name := rep.Accounts[base]
		if name != ERPAccount(base, p.Abbr) {
			t.Errorf("account %q resolved to %q", base, name)
			continue
		}
		acc := f.doc(dtAccount, name)
		if acc == nil || acc["company"] != p.ERPCompany || str(acc["is_group"]) != "0" {
			t.Errorf("account %q: %v", name, acc)
		}
	}
	if rep.BankAccount != "HDFC Current 0001 - STPL" {
		t.Errorf("bank account %q", rep.BankAccount)
	}
	for name, want := range map[string][2]string{
		"HDFC Current 0001 - STPL": {"Bank Accounts - STPL", "Bank"},
		"Office Equipment - STPL":  {"Fixed Assets - STPL", "Fixed Asset"},
		"Prepaid Expenses - STPL":  {"Current Assets - STPL", ""},
		"Rent - STPL":              {"Indirect Expenses - STPL", ""},
		"Interest Income - STPL":   {"Indirect Income - STPL", ""},
	} {
		acc := f.doc(dtAccount, name)
		if str(acc["parent_account"]) != want[0] || str(acc["account_type"]) != want[1] {
			t.Errorf("%s: parent %v, type %v; want %v", name, acc["parent_account"], acc["account_type"], want)
		}
	}

	// GST Settings rows and accounts.
	wantIn := GSTAccounts{CGST: "Input Tax CGST - STPL", SGST: "Input Tax SGST - STPL", IGST: "Input Tax IGST - STPL"}
	wantOut := GSTAccounts{CGST: "Output Tax CGST - STPL", SGST: "Output Tax SGST - STPL", IGST: "Output Tax IGST - STPL"}
	if rep.InputGST != wantIn || rep.OutputGST != wantOut {
		t.Errorf("GST accounts %+v %+v", rep.InputGST, rep.OutputGST)
	}
	rows, _ := f.doc(dtGSTSettings, dtGSTSettings)["gst_accounts"].([]any)
	if len(rows) != 2 {
		t.Fatalf("GST Settings has %d rows, want 2", len(rows))
	}
	if acc := f.doc(dtAccount, "Output Tax IGST - STPL"); str(acc["parent_account"]) != "Duties and Taxes - STPL" || acc["root_type"] != rootLiability {
		t.Errorf("Output Tax IGST: %v", acc)
	}

	// Custom fields.
	for _, dt := range extIDDocTypes {
		cf := f.doc(dtCustomField, dt+"-"+ExtIDField)
		if cf == nil || cf["fieldtype"] != "Data" || cf["label"] != extIDLabel ||
			str(cf["no_copy"]) != "1" || str(cf["read_only"]) != "1" || str(cf["search_index"]) != "1" {
			t.Errorf("custom field on %s: %v", dt, cf)
		}
	}

	// Suppliers and their addresses.
	for _, s := range p.Suppliers {
		sup := f.doc(dtSupplier, s.Name)
		if sup == nil || rep.Suppliers[s.ID] != s.Name {
			t.Errorf("supplier %s missing", s.ID)
			continue
		}
		if !s.IsRegistered() {
			if str(sup["gstin"]) != "" || sup["gst_category"] != gstUnregistered {
				t.Errorf("unregistered %s: gstin %v, category %v", s.ID, sup["gstin"], sup["gst_category"])
			}
			if _, ok := rep.SupplierAddresses[s.ID]; ok {
				t.Errorf("unregistered %s has an address", s.ID)
			}
			continue
		}
		g, _ := p.SupplierGSTIN(s)
		if sup["gstin"] != g || sup["gst_category"] != gstRegisteredRegular || sup["pan"] != p.SupplierPAN(s) {
			t.Errorf("registered %s: %v %v %v", s.ID, sup["gstin"], sup["gst_category"], sup["pan"])
		}
		if err := ValidGSTIN(str(sup["gstin"])); err != nil {
			t.Errorf("supplier %s GSTIN: %v", s.ID, err)
		}
		a := f.doc(dtAddress, rep.SupplierAddresses[s.ID])
		st := states[s.StateCode]
		if a == nil || a["gstin"] != g || a["state"] != st.Name || a["pincode"] != st.Pincode || !linksTo(a, dtSupplier, s.Name) {
			t.Errorf("address of %s: %v", s.ID, a)
		}
	}
	ca := f.doc(dtAddress, rep.CompanyAddress)
	if ca == nil || ca["gstin"] != gstin || str(ca["is_your_company_address"]) != "1" || ca["state"] != "Karnataka" || !linksTo(ca, dtCompany, p.ERPCompany) {
		t.Errorf("company address %v", ca)
	}

	// Customers and items.
	if len(rep.Customers) != p.Customers || rep.Customers[0] != "CUST-001" || rep.Customers[p.Customers-1] != CustomerName(p.Customers) {
		t.Errorf("customers %v", rep.Customers)
	}
	for _, it := range itemSpecs {
		item := f.doc(dtItem, it.Code)
		if item == nil || str(item["is_stock_item"]) != "0" || item["stock_uom"] != uomNos || item["gst_hsn_code"] != it.HSN {
			t.Errorf("item %s: %v", it.Code, item)
		}
	}
	if rep.ItemTaxTemplates["18"] != "GST 18% - STPL" || rep.ItemTaxTemplates["5"] != "GST 5% - STPL" || rep.ItemTaxTemplates["12"] != "GST 12% - STPL" {
		t.Errorf("item tax templates %v", rep.ItemTaxTemplates)
	}
	if rep.ExtIDField != ExtIDField {
		t.Errorf("ext id field %q", rep.ExtIDField)
	}
}

func TestBootstrapSecondRunNoChanges(t *testing.T) {
	p := loadSharma(t)
	f := newSiteFake(p)
	c := f.client(t)
	first, err := Bootstrap(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Changes() == 0 {
		t.Fatal("first run changed nothing")
	}
	writes := len(f.writeLog())
	second, err := Bootstrap(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if n := second.Changes(); n != 0 {
		t.Errorf("second run made %d changes: %+v", n, second.Counts)
	}
	if extra := f.writeLog()[writes:]; len(extra) != 0 {
		t.Errorf("second run wrote:\n%s", strings.Join(extra, "\n"))
	}
	// The resolved names are the same.
	first.Counts, second.Counts = nil, nil
	if !bytes.Equal(reportJSON(t, first), reportJSON(t, second)) {
		t.Error("the second run resolved different names")
	}
}

// TestBootstrapGolden pins the report and every write on a fresh site:
// the same profile gives byte-identical requests and output.
func TestBootstrapGolden(t *testing.T) {
	p := loadSharma(t)
	var reports, logs [2][]byte
	for i := range 2 {
		f := newSiteFake(p)
		rep, err := Bootstrap(context.Background(), f.client(t), p)
		if err != nil {
			t.Fatal(err)
		}
		reports[i] = reportJSON(t, rep)
		logs[i] = []byte(strings.Join(f.writeLog(), "\n") + "\n")
	}
	if !bytes.Equal(reports[0], reports[1]) || !bytes.Equal(logs[0], logs[1]) {
		t.Fatal("two runs on fresh sites differ")
	}
	checkGolden(t, goldenBootstrapReport, reports[0])
	checkGolden(t, goldenBootstrapWrites, logs[0])
}

func TestBootstrapUpdatesOwnedFields(t *testing.T) {
	p := loadSharma(t)
	f := newSiteFake(p)
	c := f.client(t)
	if _, err := Bootstrap(context.Background(), c, p); err != nil {
		t.Fatal(err)
	}
	// Someone changed an owned field (gst_category) and a field Bootstrap
	// doesn't own (supplier_group).
	s := p.Suppliers[0]
	f.mu.Lock()
	f.docs[dtSupplier][s.Name]["gst_category"] = gstUnregistered
	f.docs[dtSupplier][s.Name]["supplier_group"] = "Services"
	f.mu.Unlock()

	rep, err := Bootstrap(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if c := rep.Counts[dtSupplier]; c.Updated != 1 || !slices.Equal(c.UpdatedNames, []string{s.Name}) {
		t.Errorf("supplier counts %+v", c)
	}
	if rep.Changes() != 1 {
		t.Errorf("%d changes, want 1", rep.Changes())
	}
	sup := f.doc(dtSupplier, s.Name)
	if sup["gst_category"] != gstRegisteredRegular || sup["supplier_group"] != "Services" {
		t.Errorf("supplier after repair: %v, %v", sup["gst_category"], sup["supplier_group"])
	}
}

func TestBootstrapIgnoresOtherCompanysAccounts(t *testing.T) {
	p := loadSharma(t)
	f := newSiteFake(p)
	f.addCompany("Other Traders Pvt Ltd", "OTPL")
	// Same account_name, other company: ours must still be created.
	for _, base := range []string{AccountRent, p.Bank.Account, "Input Tax IGST"} {
		f.put(dtAccount, map[string]any{
			"name": ERPAccount(base, "OTPL"), "account_name": base, "company": "Other Traders Pvt Ltd",
			"parent_account": "Indirect Expenses - OTPL", "root_type": rootExpense, "is_group": 0,
		})
	}
	// A GST Settings row for the other company doesn't count for ours.
	f.docs[dtGSTSettings][dtGSTSettings]["gst_accounts"] = []any{map[string]any{
		"name": "row1", "company": "Other Traders Pvt Ltd", "account_type": gstInput,
		"cgst_account": "Input Tax IGST - OTPL", "sgst_account": "Input Tax IGST - OTPL", "igst_account": "Input Tax IGST - OTPL",
	}}

	rep, err := Bootstrap(context.Background(), f.client(t), p)
	if err != nil {
		t.Fatal(err)
	}
	for base, want := range map[string]string{
		AccountRent:    "Rent - STPL",
		p.Bank.Account: "HDFC Current 0001 - STPL",
	} {
		if rep.Accounts[base] != want {
			t.Errorf("%s resolved to %q, want %q", base, rep.Accounts[base], want)
		}
		if !slices.Contains(rep.Counts[dtAccount].CreatedNames, want) {
			t.Errorf("%s was not created", want)
		}
	}
	if rep.InputGST.IGST != "Input Tax IGST - STPL" {
		t.Errorf("input IGST %q", rep.InputGST.IGST)
	}
	if other := f.doc(dtAccount, "Rent - OTPL"); other["company"] != "Other Traders Pvt Ltd" {
		t.Errorf("the other company's account changed: %v", other)
	}
	rows, _ := f.doc(dtGSTSettings, dtGSTSettings)["gst_accounts"].([]any)
	if len(rows) != 3 {
		t.Errorf("GST Settings has %d rows, want the other company's and our two", len(rows))
	}
}

func TestBootstrapRepairsGSTRowWithMissingAccount(t *testing.T) {
	p := loadSharma(t)
	f := newSiteFake(p)
	f.docs[dtGSTSettings][dtGSTSettings]["gst_accounts"] = []any{
		map[string]any{"name": "in", "company": p.ERPCompany, "account_type": gstInput,
			"cgst_account": "Gone CGST - STPL", "sgst_account": "Gone SGST - STPL", "igst_account": "Gone IGST - STPL"},
	}
	rep, err := Bootstrap(context.Background(), f.client(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.InputGST.CGST != "Input Tax CGST - STPL" {
		t.Errorf("input CGST %q", rep.InputGST.CGST)
	}
	rows, _ := f.doc(dtGSTSettings, dtGSTSettings)["gst_accounts"].([]any)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	in, _ := rows[0].(map[string]any)
	if in["name"] != "in" || in["igst_account"] != "Input Tax IGST - STPL" {
		t.Errorf("input row not repaired in place: %v", in)
	}
}

func TestBootstrapCustomFieldnameMismatch(t *testing.T) {
	p := loadSharma(t)

	t.Run("Frappe prefixes on insert", func(t *testing.T) {
		f := newSiteFake(p)
		f.customPrefix = true
		_, err := Bootstrap(context.Background(), f.client(t), p)
		if err == nil || !strings.Contains(err.Error(), `"custom_copilot_ext_id"`) {
			t.Fatalf("err = %v, want one naming the stored fieldname", err)
		}
	})

	t.Run("an existing prefixed field", func(t *testing.T) {
		f := newSiteFake(p)
		f.put(dtCustomField, map[string]any{"name": "Journal Entry-custom_copilot_ext_id", "dt": "Journal Entry",
			"fieldname": "custom_copilot_ext_id", "label": extIDLabel, "fieldtype": "Data"})
		_, err := Bootstrap(context.Background(), f.client(t), p)
		if err == nil || !strings.Contains(err.Error(), `"custom_copilot_ext_id"`) {
			t.Fatalf("err = %v, want one naming the stored fieldname", err)
		}
	})
}

func TestBootstrapRequiresStandardAccounts(t *testing.T) {
	p := loadSharma(t)
	for _, base := range []string{AccountSales, AccountDebtors, AccountCreditors} {
		t.Run(base, func(t *testing.T) {
			f := newSiteFake(p)
			delete(f.docs[dtAccount], ERPAccount(base, p.Abbr))
			_, err := Bootstrap(context.Background(), f.client(t), p)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q does not exist", base)) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestBootstrapFailsClearly(t *testing.T) {
	p := loadSharma(t)
	cases := []struct {
		name  string
		edit  func(*fakeERP)
		want  string
		edits func(*Profile)
	}{
		{"no company", func(f *fakeERP) { delete(f.docs[dtCompany], p.ERPCompany) }, "does not exist in ERPNext", nil},
		{"abbreviation differs", func(f *fakeERP) { f.docs[dtCompany][p.ERPCompany]["abbr"] = "XX" }, `abbreviation "XX"`, nil},
		{"no fiscal year", func(f *fakeERP) { delete(f.docs[dtFiscalYear], "2026-2027") }, "no enabled Fiscal Year", nil},
		{"fiscal year for another company", func(f *fakeERP) {
			f.docs[dtFiscalYear]["2026-2027"]["companies"] = []any{map[string]any{"company": "Someone Else"}}
		}, "no enabled Fiscal Year", nil},
		{"short fiscal year", func(f *fakeERP) { f.docs[dtFiscalYear]["2026-2027"]["year_end_date"] = "2026-12-31" }, "no enabled Fiscal Year", nil},
		{"rent is a group", func(f *fakeERP) {
			f.put(dtAccount, map[string]any{"name": "Rent - STPL", "account_name": "Rent", "company": p.ERPCompany, "is_group": 1, "root_type": rootExpense})
		}, "is a group", nil},
		{"rent under assets", func(f *fakeERP) {
			f.put(dtAccount, map[string]any{"name": "Rent - STPL", "account_name": "Rent", "company": p.ERPCompany, "is_group": 0, "root_type": rootAsset})
		}, `root type "Asset"`, nil},
		{"account with a number", func(f *fakeERP) {
			f.put(dtAccount, map[string]any{"name": "5100 - Rent - STPL", "account_name": "Rent", "company": p.ERPCompany, "is_group": 0, "root_type": rootExpense})
		}, "naming rule", nil},
		{"no parent group", func(f *fakeERP) { delete(f.docs[dtAccount], "Fixed Assets - STPL") }, `parent group "Fixed Assets"`, nil},
		{"no HSN code", func(f *fakeERP) { delete(f.docs[dtGSTHSNCode], "847130") }, `"847130" does not exist`, nil},
		{"no item tax template", func(f *fakeERP) { delete(f.docs[dtItemTaxTemplate], "GST 12% - STPL") }, "GST 12%", nil},
		{"no GST Settings", func(f *fakeERP) { delete(f.docs[dtGSTSettings], dtGSTSettings) }, "GST Settings not found", nil},
		{"state without a pincode", nil, `state code "01"`, func(p *Profile) { p.Suppliers[0].StateCode = "01" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pp := p
			pp.Suppliers = slices.Clone(p.Suppliers)
			if tc.edits != nil {
				tc.edits(&pp)
			}
			f := newSiteFake(pp)
			if tc.edit != nil {
				tc.edit(f)
			}
			_, err := Bootstrap(context.Background(), f.client(t), pp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBootstrapNilClient(t *testing.T) {
	if _, err := Bootstrap(context.Background(), nil, loadSharma(t)); err == nil {
		t.Fatal("want an error for a nil client")
	}
}

func TestERPAccount(t *testing.T) {
	if got := ERPAccount("Rent", "STPL"); got != "Rent - STPL" {
		t.Errorf("ERPAccount = %q", got)
	}
}

func TestAccountSpecsCoverAccounts(t *testing.T) {
	for _, base := range Accounts {
		if _, ok := accountSpecs[base]; !ok {
			t.Errorf("account %q has no placement rule", base)
		}
	}
	if len(accountSpecs) != len(Accounts) {
		t.Errorf("%d placement rules for %d accounts", len(accountSpecs), len(Accounts))
	}
}

// eventsWithLines are the event kinds posted as documents with item lines
// (Sales and Purchase Invoices); the rest become payments or journals.
var eventsWithLines = []string{EventSale, EventPurchase}

func TestItemForCoversEventKinds(t *testing.T) {
	codes := map[string]bool{}
	for _, it := range itemSpecs {
		codes[it.Code] = true
	}
	for _, kind := range EventKinds {
		for _, sk := range append([]string{""}, supplierKinds...) {
			for _, asset := range []bool{false, true} {
				got := ItemFor(kind, sk, asset)
				switch {
				case kind == EventSale || (kind == EventPurchase && (sk != "" || asset)):
					if !codes[got] {
						t.Errorf("ItemFor(%q, %q, %v) = %q, not a bootstrapped item", kind, sk, asset, got)
					}
				case !slices.Contains(eventsWithLines, kind) && got != "":
					t.Errorf("ItemFor(%q, %q, %v) = %q; %s has no item lines", kind, sk, asset, got, kind)
				}
			}
		}
	}
	if ItemFor(EventPurchase, KindGoods, true) != ItemLaptop || ItemFor(EventSale, "", false) != ItemSales {
		t.Error("asset and sale items")
	}
	// Every line-bearing event of the generated worlds maps to an item.
	profiles, err := LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		kinds := map[string]string{}
		for _, s := range p.Suppliers {
			kinds[s.ID] = s.Kind
		}
		for _, month := range []string{"2026-09", "2026-10"} {
			w, err := Generate(p, month, Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range w.Events {
				if !slices.Contains(eventsWithLines, ev.Kind) {
					continue
				}
				item := ItemFor(ev.Kind, kinds[ev.Meta.SupplierID], ev.Account == AccountOfficeEquipment)
				if !codes[item] {
					t.Errorf("%s %s (%s): no item", p.ID, ev.ExtID, ev.Kind)
				}
			}
		}
	}
}

// TestStatesCoverProfiles checks that every state code the profiles use
// (the company's and registered suppliers') has a state name and a pincode.
func TestStatesCoverProfiles(t *testing.T) {
	profiles, err := LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 0 {
		t.Fatal("no profiles")
	}
	for _, p := range profiles {
		codes := []string{p.StateCode}
		for _, s := range p.Suppliers {
			if s.IsRegistered() {
				codes = append(codes, s.StateCode)
			}
		}
		for _, c := range codes {
			st, ok := states[c]
			if !ok {
				t.Errorf("%s: state code %s has no entry in states", p.ID, c)
				continue
			}
			if len(st.Pincode) != 6 || st.Name == "" || st.City == "" {
				t.Errorf("state %s: %+v", c, st)
			}
		}
	}
}

func TestGSTRatesUsed(t *testing.T) {
	got := gstRatesUsed(loadSharma(t))
	if !slices.Equal(got, []int{5, 12, 18}) {
		t.Errorf("rates %v", got)
	}
}

func TestWholeNumber(t *testing.T) {
	for in, want := range map[string]string{"18": "18", "18.0": "18", "5.000": "5", "12.5": "", "": "", "x": ""} {
		got, ok := wholeNumber(in)
		if got != want || ok != (want != "") {
			t.Errorf("wholeNumber(%q) = %q, %v", in, got, ok)
		}
	}
}
