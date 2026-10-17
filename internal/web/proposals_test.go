package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// getFailing is a backend whose Get fails with err.
type getFailing struct {
	*fakeApprovals
	err error
}

func (g getFailing) Get(context.Context, uuid.UUID) (store.JournalProposal, error) {
	return store.JournalProposal{}, g.err
}

var (
	tDecided = time.Date(2026, 9, 3, 11, 5, 0, 0, time.UTC)
	tPosted  = time.Date(2026, 9, 3, 11, 6, 0, 0, time.UTC)
)

// propAt is uiProposal with its decision recorded as status requires.
func propAt(n byte, status, maker, checker string) store.JournalProposal {
	x := uiProposal(n, status, maker, nil)
	if checker != "" {
		x.Checker = &checker
		d := tDecided
		x.DecidedAt = &d
	}
	if status == "posted" {
		name, at := "ACC-JV-2026-00011", tPosted
		x.ERPName, x.PostedAt = &name, &at
	}
	if status == "rejected" {
		reason := "wrong account"
		x.Reason = &reason
	}
	return x
}

func proposalsServer(t *testing.T, rows ...store.JournalProposal) (*testServer, *fakeApprovals) {
	t.Helper()
	fa := &fakeApprovals{rows: map[uuid.UUID]store.JournalProposal{}}
	for _, x := range rows {
		fa.rows[x.ID] = x
	}
	return approvalsServer(t, fa, nil), fa
}

func proposalBody(t *testing.T, ts *testServer, role Role, id uuid.UUID) string {
	t.Helper()
	w := ts.get(t, role, "/proposals/"+id.String())
	if w.Code != http.StatusOK {
		t.Fatalf("%s GET proposal = %d %s", role, w.Code, w.Body)
	}
	return w.Body.String()
}

func propTimeline(t *testing.T, body string) string {
	t.Helper()
	return segment(t, body, `<ol class="ui-timeline">`, "</ol>")
}

func TestProposalPageTimeline(t *testing.T) {
	f := fid(7)
	posted := propAt(1, "posted", "asha", "ravi")
	posted.FindingID = &f
	ts, _ := proposalsServer(t, posted, propAt(2, "rejected", "asha", "ravi"), propAt(3, "proposed", "asha", ""),
		propAt(4, "approved", "asha", "ravi"))

	cases := []struct {
		name  string
		id    uuid.UUID
		order []string // in this order within the timeline
		none  []string
		items int
	}{
		{"posted", pid(1), []string{"Proposed", "by asha", "2026-09-02 10:30 UTC", "Approved", "by ravi", "2026-09-03 11:05 UTC",
			"Posted", "2026-09-03 11:06 UTC", "Journal entry ACC-JV-2026-00011"}, []string{"Rejected"}, 3},
		{"rejected", pid(2), []string{"Proposed", "by asha", "Rejected", "by ravi", "2026-09-03 11:05 UTC", "Reason: wrong account"},
			[]string{"Approved", "Posted"}, 2},
		{"pending", pid(3), []string{"Proposed", "by asha", "2026-09-02 10:30 UTC"}, []string{"Approved", "Rejected", "Posted", "by ravi"}, 1},
		{"approved, posting failed", pid(4), []string{"Proposed", "Approved", "by ravi"}, []string{"Posted", "Rejected"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := propTimeline(t, proposalBody(t, ts, RoleViewer, tc.id))
			at := 0
			for _, s := range tc.order {
				i := strings.Index(tl[at:], s)
				if i < 0 {
					t.Fatalf("%q missing or out of order in %s", s, tl)
				}
				at += i + len(s)
			}
			wantNone(t, tl, tc.none...)
			if n := strings.Count(tl, `<li class="ui-timeline-item`); n != tc.items {
				t.Errorf("entries = %d, want %d", n, tc.items)
			}
		})
	}

	// Missing times are left out, not guessed.
	x := propAt(5, "posted", "asha", "ravi")
	x.DecidedAt, x.PostedAt = nil, nil
	ts, _ = proposalsServer(t, x)
	tl := propTimeline(t, proposalBody(t, ts, RoleViewer, pid(5)))
	if n := strings.Count(tl, "<time"); n != 1 {
		t.Errorf("times shown = %d, want only the proposal's", n)
	}
	wantAll(t, tl, "Approved", "Posted")
}

func TestProposalPageControls(t *testing.T) {
	type want struct{ approve, reject, own, note bool }
	cases := []struct {
		name   string
		viewer string // ravi, meera (a second checker), asha (maker), demo (viewer)
		x      store.JournalProposal
		want   want
	}{
		{"checker, someone else's proposed", "ravi", propAt(1, "proposed", "asha", ""), want{approve: true, reject: true}},
		{"recorded checker, approved", "ravi", propAt(1, "approved", "asha", "Ravi"), want{approve: true, note: true}},
		{"other checker, approved", "meera", propAt(1, "approved", "asha", "ravi"), want{note: true}},
		{"checker, own proposed", "ravi", propAt(1, "proposed", " RAVI ", ""), want{own: true}},
		{"checker, posted", "ravi", propAt(1, "posted", "asha", "ravi"), want{}},
		{"checker, rejected", "ravi", propAt(1, "rejected", "asha", "ravi"), want{}},
		{"checker, own posted", "ravi", propAt(1, "posted", "ravi", "meera"), want{}},
		{"maker, own proposed", "asha", propAt(1, "proposed", "asha", ""), want{}},
		{"maker, someone else's proposed", "asha", propAt(1, "proposed", "nora", ""), want{}},
		{"maker, approved", "asha", propAt(1, "approved", "nora", "asha"), want{note: true}},
		{"viewer, proposed", "demo", propAt(1, "proposed", "asha", ""), want{}},
		{"viewer, own proposed", "demo", propAt(1, "proposed", "demo", ""), want{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := proposalsServer(t, tc.x)
			var ck *http.Cookie
			switch tc.viewer {
			case "meera":
				ck, _ = addChecker(t, ts, "meera")
			default:
				ck, _ = ts.newSession(ts.fix.byName[tc.viewer])
			}
			body := getWith(t, ts, ck, "/proposals/"+pid(1).String())
			for kind, w := range map[string]bool{"approve": tc.want.approve, "reject": tc.want.reject} {
				for _, h := range controlHooks(pid(1), kind) {
					if got := strings.Contains(body, h); got != w {
						t.Errorf("%s shown = %v, want %v", h, got, w)
					}
				}
			}
			if tc.want.own {
				wantOwnState(t, body, pid(1), 2)
			} else {
				wantNone(t, body, `own-`+pid(1).String(), "You proposed this")
			}
			if got := strings.Contains(body, "approved, posting failed; retry by"); got != tc.want.note {
				t.Errorf("status note shown = %v, want %v", got, tc.want.note)
			}
			if !tc.want.approve && !tc.want.reject && !tc.want.own {
				wantNone(t, body, `action="/proposals/`)
				wantNone(t, mainContent(t, body), "<dialog", "data-dialog-open")
			}
		})
	}
}

func TestProposalPageRoles(t *testing.T) {
	ts, _ := proposalsServer(t, propAt(1, "proposed", "asha", ""))
	path := "/proposals/" + pid(1).String()
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		if w := ts.get(t, role, path); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Proposal "+pid(1).String()[:8]) {
			t.Errorf("%s = %d", role, w.Code)
		}
	}
	off := findingsServer(t, newFakeFindings())
	failing := approvalsServer(t, nil, nil)
	failing.approvals = getFailing{&fakeApprovals{}, errors.New("db down: secret detail")}

	cases := []struct {
		name string
		ts   *testServer
		r    req
		code int
	}{
		{"anonymous", ts, req{method: http.MethodGet, path: path}, http.StatusSeeOther},
		{"unknown id", ts, ts.as(RoleViewer, http.MethodGet, "/proposals/"+pid(2).String(), "", ""), http.StatusNotFound},
		{"not a uuid", ts, ts.as(RoleViewer, http.MethodGet, "/proposals/nope", "", ""), http.StatusNotFound},
		{"not configured", off, off.as(RoleViewer, http.MethodGet, path, "", ""), http.StatusNotImplemented},
		{"not configured, anonymous", off, req{method: http.MethodGet, path: path}, http.StatusSeeOther},
		{"backend error", failing, failing.as(RoleViewer, http.MethodGet, path, "", ""), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.ts.do(t, tc.r)
			if w.Code != tc.code {
				t.Fatalf("= %d, want %d", w.Code, tc.code)
			}
			body := w.Body.String()
			wantNone(t, body, "asha", "secret detail", "db down")
			if tc.code == http.StatusInternalServerError {
				if strings.TrimSpace(body) != "internal error" {
					t.Errorf("body %q, want only internal error", body)
				}
				if !strings.Contains(tc.ts.logs.String(), "web: approvals") || !strings.Contains(tc.ts.logs.String(), `"action":"get"`) {
					t.Error("internal error not logged")
				}
			}
		})
	}
}

func TestProposalPageLinks(t *testing.T) {
	f := fid(7)
	withFinding := propAt(1, "posted", "asha", "ravi")
	withFinding.FindingID = &f
	noFinding := propAt(2, "proposed", "asha", "")
	hostile := propAt(3, "posted", "asha", "ravi")
	evilName := `JV"><b>1</b>`
	hostile.ERPName = &evilName
	ts, _ := proposalsServer(t, withFinding, noFinding, hostile)

	body := proposalBody(t, ts, RoleViewer, pid(1))
	wantAll(t, body, `href="/findings/`+f.String()+`"`, "View finding", "Back to finding", "ERPNext journal entry",
		`href="`+erpTest+`/app/journal-entry/ACC-JV-2026-00011" rel="noopener noreferrer" target="_blank"`)
	wantNone(t, body, "Back to approvals")

	// A checker goes back to the approvals list.
	wantAll(t, proposalBody(t, ts, RoleChecker, pid(1)), `href="/approvals"`, "Back to approvals")

	// No finding: no finding link and no back link for a viewer; a pending
	// proposal has no journal entry fact.
	body = proposalBody(t, ts, RoleViewer, pid(2))
	wantNone(t, body, `href="/findings/`, "View finding", "Back to", "ERPNext journal entry")

	body = proposalBody(t, ts, RoleViewer, pid(3))
	wantNone(t, body, "<b>1</b>", `JV">`)
	wantAll(t, body, `JV&#34;&gt;&lt;b&gt;1&lt;/b&gt;`, `/app/journal-entry/JV%22%3E%3Cb%3E1%3C%2Fb%3E" rel="noopener noreferrer" target="_blank"`)
}

func TestProposalPageEscaping(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>`
	const esc = `&lt;img src=x onerror=alert(1)&gt;`
	rej := propAt(1, "rejected", "asha"+evil, "ravi"+evil)
	rej.CompanyID, rej.Payload.Remark, rej.Payload.PostingDate = "co"+evil, "remark"+evil, "2026-08-31"+evil
	rej.Payload.Lines[0].Account = "acct" + evil
	reason := "reason" + evil
	rej.Reason = &reason
	post := propAt(2, "posted", "asha", "ravi")
	name := "erp" + evil
	post.ERPName = &name
	appr := propAt(3, "approved", "asha"+evil, "ravi"+evil)
	ts, _ := proposalsServer(t, rej, post, appr)

	cases := []struct {
		name  string
		id    uuid.UUID
		role  Role
		wants []string
	}{
		{"rejected", pid(1), RoleViewer, []string{"co", "remark", "acct", "by asha", "by ravi", "Reason: reason", "2026-08-31"}},
		{"posted", pid(2), RoleViewer, []string{"erp", "Journal entry erp"}},
		{"approved, as checker", pid(3), RoleChecker, []string{"by asha", "approved, posting failed; retry by ravi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := proposalBody(t, ts, tc.role, tc.id)
			wantNone(t, body, evil, "<img")
			for _, s := range tc.wants {
				wantAll(t, body, s+esc)
			}
		})
	}
}
