package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const erpTest = "http://erp.test:8080"

// fakeFindings is an in-memory FindingsStore.
type fakeFindings struct {
	mu       sync.Mutex
	findings map[uuid.UUID]store.Finding
	runs     map[uuid.UUID]store.CloseRun
	steps    map[uuid.UUID][]store.Step
	arts     map[string]store.Artifact
	setErr   error
	auditErr error
	audits   []store.AuditLogEntry
	sets     []string
}

func newFakeFindings() *fakeFindings {
	return &fakeFindings{
		findings: map[uuid.UUID]store.Finding{}, runs: map[uuid.UUID]store.CloseRun{},
		steps: map[uuid.UUID][]store.Step{}, arts: map[string]store.Artifact{},
	}
}

func (f *fakeFindings) GetFinding(_ context.Context, id uuid.UUID) (store.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.findings[id]
	if !ok {
		return store.Finding{}, fmt.Errorf("%w: finding", store.ErrNotFound)
	}
	return x, nil
}

func (f *fakeFindings) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.runs[id]
	if !ok {
		return store.CloseRun{}, fmt.Errorf("%w: run", store.ErrNotFound)
	}
	return x, nil
}

func (f *fakeFindings) ListSteps(_ context.Context, id uuid.UUID) ([]store.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps[id], nil
}

func (f *fakeFindings) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.arts[sha]
	if !ok {
		return store.Artifact{}, fmt.Errorf("%w: artifact", store.ErrNotFound)
	}
	return a, nil
}

func (f *fakeFindings) SetFindingStatus(_ context.Context, id uuid.UUID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	x := f.findings[id]
	x.Status = status
	f.findings[id] = x
	f.sets = append(f.sets, status)
	return nil
}

func (f *fakeFindings) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return 0, f.auditErr
	}
	f.audits = append(f.audits, e)
	return int64(len(f.audits)), nil
}

func (f *fakeFindings) status(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findings[id].Status
}

func (f *fakeFindings) auditCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.audits)
}

// put stores a finding with a run (id runID(1), acme 2026-09).
func (f *fakeFindings) put(x store.Finding) store.Finding {
	f.mu.Lock()
	defer f.mu.Unlock()
	x.RunID = runID(1)
	if x.Keys == nil {
		x.Keys = map[string]string{}
	}
	f.runs[runID(1)] = store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-09", Status: store.RunDone}
	f.findings[x.ID] = x
	return x
}

// snap stores a tool-result artifact and returns its sha key.
func (f *fakeFindings) snap(key, server, tool string, result any) string {
	return f.art(key, store.ArtifactToolResult, map[string]any{"server": server, "tool": tool, "args": map[string]any{}, "result": result})
}

func (f *fakeFindings) art(key, kind string, content any) string {
	b, _ := json.Marshal(content)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arts[key] = store.Artifact{SHA256: key, Kind: kind, Content: b}
	return key
}

func findingsServer(t testing.TB, ff *fakeFindings, mods ...func(*Options)) *testServer {
	t.Helper()
	return newTestServer(t, append([]func(*Options){func(o *Options) {
		o.ERPBaseURL = erpTest
		o.Runs = &RunsBackend{
			Store:    newFakeRunsStore(),
			Execute:  func(context.Context, uuid.UUID) error { return nil },
			Findings: ff,
		}
	}}, mods...)...)
}

func fnd(n byte, typ, status string) store.Finding {
	return store.Finding{ID: fid(n), Type: typ, Severity: "high", Title: "title " + typ, Status: status}
}

func ref(server, tool, art string, ids ...string) store.EvidenceRef {
	return store.EvidenceRef{Server: server, Tool: tool, Args: json.RawMessage(`{"company":"acme"}`), IDs: ids, Artifact: art}
}

func page(t testing.TB, ts *testServer, role Role, id uuid.UUID) string {
	t.Helper()
	w := ts.get(t, role, "/findings/"+id.String())
	if w.Code != http.StatusOK {
		t.Fatalf("GET finding = %d: %s", w.Code, w.Body)
	}
	return w.Body.String()
}

func wantAll(t testing.TB, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func wantNone(t testing.TB, body string, nots ...string) {
	t.Helper()
	for _, w := range nots {
		if strings.Contains(body, w) {
			t.Errorf("page has %q", w)
		}
	}
}

func TestFindingPage(t *testing.T) {
	ff := newFakeFindings()
	x := fnd(1, "unrecorded_bank_charge", "open")
	x.AmountPaise = paise(12345678)
	x.Verified = true
	x = ff.put(x)
	ff.put(fnd(2, "missing_accrual", "open")) // amount nil
	ts := findingsServer(t, ff)

	body := page(t, ts, RoleViewer, x.ID)
	wantAll(t, body, "unrecorded_bank_charge", "high", "₹1,23,456.78", "open", `badge verified">verified<`,
		`href="/runs/`+runID(1).String()+`/findings"`, "acme 2026-09", "no explanation yet")
	last := -1
	for _, id := range []string{`id="header"`, `id="explanation"`, `id="citations"`, `id="evidence"`, `id="actions"`} {
		i := strings.Index(body, id)
		if i < last || i < 0 {
			t.Errorf("section %s out of order or missing", id)
		}
		last = i
	}
	wantNone(t, body, `id="review"`, `id="trail"`, `id="proposal"`)

	if b := page(t, ts, RoleViewer, fid(2)); !strings.Contains(b, `<dd class="amount"></dd>`) {
		t.Error("nil amount should render empty")
	}

	bad := ts.get(t, RoleViewer, "/findings/not-a-uuid")
	unk := ts.get(t, RoleViewer, "/findings/"+fid(99).String())
	if bad.Code != 404 || unk.Code != 404 || bad.Body.String() != unk.Body.String() {
		t.Errorf("404s differ: %d %q vs %d %q", bad.Code, bad.Body, unk.Code, unk.Body)
	}
}

// typeFixtures: one finding type with the tool and record its evidence uses.
var typeFixtures = []struct {
	typ, server, tool string
	rec               map[string]any
	idKey             string
	link              string
}{
	{"unrecorded_bank_charge", "evidence", "list_bank_lines", map[string]any{"txn_id": "BT-1", "narration": "NEFT fee", "amount": -35400}, "txn_id", ""},
	{"unmatched_bank_line", "evidence", "list_bank_lines", map[string]any{"txn_id": "BT-2", "narration": "unknown", "amount": 500000}, "txn_id", ""},
	{"unmatched_ledger_entry", "books", "list_gl_entries", map[string]any{"name": "GL-3", "voucher_type": "Journal Entry", "voucher_no": "JV-3", "debit": 700}, "name", "/app/journal-entry/JV-3"},
	{"duplicate_vendor_payment", "books", "list_payments", map[string]any{"name": "PE-4", "party": "Acme Supplies", "amount": 99900}, "name", "/app/payment-entry/PE-4"},
	{"gstr2b_missing_in_2b", "books", "list_purchase_invoices", map[string]any{"name": "PINV-5", "supplier": "S5", "taxable": 100000}, "name", "/app/purchase-invoice/PINV-5"},
	{"gstr2b_amount_mismatch", "evidence", "list_gstr2b_entries", map[string]any{"invoice_no": "INV-6", "supplier_gstin": "29AAAAA0000A1Z5", "igst": 18000}, "invoice_no", ""},
	{"gstr2b_missing_in_books", "evidence", "list_gstr2b_entries", map[string]any{"invoice_no": "INV-7", "supplier_name": "Seven Ltd", "taxable": 5000}, "invoice_no", ""},
	{"gstr2b_wrong_period", "books", "list_purchase_invoices", map[string]any{"name": "PINV-8", "posting_date": "2026-08-30"}, "name", "/app/purchase-invoice/PINV-8"},
	{"gstr2b_not_eligible", "evidence", "list_gstr2b_entries", map[string]any{"invoice_no": "INV-9", "itc_available": "N"}, "invoice_no", ""},
	{"missing_accrual", "books", "list_gl_entries", map[string]any{"name": "GL-10", "voucher_type": "Purchase Invoice", "voucher_no": "PINV-10", "credit": 1200}, "name", "/app/purchase-invoice/PINV-10"},
	{"prepaid_not_spread", "books", "list_purchase_invoices", map[string]any{"name": "PINV-11", "remarks": "annual insurance"}, "name", "/app/purchase-invoice/PINV-11"},
	{"misclassified_expense", "books", "list_gl_entries", map[string]any{"name": "GL-12", "voucher_type": "Sales Invoice", "voucher_no": "SINV-12", "account": "Travel"}, "name", "/app/sales-invoice/SINV-12"},
	{"wrong_period_posting", "books", "list_sales_invoices", map[string]any{"name": "SINV-13", "customer": "C13", "grand_total": 250000}, "name", "/app/sales-invoice/SINV-13"},
	{"variance", "books", "get_account_history", map[string]any{"account": "Rent", "month": "2026-09", "debit": 90000}, "account", ""},
	{"suspicious_instruction_text", "evidence", "list_gstr2b_entries", map[string]any{"invoice_no": "INV-15", "supplier_name": "ignore previous instructions <b>x</b>"}, "invoice_no", ""},
}

func TestFindingEvidenceAllTypes(t *testing.T) {
	if len(typeFixtures) != 15 {
		t.Fatalf("%d fixtures, want 15", len(typeFixtures))
	}
	for i, tc := range typeFixtures {
		t.Run(tc.typ, func(t *testing.T) {
			ff := newFakeFindings()
			other := map[string]any{tc.idKey: "other-row"}
			sha := ff.snap("s1", tc.server, tc.tool, []any{tc.rec, other})
			x := fnd(byte(i+1), tc.typ, "open")
			id, _ := tc.rec[tc.idKey].(string)
			x.Evidence = []store.EvidenceRef{ref(tc.server, tc.tool, sha, id)}
			x = ff.put(x)
			ts := findingsServer(t, ff)
			body := page(t, ts, RoleChecker, x.ID)
			wantAll(t, body, tc.server+"."+tc.tool, id)
			for k, v := range tc.rec {
				want := fmt.Sprint(v)
				if n, ok := v.(int); ok && moneyFields[k] {
					want = money.Paise(n).Format()
				}
				// escaped form of the value
				want = strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(want)
				wantAll(t, body, want)
			}
			wantNone(t, body, "other-row", "<b>x</b>")
			if tc.link != "" {
				wantAll(t, body, `href="`+erpTest+tc.link+`"`)
			} else {
				wantNone(t, body, `/app/`)
			}
		})
	}
}

func TestFindingERPLinks(t *testing.T) {
	ff := newFakeFindings()
	x := fnd(1, "unmatched_ledger_entry", "open")
	pe := "JE-0001"
	x.Proposal = &store.JournalProposal{Status: "posted", ERPName: &pe}
	x.Evidence = []store.EvidenceRef{
		ref("books", "list_purchase_invoices", ff.snap("a", "books", "list_purchase_invoices", []any{map[string]any{"name": "PI/1 ?x"}})),
		ref("books", "list_gl_entries", ff.snap("b", "books", "list_gl_entries", []any{map[string]any{"name": "GLNAME", "voucher_type": "Payment Entry", "voucher_no": "PE-7"}})),
		ref("evidence", "list_bank_lines", ff.snap("c", "evidence", "list_bank_lines", []any{map[string]any{"txn_id": "T1"}})),
		ref("evidence", "list_gstr2b_entries", ff.snap("d", "evidence", "list_gstr2b_entries", []any{map[string]any{"invoice_no": "I1"}})),
	}
	x = ff.put(x)
	body := page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantAll(t, body, `href="`+erpTest+`/app/purchase-invoice/PI%2F1%20%3Fx"`, `href="`+erpTest+`/app/payment-entry/PE-7"`,
		`href="`+erpTest+`/app/journal-entry/JE-0001"`, `rel="noopener noreferrer" target="_blank"`)
	wantNone(t, body, "GLNAME</a>", "/app/gl-entry", "/app/bank")
	if n := strings.Count(body, `target="_blank"`); n != 3 {
		t.Errorf("%d links, want 3 (bank and GSTR-2B rows have none)", n)
	}

	for _, base := range []string{"", "ftp://erp", "http://", "not a url", "javascript:alert(1)"} {
		ts := findingsServer(t, ff, func(o *Options) { o.ERPBaseURL = base })
		b := page(t, ts, RoleViewer, x.ID)
		wantNone(t, b, "<a href=\"http", `target="_blank"`, "javascript:")
		wantAll(t, b, "PI/1 ?x", "PE-7", "JE-0001")
	}
}

func TestFindingNeedsReview(t *testing.T) {
	mk := func(n byte, status string, verified bool) (*fakeFindings, store.Finding) {
		ff := newFakeFindings()
		x := fnd(n, "variance", status)
		x.Verified = verified
		ex := "an explanation"
		x.Explanation = &ex
		return ff, ff.put(x)
	}
	step := func(f store.Finding, status string, refs ...string) store.Step {
		return store.Step{RunID: f.RunID, Kind: store.StepKindVerify, Subject: f.ID.String(), Status: status, OutputRefs: refs}
	}

	ff, x := mk(1, "needs_review", false)
	vd := ff.art("v1", store.ArtifactVerdict, map[string]any{"pass": false, "violations": []any{
		map[string]any{"code": "amount_not_in_evidence", "field": "explanation", "value_paise": 123456, "detail": "₹1,234.56 is not <b>cited</b>"},
	}})
	ff.steps[x.RunID] = []store.Step{step(x, store.StepDone, "other", vd)}
	body := page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantAll(t, body, `id="review"`, "amount_not_in_evidence", "explanation", "₹1,234.56 is not &lt;b&gt;cited&lt;/b&gt;", "₹1,234.56")
	wantNone(t, body, "<b>cited</b>")

	ff, x = mk(2, "needs_review", false)
	body = page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantAll(t, body, `id="review"`, "the verifier recorded no reasons")

	ff, x = mk(3, "needs_review", false)
	ff.steps[x.RunID] = []store.Step{step(x, store.StepSkipped)}
	body = page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantAll(t, body, `id="review"`, "not verified: step skipped")

	ff, x = mk(4, "open", false) // unverified with an explanation
	body = page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantAll(t, body, `id="review"`)

	ff, x = mk(5, "open", true)
	body = page(t, findingsServer(t, ff), RoleViewer, x.ID)
	wantNone(t, body, `id="review"`)
}

func TestFindingCitations(t *testing.T) {
	ff := newFakeFindings()
	x := fnd(1, "variance", "open")
	x.Citations = []store.Citation{{DocID: "policy-1", Section: "§3 <i>x</i>"}, {DocID: "doc<2>", Section: "A"}}
	x = ff.put(x)
	body := page(t, findingsServer(t, ff), RoleViewer, x.ID)
	if n := strings.Count(body, "<details>"); n != 2 {
		t.Errorf("%d details, want 2", n)
	}
	wantAll(t, body, "<summary>policy-1, §3 &lt;i&gt;x&lt;/i&gt;</summary>", "doc&lt;2&gt;", "snippet not stored on the finding")
	wantNone(t, body, "<i>x</i>")
}

func TestFindingTrail(t *testing.T) {
	ff := newFakeFindings()
	x := fnd(1, "variance", "open")
	x.Keys = map[string]string{"resolution": "explained"}
	x.Evidence = []store.EvidenceRef{
		ref("books", "get_account_history", ff.snap("a", "books", "get_account_history", []any{map[string]any{"month": "2026-08"}, map[string]any{"month": "2026-09"}})),
		ref("books", "list_recurring_suppliers", ""),
	}
	x = ff.put(x)
	y := ff.put(func() store.Finding {
		y := fnd(2, "variance", "open")
		y.Evidence = x.Evidence
		return y
	}())
	ts := findingsServer(t, ff)
	body := page(t, ts, RoleViewer, x.ID)
	wantAll(t, body, `id="trail"`, `<code>books.get_account_history</code> <code>{&#34;company&#34;:&#34;acme&#34;}</code> rows: 2`,
		"<code>books.list_recurring_suppliers</code>", "rows: n/a")
	if strings.Index(body, "books.get_account_history</code>") > strings.Index(body, "books.list_recurring_suppliers</code>") {
		t.Error("trail not in call order")
	}
	wantAll(t, body, `id="evidence"`, "snapshot not stored")
	wantNone(t, page(t, ts, RoleViewer, y.ID), `id="trail"`)
}

func TestFindingProposalPreview(t *testing.T) {
	prop := func(status string, dr, cr int64, erp *string) *store.JournalProposal {
		return &store.JournalProposal{Status: status, Maker: "asha", ERPName: erp, Payload: store.JournalPayload{
			PostingDate: "2026-09-30", Remark: "bank <b>charge</b>",
			Lines: []store.JournalLine{{Account: "Bank Charges", DebitPaise: money.Paise(dr)}, {Account: "HDFC Bank", CreditPaise: money.Paise(cr)}},
		}}
	}
	je := "JE-9"
	ff := newFakeFindings()
	a := fnd(1, "variance", "open")
	a.Proposal = prop("proposed", 12345678, 12345678, nil)
	b := fnd(2, "variance", "open")
	b.Proposal = prop("proposed", 10000, 7550, nil)
	c := fnd(3, "variance", "open")
	d := fnd(4, "variance", "open")
	d.Proposal = prop("posted", 100, 100, &je)
	for _, x := range []store.Finding{a, b, c, d} {
		ff.put(x)
	}
	ts := findingsServer(t, ff)

	body := page(t, ts, RoleViewer, a.ID)
	wantAll(t, body, `id="proposal"`, "₹1,23,456.78", "Bank Charges", "HDFC Bank", "Balanced", "bank &lt;b&gt;charge&lt;/b&gt;", "2026-09-30")
	if n := strings.Count(body, "₹1,23,456.78"); n < 4 { // two lines, two totals, plus the header amount absent
		t.Errorf("amount appears %d times", n)
	}
	wantAll(t, page(t, ts, RoleViewer, b.ID), "Unbalanced: difference ₹24.50")
	wantNone(t, page(t, ts, RoleViewer, c.ID), `id="proposal"`)
	wantAll(t, page(t, ts, RoleViewer, d.ID), `href="`+erpTest+`/app/journal-entry/JE-9"`)
}

func formPost(ts *testServer, role Role, path string, fields url.Values) req {
	ck, csrf := ts.session(role)
	if fields == nil {
		fields = url.Values{}
	}
	fields.Set("csrf_token", csrf)
	return req{method: http.MethodPost, path: path, body: fields.Encode(),
		contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}}
}

func TestFindingActions(t *testing.T) {
	newOne := func(status string, withProposal bool) (*fakeFindings, store.Finding, *testServer) {
		ff := newFakeFindings()
		x := fnd(1, "variance", status)
		if withProposal {
			x.Proposal = &store.JournalProposal{Status: "proposed"}
		}
		x = ff.put(x)
		return ff, x, findingsServer(t, ff)
	}
	ff, x, ts := newOne("open", true)
	body := page(t, ts, RoleMaker, x.ID)
	wantAll(t, body, "Mark accepted", ">Dismiss<", `name="reason"`, "Create proposal (available with approvals)", "disabled")
	for _, r := range []Role{RoleViewer, RoleChecker} {
		wantNone(t, page(t, ts, r, x.ID), "Mark accepted", ">Dismiss<", "Create proposal", `name="reason"`)
	}
	_, y, ts2 := newOne("open", false)
	wantNone(t, page(t, ts2, RoleMaker, y.ID), "Create proposal")
	_, z, ts3 := newOne("open", false)
	wantAll(t, page(t, ts3, RoleMaker, z.ID), "Mark accepted")

	// Accept.
	w := ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/accept", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/findings/"+x.ID.String() {
		t.Fatalf("accept = %d %s", w.Code, w.Header().Get("Location"))
	}
	if ff.status(x.ID) != "accepted" || ff.auditCount() != 1 {
		t.Fatalf("status %s audits %d", ff.status(x.ID), ff.auditCount())
	}
	e := ff.audits[0]
	var args map[string]string
	_ = json.Unmarshal(e.Args, &args)
	if e.Actor != "asha" || e.Tool == nil || *e.Tool != "finding.accept" || args["from"] != "open" || args["to"] != "accepted" ||
		args["finding_id"] != x.ID.String() || args["run_id"] != x.RunID.String() {
		t.Errorf("audit %+v args %v", e, args)
	}
	// Already decided: both actions refuse with 409 and no buttons.
	for _, act := range []string{"accept", "dismiss"} {
		w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/"+act, url.Values{"reason": {"why"}}))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "accepted") || strings.Contains(w.Body.String(), "Mark accepted") {
			t.Errorf("%s on accepted = %d", act, w.Code)
		}
	}
	wantNone(t, page(t, ts, RoleMaker, x.ID), "Mark accepted", ">Dismiss<")

	// Dismiss from needs_review.
	ff, x, ts = newOne("needs_review", false)
	w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/dismiss", url.Values{"reason": {" not a real issue \r\nsecond line "}}))
	if w.Code != http.StatusSeeOther || ff.status(x.ID) != "dismissed" || ff.auditCount() != 1 {
		t.Fatalf("dismiss = %d status %s audits %d", w.Code, ff.status(x.ID), ff.auditCount())
	}
	args = nil
	_ = json.Unmarshal(ff.audits[0].Args, &args)
	if *ff.audits[0].Tool != "finding.dismiss" || args["reason"] != "not a real issue \nsecond line" || args["to"] != "dismissed" {
		t.Errorf("dismiss audit %v", args)
	}
	w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/dismiss", url.Values{"reason": {"again"}}))
	if w.Code != http.StatusConflict {
		t.Errorf("second dismiss = %d", w.Code)
	}

	// Store error: 500, no audit row.
	ff, x, ts = newOne("open", false)
	ff.setErr = errors.New("db down")
	w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/accept", nil))
	if w.Code != http.StatusInternalServerError || ff.auditCount() != 0 || ff.status(x.ID) != "open" {
		t.Errorf("store error = %d audits %d status %s", w.Code, ff.auditCount(), ff.status(x.ID))
	}
	// Audit error: still redirects.
	ff, x, ts = newOne("open", false)
	ff.auditErr = errors.New("audit down")
	w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/accept", nil))
	if w.Code != http.StatusSeeOther || ff.status(x.ID) != "accepted" {
		t.Errorf("audit error = %d status %s", w.Code, ff.status(x.ID))
	}
	if !strings.Contains(ts.logs.String(), "audit row not written") {
		t.Error("audit failure not logged")
	}
}

func TestFindingDismissReason(t *testing.T) {
	secret := "SECRET-REASON-TEXT"
	tests := []struct {
		name, reason string
		ok           bool
	}{
		{"empty", "", false},
		{"whitespace", " \t\n ", false},
		{"too long", strings.Repeat("é", 501), false},
		{"control", secret + "\x07", false},
		{"nul", secret + "\x00", false},
		{"max length", strings.Repeat("é", 500), true},
		{"newline ok", secret + "\nok", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ff := newFakeFindings()
			x := ff.put(fnd(1, "variance", "open"))
			ts := findingsServer(t, ff)
			w := ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/dismiss", url.Values{"reason": {tc.reason}}))
			if tc.ok {
				if w.Code != http.StatusSeeOther || ff.status(x.ID) != "dismissed" {
					t.Errorf("= %d %s", w.Code, ff.status(x.ID))
				}
			} else if w.Code != http.StatusBadRequest || ff.status(x.ID) != "open" || ff.auditCount() != 0 || !strings.Contains(w.Body.String(), `role="alert"`) {
				t.Errorf("= %d status %s audits %d", w.Code, ff.status(x.ID), ff.auditCount())
			}
			if strings.Contains(ts.logs.String(), secret) {
				t.Error("reason text is in the logs")
			}
		})
	}
}

func TestFindingRoles(t *testing.T) {
	ff := newFakeFindings()
	x := ff.put(fnd(1, "variance", "open"))
	ts := findingsServer(t, ff)
	path := "/findings/" + x.ID.String()

	w := ts.do(t, req{method: http.MethodGet, path: path})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous GET = %d %s", w.Code, w.Header().Get("Location"))
	}
	w = ts.do(t, req{method: http.MethodPost, path: path + "/accept"})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous POST = %d", w.Code)
	}
	for _, r := range []Role{RoleViewer, RoleChecker, RoleMaker} {
		if c := ts.get(t, r, path).Code; c != http.StatusOK {
			t.Errorf("%s GET = %d", r, c)
		}
	}
	for _, r := range []Role{RoleViewer, RoleChecker} {
		for _, act := range []string{"accept", "dismiss"} {
			if c := ts.do(t, formPost(ts, r, path+"/"+act, url.Values{"reason": {"x"}})).Code; c != http.StatusForbidden {
				t.Errorf("%s POST %s = %d", r, act, c)
			}
		}
	}
	ck, _ := ts.session(RoleMaker)
	w = ts.do(t, req{method: http.MethodPost, path: path + "/accept", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusForbidden || ff.status(x.ID) != "open" {
		t.Errorf("no CSRF = %d status %s", w.Code, ff.status(x.ID))
	}
}

func TestFindingNotConfigured(t *testing.T) {
	id := fid(1).String()
	for name, mod := range map[string]func(*Options){
		"nil findings": func(o *Options) {
			o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		},
		"nil runs": func(o *Options) { o.Runs = nil },
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, mod)
			if c := ts.get(t, RoleViewer, "/findings/"+id).Code; c != http.StatusNotImplemented {
				t.Errorf("GET = %d", c)
			}
			for _, act := range []string{"accept", "dismiss"} {
				if c := ts.do(t, formPost(ts, RoleMaker, "/findings/"+id+"/"+act, url.Values{"reason": {"x"}})).Code; c != http.StatusNotImplemented {
					t.Errorf("POST %s = %d", act, c)
				}
				if c := ts.do(t, formPost(ts, RoleViewer, "/findings/"+id+"/"+act, nil)).Code; c != http.StatusForbidden {
					t.Errorf("viewer POST %s = %d (role check comes first)", act, c)
				}
			}
		})
	}
}

func TestFindingNotFound(t *testing.T) {
	ff := newFakeFindings()
	orphan := fnd(1, "variance", "open")
	orphan.RunID = runID(77)
	ff.findings[orphan.ID] = orphan
	ts := findingsServer(t, ff)
	for name, id := range map[string]string{"bad uuid": "zzz", "unknown": fid(9).String(), "missing run": orphan.ID.String()} {
		if c := ts.get(t, RoleViewer, "/findings/"+id).Code; c != http.StatusNotFound {
			t.Errorf("%s GET = %d", name, c)
		}
		for _, act := range []string{"accept", "dismiss"} {
			if c := ts.do(t, formPost(ts, RoleMaker, "/findings/"+id+"/"+act, url.Values{"reason": {"x"}})).Code; c != http.StatusNotFound {
				t.Errorf("%s POST %s = %d", name, act, c)
			}
		}
	}
	if !strings.Contains(ts.logs.String(), "finding has no run") {
		t.Error("run-less finding not logged")
	}
}

func TestFindingEscaping(t *testing.T) {
	ff := newFakeFindings()
	evil := `<script>alert(1)</script> **bold** [x](javascript:alert(1))`
	x := fnd(1, "suspicious_instruction_text", "needs_review")
	x.Title = evil
	x.Explanation, x.Action = &evil, &evil
	x.Citations = []store.Citation{{DocID: evil, Section: evil}}
	x.Evidence = []store.EvidenceRef{{Server: "evidence", Tool: "list_gstr2b_entries", Args: json.RawMessage(`{"q":"<script>alert(1)</script>"}`),
		Artifact: ff.snap("e", "evidence", "list_gstr2b_entries", []any{map[string]any{"invoice_no": "I1", "supplier_name": evil}})}}
	x.Keys = map[string]string{"resolution": "r"}
	x = ff.put(x)
	vd := ff.art("v", store.ArtifactVerdict, map[string]any{"violations": []any{map[string]any{"code": "c", "field": "f", "detail": evil}}})
	ff.steps[x.RunID] = []store.Step{{RunID: x.RunID, Kind: store.StepKindVerify, Subject: x.ID.String(), Status: store.StepDone, OutputRefs: []string{vd}}}
	ts := findingsServer(t, ff)

	body := page(t, ts, RoleMaker, x.ID)
	wantNone(t, body, "<script>", `href="javascript:`, `src="javascript:`, "<b>")
	if n := strings.Count(body, "<script"); n != 1 { // only the layout's htmx tag
		t.Errorf("%d <script tags, want 1", n)
	}
	wantAll(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", "**bold**")

	w := ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/dismiss", url.Values{"reason": {evil + "\x07"}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dismiss = %d", w.Code)
	}
	wantNone(t, w.Body.String(), "<script>", `href="javascript:`)
}
