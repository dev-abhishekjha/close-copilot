package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

const selfApprovalMsg = "you proposed this entry, so a different checker must approve it"

// fakeApprovals records what the handlers pass and answers with the
// configured errors. It knows nothing about roles: the actor it records
// is what the handler took from the session.
type fakeApprovals struct {
	mu         sync.Mutex
	actors     []string
	ids        []uuid.UUID
	reasons    []string
	proposeErr error
	approveErr error
	rejectErr  error
	pendingErr error
	pending    []store.JournalProposal
	rows       map[uuid.UUID]store.JournalProposal
	erpName    string
}

func (f *fakeApprovals) record(actor string, id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, actor)
	f.ids = append(f.ids, id)
}

func (f *fakeApprovals) Propose(_ context.Context, actor string, findingID uuid.UUID) (store.JournalProposal, error) {
	f.record(actor, findingID)
	if f.proposeErr != nil {
		return store.JournalProposal{}, f.proposeErr
	}
	return store.JournalProposal{ID: pid(9), FindingID: &findingID, Status: "proposed", Maker: actor}, nil
}

func (f *fakeApprovals) Approve(_ context.Context, actor string, id uuid.UUID) (ApproveResult, error) {
	f.record(actor, id)
	if f.approveErr != nil {
		return ApproveResult{}, f.approveErr
	}
	name := f.erpName
	if name == "" {
		name = "ACC-JV-2026-00007"
	}
	f.mu.Lock()
	if f.rows == nil {
		f.rows = map[uuid.UUID]store.JournalProposal{}
	}
	f.rows[id] = store.JournalProposal{ID: id, Status: "posted", ERPName: &name}
	f.mu.Unlock()
	return ApproveResult{ProposalID: id, Status: "posted", ERPName: name}, nil
}

func (f *fakeApprovals) Reject(_ context.Context, actor string, id uuid.UUID, reason string) error {
	f.record(actor, id)
	f.mu.Lock()
	f.reasons = append(f.reasons, reason)
	f.mu.Unlock()
	return f.rejectErr
}

func (f *fakeApprovals) Pending(context.Context) ([]store.JournalProposal, error) {
	return f.pending, f.pendingErr
}

func (f *fakeApprovals) Get(_ context.Context, id uuid.UUID) (store.JournalProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[id]
	if !ok {
		return store.JournalProposal{}, &ApprovalError{Kind: ErrApprovalNotFound, Message: notFoundMsg}
	}
	return p, nil
}

func (f *fakeApprovals) calls() ([]string, []uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actors...), append([]uuid.UUID(nil), f.ids...)
}

func pid(n byte) uuid.UUID { return uuid.UUID{0xab, 0xcd, 15: n} }

func approvalsServer(t testing.TB, fa *fakeApprovals, ff *fakeFindings) *testServer {
	t.Helper()
	if ff == nil {
		ff = newFakeFindings()
	}
	return findingsServer(t, ff, func(o *Options) { o.Approvals = fa })
}

func decodeJSON(t testing.TB, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

func TestApprovalsNotConfigured(t *testing.T) {
	ts := findingsServer(t, newFakeFindings())
	for _, c := range []struct {
		role         Role
		method, path string
	}{
		{RoleChecker, http.MethodGet, "/approvals"},
		{RoleChecker, http.MethodPost, "/api/proposals/" + pid(1).String() + "/approve"},
		{RoleChecker, http.MethodPost, "/proposals/" + pid(1).String() + "/reject"},
		{RoleMaker, http.MethodPost, "/api/findings/" + fid(1).String() + "/proposals"},
		{RoleMaker, http.MethodPost, "/findings/" + fid(1).String() + "/proposals"},
	} {
		if w := ts.do(t, ts.as(c.role, c.method, c.path, "", "")); w.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", c.method, c.path, w.Code)
		}
	}
}

func TestProposeAPI(t *testing.T) {
	fa := &fakeApprovals{}
	ts := approvalsServer(t, fa, nil)
	path := "/api/findings/" + fid(1).String() + "/proposals"

	// Forged maker/checker fields in the body are ignored.
	w := ts.do(t, ts.as(RoleMaker, http.MethodPost, path, `{"maker":"ravi","checker":"asha"}`, "application/json"))
	if w.Code != http.StatusCreated {
		t.Fatalf("propose = %d %s", w.Code, w.Body)
	}
	m := decodeJSON(t, w.Body.String())
	if m["proposal_id"] != pid(9).String() || m["status"] != "proposed" {
		t.Errorf("body %v", m)
	}
	actors, ids := fa.calls()
	if len(actors) != 1 || actors[0] != "asha" || ids[0] != fid(1) {
		t.Errorf("backend got %v %v, want asha on the finding", actors, ids)
	}

	// Bearer works too (no CSRF).
	w = ts.do(t, req{method: http.MethodPost, path: path, header: map[string]string{"Authorization": "Bearer " + ts.fix.tokens["asha"]}})
	if w.Code != http.StatusCreated {
		t.Errorf("bearer propose = %d", w.Code)
	}

	// Not a UUID: 404 without a backend call.
	w = ts.do(t, ts.as(RoleMaker, http.MethodPost, "/api/findings/nope/proposals", "", ""))
	if w.Code != http.StatusNotFound || decodeJSON(t, w.Body.String())["error"] != notFoundMsg {
		t.Errorf("bad id = %d %s", w.Code, w.Body)
	}
	if actors, _ := fa.calls(); len(actors) != 2 {
		t.Errorf("backend called %d times, want 2", len(actors))
	}
}

func TestApprovalErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"invalid", &ApprovalError{Kind: ErrApprovalInvalid, Message: "the proposed entry is invalid"}, http.StatusBadRequest, "the proposed entry is invalid"},
		{"self", &ApprovalError{Kind: ErrApprovalForbidden, Message: selfApprovalMsg}, http.StatusForbidden, selfApprovalMsg},
		{"not found", &ApprovalError{Kind: ErrApprovalNotFound, Message: "not found"}, http.StatusNotFound, "not found"},
		{"conflict", &ApprovalError{Kind: ErrApprovalConflict, Message: "this finding already has a journal proposal"}, http.StatusConflict, "this finding already has a journal proposal"},
		{"post failed", &ApprovalError{Kind: ErrApprovalPostFailed, Message: "posting to ERPNext was refused: unbalanced"}, http.StatusBadGateway, "posting to ERPNext was refused: unbalanced"},
		{"bare kind", ErrApprovalConflict, http.StatusConflict, "conflict"},
		{"unknown", errors.New("db down: secret detail"), http.StatusInternalServerError, "internal error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeApprovals{proposeErr: tc.err, approveErr: tc.err, rejectErr: tc.err}
			ts := approvalsServer(t, fa, nil)
			for _, c := range []struct {
				role Role
				path string
				body string
			}{
				{RoleMaker, "/api/findings/" + fid(1).String() + "/proposals", ""},
				{RoleChecker, "/api/proposals/" + pid(1).String() + "/approve", ""},
				{RoleChecker, "/api/proposals/" + pid(1).String() + "/reject", `{"reason":"no"}`},
			} {
				w := ts.do(t, ts.as(c.role, http.MethodPost, c.path, c.body, "application/json"))
				if w.Code != tc.code || decodeJSON(t, w.Body.String())["error"] != tc.msg {
					t.Errorf("%s = %d %s, want %d %q", c.path, w.Code, w.Body, tc.code, tc.msg)
				}
				if strings.Contains(w.Body.String(), "secret detail") {
					t.Errorf("%s leaks the cause", c.path)
				}
			}
			if tc.code == http.StatusInternalServerError && !strings.Contains(ts.logs.String(), "web: approvals") {
				t.Error("internal error not logged")
			}
		})
	}
}

func TestApproveAPI(t *testing.T) {
	fa := &fakeApprovals{}
	ts := approvalsServer(t, fa, nil)
	w := ts.do(t, ts.as(RoleChecker, http.MethodPost, "/api/proposals/"+pid(1).String()+"/approve", `{"checker":"asha"}`, "application/json"))
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	m := decodeJSON(t, w.Body.String())
	if m["status"] != "posted" || m["erp_name"] != "ACC-JV-2026-00007" || m["proposal_id"] != pid(1).String() ||
		m["erp_link"] != erpTest+"/app/journal-entry/ACC-JV-2026-00007" {
		t.Errorf("body %v", m)
	}
	if actors, _ := fa.calls(); len(actors) != 1 || actors[0] != "ravi" {
		t.Errorf("actor %v, want ravi", actors)
	}
	// Not a UUID.
	if w := ts.do(t, ts.as(RoleChecker, http.MethodPost, "/api/proposals/123/approve", "", "")); w.Code != http.StatusNotFound {
		t.Errorf("bad id = %d", w.Code)
	}
}

// A proposal whose finding is no longer open is refused with 409 and the
// backend's fixed message, over JSON and over the form.
func TestApproveFindingNotOpen(t *testing.T) {
	const notOpen = "the finding is no longer open, so this entry can't be approved"
	fa := &fakeApprovals{approveErr: &ApprovalError{Kind: ErrApprovalConflict, Message: notOpen}}
	ts := approvalsServer(t, fa, nil)
	w := ts.do(t, ts.as(RoleChecker, http.MethodPost, "/api/proposals/"+pid(1).String()+"/approve", "", ""))
	if w.Code != http.StatusConflict || decodeJSON(t, w.Body.String())["error"] != notOpen {
		t.Errorf("JSON = %d %s", w.Code, w.Body)
	}
	w = ts.do(t, formPost(ts, RoleChecker, "/proposals/"+pid(1).String()+"/approve", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the finding is no longer open") {
		t.Errorf("form = %d %s", w.Code, w.Body)
	}
}

// A checker who made the proposal (their role changed since) is refused by
// the backend with the fixed message, over JSON and over the form.
func TestSelfApprovalMessage(t *testing.T) {
	fa := &fakeApprovals{approveErr: &ApprovalError{Kind: ErrApprovalForbidden, Message: selfApprovalMsg}}
	ts := approvalsServer(t, fa, nil)
	w := ts.do(t, ts.as(RoleChecker, http.MethodPost, "/api/proposals/"+pid(1).String()+"/approve", "", ""))
	if w.Code != http.StatusForbidden || decodeJSON(t, w.Body.String())["error"] != selfApprovalMsg {
		t.Errorf("JSON = %d %s", w.Code, w.Body)
	}
	w = ts.do(t, formPost(ts, RoleChecker, "/proposals/"+pid(1).String()+"/approve", nil))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), selfApprovalMsg) || !strings.Contains(w.Body.String(), `id="approvals"`) {
		t.Errorf("form = %d %s", w.Code, w.Body)
	}
}

func TestApprovalForms(t *testing.T) {
	fa := &fakeApprovals{}
	ts := approvalsServer(t, fa, nil)
	forged := url.Values{"maker": {"ravi"}, "checker": {"asha"}, "actor": {"asha"}}

	// Approve: 303 to the page naming the posted proposal, which shows the
	// ERPNext link.
	w := ts.do(t, formPost(ts, RoleChecker, "/proposals/"+pid(1).String()+"/approve", forged))
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || loc != "/approvals?posted="+pid(1).String() {
		t.Fatalf("approve form = %d %s", w.Code, loc)
	}
	body := ts.get(t, RoleChecker, loc).Body.String()
	wantAll(t, body, "posted as journal entry", `href="`+erpTest+`/app/journal-entry/ACC-JV-2026-00007"`, "ACC-JV-2026-00007", pid(1).String()[:8])

	// Reject.
	reject := url.Values{"reason": {"  wrong account "}, "maker": {"ravi"}, "checker": {"asha"}}
	w = ts.do(t, formPost(ts, RoleChecker, "/proposals/"+pid(2).String()+"/reject", reject))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/approvals" {
		t.Fatalf("reject form = %d %s", w.Code, w.Header().Get("Location"))
	}

	// Propose from the finding page.
	w = ts.do(t, formPost(ts, RoleMaker, "/findings/"+fid(3).String()+"/proposals", forged))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/findings/"+fid(3).String() {
		t.Fatalf("propose form = %d %s", w.Code, w.Header().Get("Location"))
	}

	actors, ids := fa.calls()
	want := []string{"ravi", "ravi", "asha"}
	if strings.Join(actors, ",") != strings.Join(want, ",") || ids[0] != pid(1) || ids[1] != pid(2) || ids[2] != fid(3) {
		t.Errorf("backend got %v %v, want %v", actors, ids, want)
	}
	if fa.reasons[0] != "  wrong account " {
		t.Errorf("reason %q passed on (the backend trims)", fa.reasons[0])
	}

	// A form post asking for JSON gets JSON.
	w = ts.do(t, func() req {
		r := formPost(ts, RoleMaker, "/findings/"+fid(3).String()+"/proposals", nil)
		r.header = map[string]string{"Accept": "application/json"}
		return r
	}())
	if w.Code != http.StatusCreated || decodeJSON(t, w.Body.String())["status"] != "proposed" {
		t.Errorf("JSON form = %d %s", w.Code, w.Body)
	}

	// A form without the CSRF token is refused before the backend.
	before, _ := fa.calls()
	n := len(before)
	ck, _ := ts.session(RoleChecker)
	w = ts.do(t, req{method: http.MethodPost, path: "/proposals/" + pid(1).String() + "/approve", body: "x=1",
		contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusForbidden {
		t.Errorf("no CSRF = %d", w.Code)
	}
	if actors, _ := fa.calls(); len(actors) != n {
		t.Error("backend called without CSRF")
	}
}

func TestRejectAPI(t *testing.T) {
	fa := &fakeApprovals{}
	ts := approvalsServer(t, fa, nil)
	path := "/api/proposals/" + pid(1).String() + "/reject"
	w := ts.do(t, ts.as(RoleChecker, http.MethodPost, path, `{"reason":"duplicate","checker":"asha","maker":"ravi"}`, "application/json"))
	if w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	if m := decodeJSON(t, w.Body.String()); m["status"] != "rejected" || m["proposal_id"] != pid(1).String() {
		t.Errorf("body %v", m)
	}
	if actors, _ := fa.calls(); actors[0] != "ravi" || fa.reasons[0] != "duplicate" {
		t.Errorf("backend got %v %v", actors, fa.reasons)
	}
	// A form body works on the API too.
	w = ts.do(t, ts.as(RoleChecker, http.MethodPost, path, "reason=dup", "application/x-www-form-urlencoded"))
	if w.Code != http.StatusOK || fa.reasons[1] != "dup" {
		t.Errorf("form API reject = %d %v", w.Code, fa.reasons)
	}

	for _, tc := range []struct{ name, body string }{
		{"not json", `{"reason":`},
		{"two objects", `{"reason":"a"}{"reason":"b"}`},
		{"over 4 KiB", `{"reason":"` + strings.Repeat("x", 5000) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := fa.calls()
			w := ts.do(t, ts.as(RoleChecker, http.MethodPost, path, tc.body, "application/json"))
			if w.Code != http.StatusBadRequest {
				t.Errorf("= %d %s", w.Code, w.Body)
			}
			if after, _ := fa.calls(); len(after) != len(before) {
				t.Error("backend called")
			}
		})
	}
}

func TestApprovalsPage(t *testing.T) {
	t0 := time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC)
	f1 := fid(1)
	ravi := "Ravi"
	fa := &fakeApprovals{pending: []store.JournalProposal{
		{ID: pid(1), FindingID: &f1, CompanyID: "sharma", Status: "proposed", Maker: "asha", CreatedAt: t0,
			Payload: store.JournalPayload{PostingDate: "2026-08-31", Remark: "<script>alert(1)</script>", Lines: []store.JournalLine{
				{Account: "Bank Charges - STPL", DebitPaise: 29500}, {Account: "HDFC Bank - STPL", CreditPaise: 29500},
			}}},
		{ID: pid(2), CompanyID: "sharma", Status: "proposed", Maker: " RAVI ", CreatedAt: t0,
			Payload: store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{{Account: "A", DebitPaise: 100}, {Account: "B", CreditPaise: 100}}}},
		{ID: pid(3), CompanyID: "sharma", Status: "approved", Maker: "asha", Checker: &ravi, CreatedAt: t0,
			Payload: store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{{Account: "C", DebitPaise: 100}, {Account: "D", CreditPaise: 100}}}},
	}}
	ts := approvalsServer(t, fa, nil)
	w := ts.get(t, RoleChecker, "/approvals")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /approvals = %d", w.Code)
	}
	body := w.Body.String()
	wantAll(t, body, "Pending proposals", "sharma", "2026-08-31", "₹295.00", "Bank Charges - STPL", "HDFC Bank - STPL",
		`href="/findings/`+f1.String()+`"`, "2026-09-02 10:30 UTC", "&lt;script&gt;",
		`action="/proposals/`+pid(1).String()+`/approve"`, `action="/proposals/`+pid(1).String()+`/reject"`,
		`required maxlength="500"`, "You proposed this; another checker must decide",
		"approved, posting failed; retry by Ravi", `action="/proposals/`+pid(3).String()+`/approve"`)
	wantNone(t, body, "<script>alert", `action="/proposals/`+pid(2).String()+`/approve"`, `action="/proposals/`+pid(2).String()+`/reject"`,
		`action="/proposals/`+pid(3).String()+`/reject"`)

	// Viewers and makers can't see it; anonymous goes to login.
	for _, r := range []Role{RoleViewer, RoleMaker} {
		if w := ts.get(t, r, "/approvals"); w.Code != http.StatusForbidden {
			t.Errorf("%s GET /approvals = %d", r, w.Code)
		}
	}
	if w := ts.do(t, req{method: http.MethodGet, path: "/approvals"}); w.Code != http.StatusSeeOther {
		t.Errorf("anonymous = %d", w.Code)
	}

	// Another checker sees no retry button on ravi's approved row.
	other := User{Name: "meera", Role: RoleChecker, PasswordHash: ts.fix.byName["ravi"].PasswordHash}
	ck, _ := ts.newSession(other)
	if err := ts.SetUsers(ts.fix.with(t, &other)); err != nil {
		t.Fatal(err)
	}
	body = ts.do(t, req{method: http.MethodGet, path: "/approvals", cookies: []*http.Cookie{ck}}).Body.String()
	wantNone(t, body, `action="/proposals/`+pid(3).String()+`/approve"`)
	wantAll(t, body, `action="/proposals/`+pid(2).String()+`/approve"`)

	// A posted id that isn't posted shows no notice.
	if body := ts.get(t, RoleChecker, "/approvals?posted="+pid(1).String()).Body.String(); strings.Contains(body, "posted as journal entry") {
		t.Error("notice for an unposted proposal")
	}

	// Pending failure: 500.
	fa.pendingErr = errors.New("db down")
	if w := ts.get(t, RoleChecker, "/approvals"); w.Code != http.StatusInternalServerError {
		t.Errorf("pending error = %d", w.Code)
	}
}

func TestFindingPageCreateForm(t *testing.T) {
	preview := func() *store.JournalProposal {
		return &store.JournalProposal{CompanyID: "acme", Status: "proposed", Payload: store.JournalPayload{
			PostingDate: "2026-09-30", Lines: []store.JournalLine{{Account: "A", DebitPaise: 100}, {Account: "B", CreditPaise: 100}},
		}}
	}
	cases := []struct {
		name     string
		status   string
		verified bool
		preview  bool
		live     *store.JournalProposal
		liveErr  error
		form     bool
	}{
		{"verified open", "open", true, true, nil, nil, true},
		{"unverified", "open", false, true, nil, nil, false},
		{"needs review", "needs_review", true, true, nil, nil, false},
		{"no preview", "open", true, false, nil, nil, false},
		{"live exists", "open", true, true, &store.JournalProposal{ID: pid(4), Status: "proposed", Maker: "asha"}, nil, false},
		{"lookup fails", "open", true, true, nil, errors.New("db down"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff := newFakeFindings()
			x := fnd(1, "unrecorded_bank_charge", tc.status)
			x.Verified = tc.verified
			if tc.preview {
				x.Proposal = preview()
			}
			x = ff.put(x)
			if tc.live != nil {
				ff.setLive(x.ID, *tc.live)
			}
			ff.liveErr = tc.liveErr
			ts := approvalsServer(t, &fakeApprovals{}, ff)
			body := page(t, ts, RoleMaker, x.ID)
			form := `action="/findings/` + x.ID.String() + `/proposals"`
			if got := strings.Contains(body, form); got != tc.form {
				t.Errorf("create form shown = %v, want %v", got, tc.form)
			}
			for _, r := range []Role{RoleViewer, RoleChecker} {
				wantNone(t, page(t, ts, r, x.ID), "Create proposal")
			}
		})
	}

	t.Run("posted proposal shows status, maker and the entry", func(t *testing.T) {
		ff := newFakeFindings()
		x := fnd(1, "unrecorded_bank_charge", "open")
		x.Verified, x.Proposal = true, preview()
		x = ff.put(x)
		name := "ACC-JV-2026-00099"
		ff.setLive(x.ID, store.JournalProposal{ID: pid(5), Status: "posted", Maker: "asha", ERPName: &name, Payload: x.Proposal.Payload})
		ts := approvalsServer(t, &fakeApprovals{}, ff)
		body := page(t, ts, RoleViewer, x.ID)
		wantAll(t, body, "Status: posted", "Maker: asha", `href="`+erpTest+`/app/journal-entry/ACC-JV-2026-00099"`)
	})

	t.Run("refused propose shows the message on the page", func(t *testing.T) {
		ff := newFakeFindings()
		x := fnd(1, "unrecorded_bank_charge", "open")
		x.Verified, x.Proposal = true, preview()
		x = ff.put(x)
		fa := &fakeApprovals{proposeErr: &ApprovalError{Kind: ErrApprovalConflict, Message: "this finding already has a journal proposal"}}
		ts := approvalsServer(t, fa, ff)
		w := ts.do(t, formPost(ts, RoleMaker, "/findings/"+x.ID.String()+"/proposals", nil))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "this finding already has a journal proposal") ||
			!strings.Contains(w.Body.String(), `id="actions"`) {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})
}

func TestWantsJSON(t *testing.T) {
	cases := []struct {
		path, accept string
		want         bool
	}{
		{"/api/x", "", true},
		{"/approvals", "", false},
		{"/approvals", "text/html", false},
		{"/approvals", "application/json", true},
		{"/approvals", "text/html, application/json;q=0.9", true},
		{"/approvals", "application/jsonx", false},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Accept", tc.accept)
		if got := wantsJSON(r); got != tc.want {
			t.Errorf("wantsJSON(%s, %q) = %v", tc.path, tc.accept, got)
		}
	}
}
