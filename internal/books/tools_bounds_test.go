package books

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

// ---- documents per call ----

// TestToolsDocCap shows each document tool refuses a range with more than
// MaxDocsPerCall documents as an input error, before fetching any
// document.
func TestToolsDocCap(t *testing.T) {
	cases := []struct {
		tool, doctype, field string
		args                 map[string]any
		mk                   func(i int) doc
	}{
		{ToolListPurchaseInvoices, "Purchase Invoice", "to_date", september(), func(i int) doc {
			return purchaseDoc(fmt.Sprintf("PINV-%05d", i), testCompany, "SUP-A", "2026-09-05", 1, "1", "1", nil)
		}},
		{ToolListSalesInvoices, "Sales Invoice", "to_date", september(), func(i int) doc {
			return salesDoc(fmt.Sprintf("SINV-%05d", i), testCompany, "CUST-1", "2026-09-05", 1, "1", "0")
		}},
		{ToolListPayments, "Payment Entry", "to_date", september(), func(i int) doc {
			return paymentDoc(fmt.Sprintf("PE-%05d", i), testCompany, "Pay", "SUP-A", "2026-09-05", 1, "1", "Creditors - TT")
		}},
		{ToolListRecurringSuppliers, "Purchase Invoice", "lookback_months",
			map[string]any{"company": toolCompanyID, "before_month": "2026-10", "lookback_months": 1, "min_occurrences": 1},
			func(i int) doc {
				return purchaseDoc(fmt.Sprintf("PINV-%05d", i), testCompany, "SUP-A", "2026-09-05", 1, "1", "1", nil)
			}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			f := newFakeERP(t)
			for i := range MaxDocsPerCall + 1 {
				f.add(c.doctype, c.mk(i))
			}
			cs := toolSession(t, f)
			msg := callErr(t, cs, c.tool, c.args)
			want := fmt.Sprintf("invalid input: %s matches more than %d documents; narrow the range", c.field, MaxDocsPerCall)
			if msg != want {
				t.Errorf("error %q, want %q", msg, want)
			}
			for _, r := range f.requests() {
				if r.name != "" {
					t.Fatalf("fetched %s %s although the range is over the cap", r.doctype, r.name)
				}
			}
		})
	}
}

// ---- one deadline per call ----

// TestToolsTimeout shows the whole call shares one deadline: each ERPNext
// response alone is well inside it, but together they are not.
func TestToolsTimeout(t *testing.T) {
	old := toolTimeout
	toolTimeout = 300 * time.Millisecond
	t.Cleanup(func() { toolTimeout = old })

	f := purchaseERP(t)
	f.delay = 120 * time.Millisecond
	var logs bytes.Buffer
	cs := toolSessionLog(t, f, slog.New(slog.NewTextHandler(&logs, nil)))
	start := time.Now()
	msg := callErr(t, cs, ToolListPurchaseInvoices, september())
	if msg != MsgUnavailable {
		t.Errorf("error %q, want %q", msg, MsgUnavailable)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("call took %s, want it cut at the 300ms tool deadline", d)
	}
	if !strings.Contains(logs.String(), "deadline exceeded") {
		t.Errorf("log lacks the deadline error: %s", logs.String())
	}
}

func TestToolsTimeoutDefault(t *testing.T) {
	if ToolTimeout != 90*time.Second || toolTimeout != ToolTimeout {
		t.Errorf("tool timeout = %s (in use %s), want 90s", ToolTimeout, toolTimeout)
	}
}

// ---- errors the model sees ----

// leaks are strings that must never reach the model in an error.
var leaks = []string{"http", "127.0.0.1", "?", "limit_start", "filters", "Traceback", "SELECT", "tab", "api/resource", "secret"}

func TestToolsErrorsToModel(t *testing.T) {
	permission := func(r *http.Request) (int, string, bool) {
		return http.StatusForbidden, `{"exc_type":"PermissionError","exception":"frappe.exceptions.PermissionError: secret detail about tabGL Entry"}`, true
	}
	notFound := func(r *http.Request) (int, string, bool) {
		if strings.Count(r.URL.Path, "/") > 3 { // a single-document GET
			return http.StatusNotFound, `{"exc_type":"DoesNotExistError","exception":"Purchase Invoice PINV-0001 not found at http://erp/api/resource"}`, true
		}
		return 0, "", false
	}
	validation := func(r *http.Request) (int, string, bool) {
		return http.StatusExpectationFailed, `{"exc_type":"ValidationError","exception":"Traceback: SELECT name FROM tabGL Entry WHERE x=1"}`, true
	}
	cases := []struct {
		name string
		fail func(*http.Request) (int, string, bool)
		tool string
		want string
		log  string
	}{
		{"permission", permission, ToolListGLEntries, MsgPermission, "PermissionError"},
		{"not found", notFound, ToolListPurchaseInvoices, MsgNotFound, "DoesNotExistError"},
		{"other ERPNext error", validation, ToolListPayments, MsgUnavailable, "ValidationError"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := purchaseERP(t)
			f.fail = c.fail
			var logs bytes.Buffer
			cs := toolSessionLog(t, f, slog.New(slog.NewTextHandler(&logs, nil)))
			msg := callErr(t, cs, c.tool, september())
			if msg != c.want {
				t.Errorf("error %q, want %q", msg, c.want)
			}
			for _, l := range leaks {
				if strings.Contains(msg, l) {
					t.Errorf("error %q leaks %q", msg, l)
				}
			}
			if !strings.Contains(logs.String(), c.log) {
				t.Errorf("server log lacks %q: %s", c.log, logs.String())
			}
		})
	}
}

// TestToolsTransportErrorHidden points the tools at a closed server: the
// *url.Error (base URL, query string) goes to the log, not the model.
func TestToolsTransportErrorHidden(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := frappe.New(config.Config{ERPBaseURL: url}, "botkey", config.NewSecret("botsecret"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := mcp.NewServer(&mcp.Implementation{Name: "books-test", Version: "0"}, nil)
	RegisterTools(s, ToolDeps{
		Client:    c,
		Companies: ProfileCompanies([]seed.Profile{{ID: toolCompanyID, ERPCompany: testCompany}}),
		Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	})
	cs := connect(t, s)
	msg := callErr(t, cs, ToolListGLEntries, september())
	if msg != MsgUnavailable {
		t.Errorf("error %q, want %q", msg, MsgUnavailable)
	}
	for _, l := range append(leaks, strings.TrimPrefix(url, "http://")) {
		if strings.Contains(msg, l) {
			t.Errorf("error %q leaks %q", msg, l)
		}
	}
	if !strings.Contains(logs.String(), strings.TrimPrefix(url, "http://")) {
		t.Errorf("server log lacks the transport error: %s", logs.String())
	}
}

func TestToolsUnbalancedLedger(t *testing.T) {
	f := trialBalanceERP(t)
	f.add("GL Entry", glDoc("GLE-0099", testCompany, "Bank Charges - TT", "1", "0", "2026-09-29", 1, 0))
	cs := toolSession(t, f)
	if msg := callErr(t, cs, ToolGetTrialBalance, september()); msg != MsgUnbalanced {
		t.Errorf("error %q, want %q", msg, MsgUnbalanced)
	}
}

func TestToolsAccountHistoryBadAccount(t *testing.T) {
	f := trialBalanceERP(t)
	group := accountDoc("Expenses - TT", testCompany, "Expense")
	group["is_group"] = 1
	f.add("Account", group)
	cs := toolSession(t, f)
	for account, want := range map[string]string{
		"Nope - TT":     `invalid input: account no account "Nope - TT" in the company`,
		"Expenses - TT": `invalid input: account "Expenses - TT" is a group account`,
	} {
		msg := callErr(t, cs, ToolGetAccountHistory, map[string]any{"company": toolCompanyID, "account": account, "months": 2})
		if !strings.HasPrefix(msg, want) {
			t.Errorf("%s: error %q, want prefix %q", account, msg, want)
		}
	}
	var ok AccountHistoryOutput
	call(t, cs, ToolGetAccountHistory, map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 2}, &ok)
	// The accounts are read once per call (a list starts at limit_start
	// 0), not again inside AccountHistory.
	starts := 0
	for _, r := range f.lists("Account") {
		if r.start == 0 {
			starts++
		}
	}
	if starts != 3 {
		t.Errorf("%d Account list reads for three calls, want 3", starts)
	}
}

// ---- free text ----

func TestToolsTruncation(t *testing.T) {
	long := strings.Repeat("é", MaxTextRunes+100)
	exact := strings.Repeat("x", MaxTextRunes)
	clipped := strings.Repeat("é", MaxTextRunes) + TruncatedMarker

	if clipText(exact) != exact {
		t.Error("text of exactly MaxTextRunes runes was cut")
	}
	if got := clipText(long); got != clipped || utf8.RuneCountInString(got) != MaxTextRunes+utf8.RuneCountInString(TruncatedMarker) {
		t.Errorf("clipText cut to %d runes", utf8.RuneCountInString(got))
	}

	f := newFakeERP(t)
	pi := purchaseDoc("PINV-0001", testCompany, "SUP-A", "2026-09-05", 1, "1", "1",
		[]doc{taxRow("t1", "Input Tax IGST - TT", "18", "0", "igst")})
	pi["remarks"], pi["bill_no"], pi["supplier_name"] = long, long, long
	pi["items"].([]doc)[0]["description"] = long
	pi["taxes"].([]doc)[0]["description"] = long
	f.add("Purchase Invoice", pi)
	si := salesDoc("SINV-0001", testCompany, "CUST-1", "2026-09-05", 1, "1", "1")
	si["remarks"], si["customer_name"] = long, long
	si["items"].([]doc)[0]["description"] = long
	f.add("Sales Invoice", si)
	pe := paymentDoc("PE-0001", testCompany, "Pay", "SUP-A", "2026-09-05", 1, "1", "Creditors - TT")
	pe["remarks"], pe["reference_no"], pe["party_name"] = long, long, long
	f.add("Payment Entry", pe)
	gl := glDoc("GLE-0001", testCompany, "Bank - TT", "1", "0", "2026-09-05", 1, 0)
	gl["remarks"], gl["against"] = long, long
	f.add("GL Entry", gl)
	cs := toolSession(t, f)

	var p PurchaseInvoicesOutput
	call(t, cs, ToolListPurchaseInvoices, september(), &p)
	var s SalesInvoicesOutput
	call(t, cs, ToolListSalesInvoices, september(), &s)
	var pay PaymentsOutput
	call(t, cs, ToolListPayments, september(), &pay)
	var g GLEntriesOutput
	call(t, cs, ToolListGLEntries, september(), &g)

	fields := map[string]string{
		"pi.remarks": p.Invoices[0].Remarks, "pi.bill_no": p.Invoices[0].BillNo,
		"pi.supplier_name": p.Invoices[0].SupplierName, "pi.line.description": p.Invoices[0].Lines[0].Description,
		"pi.tax.description": p.Invoices[0].Taxes[0].Description,
		"si.remarks":         s.Invoices[0].Remarks, "si.customer_name": s.Invoices[0].CustomerName,
		"si.line.description": s.Invoices[0].Lines[0].Description,
		"pe.remarks":          pay.Payments[0].Remarks, "pe.reference_no": pay.Payments[0].ReferenceNo,
		"pe.party_name": pay.Payments[0].PartyName,
		"gl.remarks":    g.Entries[0].Remarks, "gl.against": g.Entries[0].Against,
	}
	for name, v := range fields {
		if v != clipped {
			t.Errorf("%s: %d runes, want %d runes ending in %q", name, utf8.RuneCountInString(v), MaxTextRunes, TruncatedMarker)
		}
	}
}
