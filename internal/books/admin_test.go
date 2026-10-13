package books

// Unit tests for post_approved_journal_entry (CC-504b): a fake proposal
// store, a fake ERPNext that accepts writes, and a real MCP client
// session. No Docker, no ERPNext. (Names avoid the TestPostApproved and
// TestProposalLock prefixes, which the acceptance greps reserve for the
// integration tests.)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const (
	adminCompanyID = "test"
	adminMaker     = "maker@example.test"
	adminChecker   = "checker@example.test"
	acctCharges    = "Bank Charges - TT"
	acctBank       = "HDFC Current - TT"
)

var adminNow = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// ---- fake proposal store ----

type fakeProposals struct {
	lock     sync.Mutex // the row lock, held across fn like FOR UPDATE
	mu       sync.Mutex // guards the fields below
	rows     map[uuid.UUID]store.JournalProposal
	locks    int
	saves    int
	failSave int     // fail this many saves (the tx rolls back)
	saveErrs []error // ctx.Err() at each save
}

func newFakeProposals(ps ...store.JournalProposal) *fakeProposals {
	f := &fakeProposals{rows: map[uuid.UUID]store.JournalProposal{}}
	for _, p := range ps {
		f.rows[p.ID] = p
	}
	return f
}

func (f *fakeProposals) get(id uuid.UUID) store.JournalProposal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

func (f *fakeProposals) counts() (locks, saves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locks, f.saves
}

func (f *fakeProposals) WithProposalLock(ctx context.Context, id uuid.UUID, fn func(context.Context, store.JournalProposal, func(context.Context, store.JournalProposal) error) error) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.mu.Lock()
	f.locks++
	p, ok := f.rows[id]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: journal proposal %s", store.ErrNotFound, id)
	}
	var pending *store.JournalProposal
	save := func(sctx context.Context, np store.JournalProposal) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.saveErrs = append(f.saveErrs, sctx.Err())
		if f.failSave > 0 {
			f.failSave--
			return errors.New("fake store: save failed")
		}
		f.saves++
		pending = &np
		return nil
	}
	if err := fn(ctx, p, save); err != nil {
		return err // rolled back: pending is dropped
	}
	if pending != nil {
		f.mu.Lock()
		f.rows[id] = *pending
		f.mu.Unlock()
	}
	return nil
}

// ---- fake ERPNext that accepts Journal Entry writes ----

type adminERP struct {
	t     *testing.T
	mu    sync.Mutex
	docs  []doc
	seq   int
	delay time.Duration

	requests       int
	inserts        int
	submitAttempts int
	submits        int
	bodies         []doc

	failSubmit int       // fail this many submits with HTTP 500
	onSubmit   func(doc) // may tamper with the document as it is submitted
	forbid     bool      // answer every request with 403
}

func newAdminERP(t *testing.T) *adminERP { return &adminERP{t: t} }

func (f *adminERP) client() *frappe.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "botkey", config.NewSecret("botsecret"))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *adminERP) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserts + f.submitAttempts
}

func (f *adminERP) stats() (requests, inserts, submits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.inserts, f.submits
}

func (f *adminERP) add(d doc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, d)
}

func (f *adminERP) find(name string) doc {
	for _, d := range f.docs {
		if d["name"] == name {
			return d
		}
	}
	return nil
}

func (f *adminERP) byExtID(id string) []doc {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []doc
	for _, d := range f.docs {
		if d[seed.ExtIDField] == id {
			out = append(out, d)
		}
	}
	return out
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	return dec.Decode(v)
}

// copyDoc deep-copies a document through JSON, keeping numbers exact.
func copyDoc(t *testing.T, d doc) doc {
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var out doc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *adminERP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	delay, forbid := f.delay, f.forbid
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if forbid {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(f.t, w, map[string]any{"exc_type": "PermissionError"})
		return
	}
	const res = "/api/resource/Journal Entry"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == res:
		f.list(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, res+"/"):
		f.mu.Lock()
		d := f.find(strings.TrimPrefix(r.URL.Path, res+"/"))
		var out doc
		if d != nil {
			out = copyDoc(f.t, d)
		}
		f.mu.Unlock()
		if out == nil {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(f.t, w, map[string]any{"exc_type": "DoesNotExistError"})
			return
		}
		writeJSON(f.t, w, map[string]any{"data": out})
	case r.Method == http.MethodPost && r.URL.Path == res:
		var body doc
		if err := decodeBody(r, &body); err != nil {
			f.t.Errorf("fake ERPNext: insert body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.inserts++
		f.seq++
		f.bodies = append(f.bodies, copyDoc(f.t, body))
		body["name"] = fmt.Sprintf("ACC-JV-2026-%05d", f.seq)
		body["docstatus"] = json.Number("0")
		rows, _ := body["accounts"].([]any)
		for _, r := range rows {
			row := r.(map[string]any)
			row["debit"], row["credit"] = row["debit_in_account_currency"], row["credit_in_account_currency"]
		}
		f.docs = append(f.docs, body)
		out := copyDoc(f.t, body)
		f.mu.Unlock()
		writeJSON(f.t, w, map[string]any{"data": out})
	case r.Method == http.MethodPost && r.URL.Path == "/api/method/frappe.client.submit":
		var body struct {
			Doc doc `json:"doc"`
		}
		if err := decodeBody(r, &body); err != nil {
			f.t.Errorf("fake ERPNext: submit body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.submitAttempts++
		if f.failSubmit > 0 {
			f.failSubmit--
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(f.t, w, map[string]any{"exc_type": "OperationalError"})
			return
		}
		name, _ := body.Doc["name"].(string)
		d := f.find(name)
		if d == nil || d["docstatus"] != json.Number("0") {
			f.mu.Unlock()
			w.WriteHeader(http.StatusExpectationFailed)
			writeJSON(f.t, w, map[string]any{"exc_type": "ValidationError"})
			return
		}
		d["docstatus"] = json.Number("1")
		f.submits++
		if f.onSubmit != nil {
			f.onSubmit(d)
		}
		out := copyDoc(f.t, d)
		f.mu.Unlock()
		writeJSON(f.t, w, map[string]any{"message": out})
	default:
		f.t.Errorf("fake ERPNext: unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *adminERP) list(w http.ResponseWriter, r *http.Request) {
	q, err := parseList(r.URL.Query())
	if err != nil {
		f.t.Errorf("fake ERPNext: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(q.filters) != 1 {
		f.t.Errorf("fake ERPNext: want one copilot_ext_id filter, got %v", q.filters)
	}
	f.mu.Lock()
	var rows []doc
	for _, d := range f.docs {
		if matchAll(f.t, d, q.filters) {
			rows = append(rows, doc{"name": d["name"], "docstatus": d["docstatus"]})
		}
	}
	f.mu.Unlock()
	if q.start >= len(rows) {
		rows = nil
	} else {
		rows = rows[q.start:min(len(rows), q.start+q.length)]
	}
	if rows == nil {
		rows = []doc{}
	}
	writeJSON(f.t, w, map[string]any{"data": rows})
}

// jeDoc is a Journal Entry as ERPNext would hold it for p.
func jeDoc(name string, docstatus int, p store.JournalProposal) doc {
	rows := make([]any, len(p.Payload.Lines))
	for i, l := range p.Payload.Lines {
		rows[i] = map[string]any{
			"account":                    l.Account,
			"debit_in_account_currency":  json.Number(l.DebitPaise.Rupees()),
			"credit_in_account_currency": json.Number(l.CreditPaise.Rupees()),
			"debit":                      json.Number(l.DebitPaise.Rupees()),
			"credit":                     json.Number(l.CreditPaise.Rupees()),
		}
	}
	return doc{
		"name": name, "docstatus": json.Number(strconv.Itoa(docstatus)), "voucher_type": "Journal Entry",
		"company": testCompany, "posting_date": p.Payload.PostingDate, seed.ExtIDField: p.ID.String(),
		"accounts": rows,
	}
}

// ---- fixtures and session ----

func approvedProposal() store.JournalProposal {
	checker := adminChecker
	decided := adminNow.Add(-time.Hour)
	return store.JournalProposal{
		ID:        uuid.New(),
		CompanyID: adminCompanyID,
		Payload: store.JournalPayload{
			PostingDate: "2026-09-30",
			Remark:      "Unrecorded bank charges, September",
			Lines: []store.JournalLine{
				{Account: acctCharges, DebitPaise: 11800},
				{Account: acctBank, CreditPaise: 11800},
			},
		},
		Status:    ProposalApproved,
		Maker:     adminMaker,
		Checker:   &checker,
		DecidedAt: &decided,
	}
}

type auditRows struct {
	mu   sync.Mutex
	rows []store.AuditLogEntry
}

func (a *auditRows) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return int64(len(a.rows)), nil
}

func (a *auditRows) all() []store.AuditLogEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rows)
}

type adminFixture struct {
	erp   *adminERP
	ps    *fakeProposals
	audit *auditRows
	logs  *bytes.Buffer
	a     *admin
	cs    *mcp.ClientSession
}

type syncBuffer struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func newAdminFixture(t *testing.T, ps ...store.JournalProposal) *adminFixture {
	t.Helper()
	fx := &adminFixture{erp: newAdminERP(t), ps: newFakeProposals(ps...), audit: &auditRows{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(&syncBuffer{buf: fx.logs}, nil))
	fx.a = &admin{
		deps: AdminDeps{
			Client:    fx.erp.client(),
			Companies: ProfileCompanies([]seed.Profile{{ID: adminCompanyID, ERPCompany: testCompany}}),
			Proposals: fx.ps,
			Now:       func() time.Time { return adminNow },
			Logger:    log,
		},
		timeout: AdminToolTimeout,
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "books-admin-test", Version: "0"}, nil)
	mcpkit.EnableAudit(s, "books-admin", fx.audit)
	registerAdmin(s, fx.a)
	fx.cs = connect(t, s)
	return fx
}

// post calls the tool and returns the result, its text and its output.
func (fx *adminFixture) post(t *testing.T, args map[string]any) (*mcp.CallToolResult, string, PostApprovedOutput) {
	t.Helper()
	res, err := fx.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolPostApprovedJournalEntry, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	var out PostApprovedOutput
	if !res.IsError && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode output: %v", err)
		}
	}
	return res, text, out
}

func (fx *adminFixture) postID(t *testing.T, id uuid.UUID) (*mcp.CallToolResult, string, PostApprovedOutput) {
	t.Helper()
	return fx.post(t, map[string]any{"proposal_id": id.String()})
}

func wantRefusal(t *testing.T, res *mcp.CallToolResult, text, code string) {
	t.Helper()
	if !res.IsError || !strings.HasPrefix(text, code+":") {
		t.Errorf("IsError=%v %q, want a %s refusal", res.IsError, text, code)
	}
	for _, leak := range []string{acctCharges, acctBank, "118.00", "11800", "botsecret"} {
		if strings.Contains(text, leak) {
			t.Errorf("refusal %q carries %q", text, leak)
		}
	}
}

// ---- tests ----

func TestAdminRefusesBeforeERPNext(t *testing.T) {
	mut := func(f func(*store.JournalProposal)) store.JournalProposal {
		p := approvedProposal()
		f(&p)
		return p
	}
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		p    store.JournalProposal
		code string
	}{
		{"proposed", mut(func(p *store.JournalProposal) { p.Status = "proposed" }), CodeNotApproved},
		{"rejected", mut(func(p *store.JournalProposal) { p.Status = "rejected" }), CodeNotApproved},
		{"posted without erp_name", mut(func(p *store.JournalProposal) { p.Status = ProposalPosted }), CodeNotApproved},
		{"erp_name on a rejected row", mut(func(p *store.JournalProposal) { p.Status, p.ERPName = "rejected", str("ACC-JV-1") }), CodeInvalidState},
		{"erp_name on an approved self-approved row", mut(func(p *store.JournalProposal) { p.ERPName, p.Checker = str("ACC-JV-1"), str(adminMaker) }), CodeSelfApproved},
		{"checker is maker", mut(func(p *store.JournalProposal) { p.Checker = str(adminMaker) }), CodeSelfApproved},
		{"checker is maker, case and spaces", mut(func(p *store.JournalProposal) { p.Checker = str("  Maker@Example.TEST \t") }), CodeSelfApproved},
		{"checker nil", mut(func(p *store.JournalProposal) { p.Checker = nil }), CodeNoChecker},
		{"checker blank", mut(func(p *store.JournalProposal) { p.Checker = str("   ") }), CodeNoChecker},
		{"no decision time", mut(func(p *store.JournalProposal) { p.DecidedAt = nil }), CodeNotDecided},
		{"bad posting date", mut(func(p *store.JournalProposal) { p.Payload.PostingDate = "2026-9-30" }), CodeInvalidDate},
		{"impossible posting date", mut(func(p *store.JournalProposal) { p.Payload.PostingDate = "2026-02-30" }), CodeInvalidDate},
		{"one line", mut(func(p *store.JournalProposal) { p.Payload.Lines = p.Payload.Lines[:1] }), CodeInvalidLines},
		{"no lines", mut(func(p *store.JournalProposal) { p.Payload.Lines = nil }), CodeInvalidLines},
		{"line with debit and credit", mut(func(p *store.JournalProposal) { p.Payload.Lines[0].CreditPaise = 11800 }), CodeInvalidLines},
		{"line with neither", mut(func(p *store.JournalProposal) {
			p.Payload.Lines = append(p.Payload.Lines, store.JournalLine{Account: acctBank})
		}), CodeInvalidLines},
		{"negative debit", mut(func(p *store.JournalProposal) {
			p.Payload.Lines[0].DebitPaise = -11800
			p.Payload.Lines[1] = store.JournalLine{Account: acctBank, DebitPaise: 11800}
		}), CodeInvalidLines},
		{"blank account", mut(func(p *store.JournalProposal) { p.Payload.Lines[0].Account = " " }), CodeInvalidLines},
		{"unbalanced", mut(func(p *store.JournalProposal) { p.Payload.Lines[1].CreditPaise = 11799 }), CodeUnbalanced},
		{"one-sided", mut(func(p *store.JournalProposal) {
			p.Payload.Lines[1] = store.JournalLine{Account: acctBank, DebitPaise: 11800}
		}), CodeUnbalanced},
		{"overflow", mut(func(p *store.JournalProposal) {
			p.Payload.Lines = []store.JournalLine{
				{Account: acctCharges, DebitPaise: math.MaxInt64}, {Account: acctCharges, DebitPaise: math.MaxInt64},
				{Account: acctBank, CreditPaise: 2},
			}
		}), CodeUnbalanced},
		{"unknown company", mut(func(p *store.JournalProposal) { p.CompanyID = "nobody" }), CodeUnknownCompany},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAdminFixture(t, tc.p)
			res, text, _ := fx.postID(t, tc.p.ID)
			wantRefusal(t, res, text, tc.code)
			if n, _, _ := fx.erp.stats(); n != 0 {
				t.Errorf("%d ERPNext requests, want none", n)
			}
			if _, saves := fx.ps.counts(); saves != 0 {
				t.Errorf("%d saves, want none", saves)
			}
			if got := fx.ps.get(tc.p.ID); got.Status != tc.p.Status || !slices.Equal(got.Payload.Lines, tc.p.Payload.Lines) {
				t.Errorf("row changed: %+v", got)
			}
		})
	}
}

func TestAdminInvalidProposalIDTakesNoLock(t *testing.T) {
	fx := newAdminFixture(t)
	for _, id := range []string{"", "not-a-uuid", strings.ToUpper(uuid.NewString()), "{" + uuid.NewString() + "}",
		"urn:uuid:" + uuid.NewString(), uuid.Nil.String(), "x') or 1=1 --"} {
		res, text, _ := fx.post(t, map[string]any{"proposal_id": id})
		wantRefusal(t, res, text, CodeInvalidProposalID)
	}
	if locks, _ := fx.ps.counts(); locks != 0 {
		t.Errorf("%d locks taken, want none", locks)
	}
	if n, _, _ := fx.erp.stats(); n != 0 {
		t.Errorf("%d ERPNext requests, want none", n)
	}
	// Refusals before the row is read keep the route actor.
	for _, r := range fx.audit.all() {
		if r.Actor != "unknown" {
			t.Errorf("audit actor %q, want the route actor", r.Actor)
		}
	}
}

func TestAdminNotFound(t *testing.T) {
	fx := newAdminFixture(t)
	res, text, _ := fx.postID(t, uuid.New())
	wantRefusal(t, res, text, CodeNotFound)
	if n, _, _ := fx.erp.stats(); n != 0 {
		t.Errorf("%d ERPNext requests, want none", n)
	}
}

func TestAdminSchemaRejectsExtraInput(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	for _, extra := range []map[string]any{
		{"proposal_id": p.ID.String(), "lines": []any{map[string]any{"account": acctCharges, "debit": 1}}},
		{"proposal_id": p.ID.String(), "company": "test"},
		{"lines": []any{}},
		{},
	} {
		res, err := fx.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolPostApprovedJournalEntry, Arguments: extra})
		if err == nil && !res.IsError {
			t.Errorf("args %v accepted", extra)
		}
	}
	if locks, _ := fx.ps.counts(); locks != 0 {
		t.Errorf("%d locks taken, want none", locks)
	}
	if n, _, _ := fx.erp.stats(); n != 0 {
		t.Errorf("%d ERPNext requests, want none", n)
	}
}

func TestAdminPostsOnceAndRecords(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)

	res, text, out := fx.postID(t, p.ID)
	if res.IsError {
		t.Fatalf("post: %s", text)
	}
	if out.ERPName == "" || out.AlreadyPosted || out.Status != ProposalPosted || out.ProposalID != p.ID.String() {
		t.Errorf("output %+v", out)
	}
	_, inserts, submits := fx.erp.stats()
	if inserts != 1 || submits != 1 {
		t.Errorf("inserts %d submits %d, want 1 and 1", inserts, submits)
	}
	row := fx.ps.get(p.ID)
	if row.Status != ProposalPosted || row.ERPName == nil || *row.ERPName != out.ERPName || row.PostedAt == nil || !row.PostedAt.Equal(adminNow) {
		t.Errorf("row %+v", row)
	}
	if row.Checker == nil || *row.Checker != adminChecker || row.DecidedAt == nil {
		t.Errorf("checker or decision changed: %+v", row)
	}

	// The body sent to ERPNext comes from the row.
	body := fx.erp.bodies[0]
	if body["voucher_type"] != "Journal Entry" || body["company"] != testCompany || body["posting_date"] != "2026-09-30" ||
		body[seed.ExtIDField] != p.ID.String() || body["user_remark"] != p.Payload.Remark {
		t.Errorf("insert body %v", body)
	}
	rows := body["accounts"].([]any)
	first := rows[0].(map[string]any)
	if len(rows) != 2 || first["account"] != acctCharges || first["debit_in_account_currency"] != json.Number("118.00") ||
		first["credit_in_account_currency"] != json.Number("0.00") {
		t.Errorf("insert rows %v", rows)
	}

	// Again: the same entry, no ERPNext call at all.
	before, _, _ := fx.erp.stats()
	res, text, again := fx.postID(t, p.ID)
	if res.IsError || again.ERPName != out.ERPName || !again.AlreadyPosted {
		t.Errorf("second call: %s %+v", text, again)
	}
	if after, _, _ := fx.erp.stats(); after != before {
		t.Errorf("second call made %d ERPNext requests", after-before)
	}

	// The audit rows name the checker and carry the proposal ID.
	for _, r := range fx.audit.all() {
		if r.Actor != adminChecker || *r.Server != "books-admin" || *r.Tool != ToolPostApprovedJournalEntry ||
			!strings.Contains(string(r.Args), p.ID.String()) || r.Error != nil {
			t.Errorf("audit row: actor %q server %q tool %q args %s err %v", r.Actor, *r.Server, *r.Tool, r.Args, r.Error)
		}
	}
	if n := len(fx.audit.all()); n != 2 {
		t.Errorf("%d audit rows, want 2", n)
	}

	// The log has the pair on record and never the payload.
	logs := fx.logs.String()
	if !strings.Contains(logs, `"proposal_id":"`+p.ID.String()+`"`) || !strings.Contains(logs, `"erp_name":"`+out.ERPName+`"`) {
		t.Errorf("log lacks the proposal_id/erp_name pair: %s", logs)
	}
	for _, leak := range []string{acctCharges, acctBank, "118.00", "11800", p.Payload.Remark, "botsecret"} {
		if strings.Contains(logs, leak) {
			t.Errorf("log carries %q", leak)
		}
	}
}

func TestAdminRepairsApprovedRowWithERPName(t *testing.T) {
	p := approvedProposal()
	name := "ACC-JV-2026-00042"
	p.ERPName = &name
	fx := newAdminFixture(t, p)
	res, text, out := fx.postID(t, p.ID)
	if res.IsError || out.ERPName != name || !out.AlreadyPosted {
		t.Fatalf("repair: %s %+v", text, out)
	}
	if row := fx.ps.get(p.ID); row.Status != ProposalPosted || row.PostedAt == nil {
		t.Errorf("row %+v, want posted", row)
	}
	if n, _, _ := fx.erp.stats(); n != 0 {
		t.Errorf("%d ERPNext requests, want none", n)
	}
}

func TestAdminRetryAfterFailedSubmitReusesDraft(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	fx.erp.failSubmit = 1

	res, text, _ := fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeERPUnavailable)
	if row := fx.ps.get(p.ID); row.Status != ProposalApproved || row.ERPName != nil {
		t.Errorf("row after a failed submit: %+v", row)
	}

	res, text, out := fx.postID(t, p.ID)
	if res.IsError {
		t.Fatalf("retry: %s", text)
	}
	_, inserts, submits := fx.erp.stats()
	if inserts != 1 || submits != 1 {
		t.Errorf("inserts %d submits %d, want the one draft submitted once", inserts, submits)
	}
	if docs := fx.erp.byExtID(p.ID.String()); len(docs) != 1 || docs[0]["name"] != out.ERPName {
		t.Errorf("entries carrying the ID: %v, want one named %s", docs, out.ERPName)
	}
}

func TestAdminRetryAfterFailedSaveRecordsSubmitted(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	fx.ps.failSave = 1

	res, text, _ := fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeStoreUnavailable)
	if row := fx.ps.get(p.ID); row.Status != ProposalApproved || row.ERPName != nil {
		t.Errorf("row after a failed save: %+v", row)
	}
	if !strings.Contains(fx.logs.String(), "submitted but not recorded") {
		t.Error("no error log for a submitted, unrecorded entry")
	}

	res, text, out := fx.postID(t, p.ID)
	if res.IsError || out.ERPName == "" {
		t.Fatalf("retry: %s", text)
	}
	_, inserts, submits := fx.erp.stats()
	if inserts != 1 || submits != 1 {
		t.Errorf("inserts %d submits %d, want 1 and 1", inserts, submits)
	}
	if row := fx.ps.get(p.ID); row.Status != ProposalPosted || row.ERPName == nil || *row.ERPName != out.ERPName {
		t.Errorf("row %+v", row)
	}
}

func TestAdminConcurrentCallsInsertOnce(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	const n = 20
	outs := make([]PostApprovedOutput, n)
	errs := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			res, err := fx.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolPostApprovedJournalEntry,
				Arguments: map[string]any{"proposal_id": p.ID.String()}})
			if err != nil {
				errs[i] = err.Error()
				return
			}
			if res.IsError {
				errs[i] = res.Content[0].(*mcp.TextContent).Text
				return
			}
			b, _ := json.Marshal(res.StructuredContent)
			_ = json.Unmarshal(b, &outs[i])
		})
	}
	wg.Wait()
	fresh := 0
	for i := range n {
		if errs[i] != "" {
			t.Errorf("call %d: %s", i, errs[i])
			continue
		}
		if outs[i].ERPName != outs[0].ERPName || outs[i].ERPName == "" {
			t.Errorf("call %d erp_name %q, want %q", i, outs[i].ERPName, outs[0].ERPName)
		}
		if !outs[i].AlreadyPosted {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d calls posted fresh, want 1", fresh)
	}
	if _, inserts, submits := fx.erp.stats(); inserts != 1 || submits != 1 {
		t.Errorf("inserts %d submits %d, want 1 and 1", inserts, submits)
	}
}

func TestAdminExistingEntries(t *testing.T) {
	t.Run("submitted entry is recorded", func(t *testing.T) {
		p := approvedProposal()
		fx := newAdminFixture(t, p)
		fx.erp.add(jeDoc("ACC-JV-2026-00900", 1, p))
		res, text, out := fx.postID(t, p.ID)
		if res.IsError || out.ERPName != "ACC-JV-2026-00900" || out.AlreadyPosted {
			t.Fatalf("%s %+v", text, out)
		}
		if fx.erp.writes() != 0 {
			t.Errorf("%d writes, want none", fx.erp.writes())
		}
	})
	t.Run("matching draft is submitted", func(t *testing.T) {
		p := approvedProposal()
		fx := newAdminFixture(t, p)
		d := jeDoc("ACC-JV-2026-00901", 0, p)
		// Rows in another order still match.
		rows := d["accounts"].([]any)
		rows[0], rows[1] = rows[1], rows[0]
		fx.erp.add(d)
		res, text, out := fx.postID(t, p.ID)
		if res.IsError || out.ERPName != "ACC-JV-2026-00901" {
			t.Fatalf("%s %+v", text, out)
		}
		if _, inserts, submits := fx.erp.stats(); inserts != 0 || submits != 1 {
			t.Errorf("inserts %d submits %d, want 0 and 1", inserts, submits)
		}
	})

	tamper := []struct {
		name string
		edit func(d doc)
	}{
		{"different amount", func(d doc) {
			r := d["accounts"].([]any)[0].(map[string]any)
			r["debit_in_account_currency"], r["debit"] = json.Number("1180.00"), json.Number("1180.00")
		}},
		{"different account", func(d doc) { d["accounts"].([]any)[1].(map[string]any)["account"] = "Cash - TT" }},
		{"extra row", func(d doc) {
			d["accounts"] = append(d["accounts"].([]any), map[string]any{"account": acctBank,
				"debit_in_account_currency": json.Number("0"), "credit_in_account_currency": json.Number("0")})
		}},
		{"company currency differs", func(d doc) { d["accounts"].([]any)[0].(map[string]any)["debit"] = json.Number("1.00") }},
		{"other company", func(d doc) { d["company"] = otherCompany }},
		{"other date", func(d doc) { d["posting_date"] = "2026-09-29" }},
		{"other voucher type", func(d doc) { d["voucher_type"] = "Contra Entry" }},
	}
	for _, tc := range tamper {
		t.Run("tampered draft: "+tc.name, func(t *testing.T) {
			p := approvedProposal()
			fx := newAdminFixture(t, p)
			d := jeDoc("ACC-JV-2026-00902", 0, p)
			tc.edit(d)
			fx.erp.add(d)
			res, text, _ := fx.postID(t, p.ID)
			wantRefusal(t, res, text, CodeERPMismatch)
			if fx.erp.writes() != 0 {
				t.Errorf("%d writes, want none", fx.erp.writes())
			}
			if row := fx.ps.get(p.ID); row.Status != ProposalApproved {
				t.Errorf("row %+v", row)
			}
		})
	}

	t.Run("two matches", func(t *testing.T) {
		p := approvedProposal()
		fx := newAdminFixture(t, p)
		fx.erp.add(jeDoc("ACC-JV-2026-00903", 1, p))
		fx.erp.add(jeDoc("ACC-JV-2026-00904", 0, p))
		res, text, _ := fx.postID(t, p.ID)
		wantRefusal(t, res, text, CodeERPConflict)
		if fx.erp.writes() != 0 {
			t.Errorf("%d writes, want none", fx.erp.writes())
		}
	})
	t.Run("cancelled match", func(t *testing.T) {
		p := approvedProposal()
		fx := newAdminFixture(t, p)
		fx.erp.add(jeDoc("ACC-JV-2026-00905", 2, p))
		res, text, _ := fx.postID(t, p.ID)
		wantRefusal(t, res, text, CodeERPConflict)
		if fx.erp.writes() != 0 {
			t.Errorf("%d writes, want none", fx.erp.writes())
		}
	})
}

func TestAdminMismatchAfterSubmit(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	fx.erp.onSubmit = func(d doc) {
		r := d["accounts"].([]any)[0].(map[string]any)
		r["debit_in_account_currency"], r["debit"] = json.Number("118.01"), json.Number("118.01")
	}
	res, text, _ := fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeERPMismatch)
	if row := fx.ps.get(p.ID); row.Status != ProposalApproved || row.ERPName != nil {
		t.Errorf("row %+v, want approved and unrecorded", row)
	}
	if _, saves := fx.ps.counts(); saves != 0 {
		t.Errorf("%d saves, want none", saves)
	}
	if logs := fx.logs.String(); !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, "ACC-JV-2026-00001") {
		t.Errorf("no error log naming the entry: %s", logs)
	}
	res, text, _ = fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeERPMismatch)
	if _, inserts, submits := fx.erp.stats(); inserts != 1 || submits != 1 {
		t.Errorf("inserts %d submits %d, want 1 and 1", inserts, submits)
	}
}

func TestAdminPermissionRefused(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	fx.erp.forbid = true
	res, text, _ := fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeERPPermission)
}

// TestAdminDeadline: the whole call shares one deadline; a slow ERPNext
// fails it without a save.
func TestAdminDeadline(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	fx.a.timeout = 50 * time.Millisecond
	fx.erp.delay = 2 * time.Second
	start := time.Now()
	res, text, _ := fx.postID(t, p.ID)
	wantRefusal(t, res, text, CodeERPUnavailable)
	if d := time.Since(start); d > time.Second {
		t.Errorf("call took %v, want it cut at the deadline", d)
	}
	if _, saves := fx.ps.counts(); saves != 0 {
		t.Errorf("%d saves, want none", saves)
	}
}

// TestAdminCancelAfterSubmitStillSaves: once ERPNext accepted the submit,
// the save runs detached, so a cancelled caller can't leave the row
// approved.
func TestAdminCancelAfterSubmitStillSaves(t *testing.T) {
	p := approvedProposal()
	fx := newAdminFixture(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fx.a.afterSubmit = cancel
	_, out, err := fx.a.handle(ctx, nil, PostApprovedInput{ProposalID: p.ID.String()})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the hook didn't cancel the caller")
	}
	fx.ps.mu.Lock()
	saveErrs := slices.Clone(fx.ps.saveErrs)
	fx.ps.mu.Unlock()
	if len(saveErrs) != 1 || saveErrs[0] != nil {
		t.Errorf("save ctx errors %v, want one live save", saveErrs)
	}
	if row := fx.ps.get(p.ID); row.Status != ProposalPosted || row.ERPName == nil || *row.ERPName != out.ERPName {
		t.Errorf("row %+v, want posted", row)
	}
}

// TestAdminToolOnlyOnAdminServer: RegisterTools never lists the write tool.
func TestAdminToolOnlyOnAdminServer(t *testing.T) {
	if slices.Contains(ToolNames, ToolPostApprovedJournalEntry) {
		t.Error("ToolNames lists the admin tool")
	}
	res, err := toolSession(t, newFakeERP(t)).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == ToolPostApprovedJournalEntry {
			t.Error("RegisterTools lists the admin tool")
		}
	}
}

// TestAdminSchemaGolden pins the tool's listed contract in its own
// directory (testdata/schemas is the read server's). Rewrite with
//
//	go test ./internal/books -run TestAdminSchemaGolden -update
func TestAdminSchemaGolden(t *testing.T) {
	fx := newAdminFixture(t)
	res, err := fx.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("%d tools, want 1", len(res.Tools))
	}
	tool := res.Tools[0]
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || !a.IdempotentHint {
		t.Errorf("annotations %+v, want not read-only, not destructive, idempotent", a)
	}
	var in struct {
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties any            `json:"additionalProperties"`
	}
	b, _ := json.Marshal(tool.InputSchema)
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Properties) != 1 || in.Properties["proposal_id"] == nil || !slices.Equal(in.Required, []string{"proposal_id"}) || in.AdditionalProperties != false {
		t.Errorf("input schema %s, want only a required proposal_id and no additional properties", b)
	}
	checkSchemaGoldens(t, res.Tools, filepath.Join("testdata", "admin-schemas"))
}

// TestAdminLogsAreQuietOnPayload: refusals log the cause but never lines.
func TestAdminLogsAreQuietOnPayload(t *testing.T) {
	p := approvedProposal()
	p.Payload.Lines[1].CreditPaise = 1 // unbalanced
	fx := newAdminFixture(t, p)
	_, _, _ = fx.postID(t, p.ID)
	logs := fx.logs.String()
	if !strings.Contains(logs, CodeUnbalanced) {
		t.Errorf("log lacks the refusal: %s", logs)
	}
	for _, leak := range []string{acctCharges, acctBank, "118.00", "11800", p.Payload.Remark} {
		if strings.Contains(logs, leak) {
			t.Errorf("log carries %q", leak)
		}
	}
}
