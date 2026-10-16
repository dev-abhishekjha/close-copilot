package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// lf is a finding of run 1 (acme 2026-09) with an explicit title.
func lf(n byte, typ, sev, status, title string) store.Finding {
	return store.Finding{ID: fid(n), RunID: runID(1), Type: typ, Severity: sev, Title: title,
		Status: status, Keys: map[string]string{}}
}

type listEnv struct {
	rs *fakeRunsStore
	ff *fakeFindings
	ts *testServer
}

// newListEnv serves run 1 (acme 2026-09) with fs in both the runs store and
// the findings store, so the list and a decision see the same rows.
func newListEnv(t testing.TB, fs ...store.Finding) listEnv {
	t.Helper()
	rs := newFakeRunsStore()
	ff := newFakeFindings()
	run := store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-09", Status: store.RunDone}
	rs.put(run, fs...)
	rs.list = []store.RunSummary{{CloseRun: run, Findings: len(fs)}}
	ff.runs[run.ID] = run
	for _, f := range fs {
		ff.findings[f.ID] = f
	}
	ts := findingsServer(t, ff, func(o *Options) {
		o.Runs = &RunsBackend{Store: rs, Execute: func(context.Context, uuid.UUID) error { return nil }, Findings: ff}
	})
	return listEnv{rs, ff, ts}
}

func (e listEnv) list(t testing.TB, role Role, query string) string {
	t.Helper()
	w := e.ts.get(t, role, "/findings"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /findings%s = %d: %s", query, w.Code, w.Body)
	}
	return w.Body.String()
}

var rowIDRE = regexp.MustCompile(`data-finding="([0-9a-f-]{36})"`)

// rowOrder is the finding ids in table order.
func rowOrder(body string) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range rowIDRE.FindAllStringSubmatch(body, -1) {
		out = append(out, uuid.MustParse(m[1]))
	}
	return out
}

func ids(ns ...byte) []uuid.UUID {
	var out []uuid.UUID
	for _, n := range ns {
		out = append(out, fid(n))
	}
	return out
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mixed() []store.Finding {
	a := lf(1, "bank_charge", "high", "open", "Alpha charge")
	a.AmountPaise = paise(500)
	b := lf(2, "gst_mismatch", "high", "needs_review", "bravo mismatch")
	b.AmountPaise = paise(900)
	b.Verified = true
	c := lf(3, "bank_charge", "medium", "accepted", "Charlie fee")
	c.AmountPaise = paise(100)
	d := lf(4, "variance", "low", "dismissed", "Delta variance")
	return []store.Finding{a, b, c, d}
}

func TestFindingsListContextRun(t *testing.T) {
	e := newListEnv(t, mixed()...)
	if got := rowOrder(e.list(t, RoleViewer, "")); len(got) != 4 {
		t.Fatalf("rows = %v", got)
	}
	// Another run for globex 2026-08 is reachable through the override only.
	other := store.CloseRun{ID: runID(2), CompanyID: "globex", Month: "2026-08", Status: store.RunDone}
	g := lf(9, "bank_charge", "high", "open", "Globex fee")
	g.RunID = other.ID
	e.rs.put(other, g)
	e.rs.list = append(e.rs.list, store.RunSummary{CloseRun: other})
	body := e.list(t, RoleViewer, "?company=globex&month=2026-08")
	if got := rowOrder(body); !sameIDs(got, ids(9)) || !strings.Contains(body, "Globex fee") {
		t.Errorf("override rows = %v", got)
	}
	// An unknown company and a bad month are ignored.
	body = e.list(t, RoleViewer, "?company=nope&month=2026-13")
	if got := rowOrder(body); len(got) != 4 {
		t.Errorf("ignored override rows = %v", got)
	}
	// No run: the empty state, and a link to /runs for makers only.
	body = e.list(t, RoleMaker, "?month=2025-01")
	wantAll(t, body, "No close run for acme 2025-01 yet", "Start a close")
	if len(rowOrder(body)) != 0 {
		t.Error("rows on the empty state")
	}
	wantNone(t, e.list(t, RoleViewer, "?month=2025-01"), "Start a close")
	// The newest run for the pair wins (the list is newest first).
	newer := store.CloseRun{ID: runID(3), CompanyID: "acme", Month: "2026-09", Status: store.RunDone}
	n := lf(8, "bank_charge", "high", "open", "Newer run fee")
	n.RunID = newer.ID
	e.rs.put(newer, n)
	e.rs.list = append([]store.RunSummary{{CloseRun: newer}}, e.rs.list...)
	if got := rowOrder(e.list(t, RoleViewer, "")); !sameIDs(got, ids(8)) {
		t.Errorf("newest run rows = %v", got)
	}
}

func TestFindingsListFilters(t *testing.T) {
	e := newListEnv(t, mixed()...)
	cases := []struct {
		name, query string
		want        []uuid.UUID
	}{
		{"severity", "?severity=high", ids(2, 1)},
		{"severity low", "?severity=low", ids(4)},
		{"type", "?type=bank_charge", ids(1, 3)},
		{"status", "?status=needs_review", ids(2)},
		{"verified yes", "?verified=yes", ids(2)},
		{"verified no", "?verified=no", ids(1, 3, 4)},
		{"q lower", "?q=ALPHA", ids(1)},
		{"q middle", "?q=mismatch", ids(2)},
		{"combined", "?severity=high&type=bank_charge&status=open", ids(1)},
		{"combined empty", "?severity=low&type=bank_charge", nil},
		{"unknown severity ignored", "?severity=critical", ids(2, 1, 3, 4)},
		{"unknown type ignored", "?type=zzz", ids(2, 1, 3, 4)},
		{"unknown status ignored", "?status=bogus", ids(2, 1, 3, 4)},
		{"unknown verified ignored", "?verified=maybe", ids(2, 1, 3, 4)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// High severity default order is by amount descending: 2 (900) then 1 (500).
			if got := rowOrder(e.list(t, RoleViewer, c.query)); !sameIDs(got, c.want) {
				t.Errorf("rows = %v, want %v", got, c.want)
			}
		})
	}
	// q is capped at 100 bytes: a title that only matches beyond the cap
	// still matches the capped prefix.
	long := strings.Repeat("a", 100)
	x := lf(5, "bank_charge", "low", "open", long+"tail")
	e2 := newListEnv(t, x)
	if got := rowOrder(e2.list(t, RoleViewer, "?q="+long+"zzzz")); !sameIDs(got, ids(5)) {
		t.Errorf("capped q rows = %v", got)
	}
	// Hostile q is escaped everywhere it is echoed.
	evil := `"><script>alert(1)</script>`
	body := e.list(t, RoleMaker, "?q="+url.QueryEscape(evil))
	wantNone(t, body, "<script>alert(1)")
	wantAll(t, body, "&lt;script&gt;")
}

func TestFindingsListSortPaging(t *testing.T) {
	e := newListEnv(t, mixed()...)
	cases := []struct {
		query string
		want  []uuid.UUID
	}{
		{"", ids(2, 1, 3, 4)},
		{"?sort=severity&dir=desc", ids(4, 3, 2, 1)},
		{"?sort=amount", ids(2, 1, 3, 4)},
		{"?sort=amount&dir=asc", ids(3, 1, 2, 4)},
		{"?sort=type", ids(2, 1, 3, 4)[:0:0]},
		{"?sort=type&dir=desc", nil},
		{"?sort=status", nil},
		{"?sort=title", ids(1, 2, 3, 4)},
		{"?sort=title&dir=desc", ids(4, 3, 2, 1)},
		{"?sort=bogus&dir=sideways", ids(2, 1, 3, 4)},
	}
	// type asc: bank_charge (1, 3 in default order), gst_mismatch (2), variance (4).
	cases[4].want = ids(1, 3, 2, 4)
	cases[5].want = ids(4, 2, 1, 3)
	// status asc: accepted(3) dismissed(4) needs_review(2) open(1).
	cases[6].want = ids(3, 4, 2, 1)
	for _, c := range cases {
		if got := rowOrder(e.list(t, RoleViewer, c.query)); !sameIDs(got, c.want) {
			t.Errorf("%q rows = %v, want %v", c.query, got, c.want)
		}
	}
	// A header link flips the direction of the sorted column and keeps the filters.
	body := e.list(t, RoleViewer, "?sort=title&dir=asc&severity=high")
	wantAll(t, body, `aria-sort="ascending"`, "dir=desc", "sort=title", "severity=high")

	// 60 findings: 25 per page, clamped, with links that keep the parameters.
	var many []store.Finding
	for i := 1; i <= 60; i++ {
		many = append(many, lf(byte(i), "bank_charge", "high", "open", fmt.Sprintf("fee %02d", i)))
	}
	m := newListEnv(t, many...)
	q := "?sort=title&type=bank_charge&q=fee"
	body = m.list(t, RoleViewer, q)
	wantAll(t, body, "Showing 1-25 of 60", "page=2", "sort=title", "type=bank_charge", "q=fee")
	wantNone(t, body, "Previous")
	body = m.list(t, RoleViewer, q+"&page=3")
	wantAll(t, body, "Showing 51-60 of 60", "Previous", "page=2")
	wantNone(t, body, ">Next<")
	if got := rowOrder(body); len(got) != 10 || got[0] != fid(51) {
		t.Errorf("page 3 rows = %d first %v", len(got), got[0])
	}
	wantAll(t, m.list(t, RoleViewer, q+"&page=99"), "Showing 51-60 of 60")
	wantAll(t, m.list(t, RoleViewer, q+"&page=-4"), "Showing 1-25 of 60")
	wantAll(t, m.list(t, RoleViewer, q+"&page=abc"), "Showing 1-25 of 60")
	// Equal sort keys keep the default order (stable).
	st := m.list(t, RoleViewer, "?sort=type")
	if got := rowOrder(st); got[0] != fid(1) || got[24] != fid(25) {
		t.Errorf("tie order = %v ... %v", got[0], got[24])
	}
}

func TestFindingsListNesting(t *testing.T) {
	parent := lf(1, "bank_charge", "high", "open", "Parent")
	child := lf(2, "variance", "low", "accepted", "Child variance")
	child.Keys = map[string]string{"related": fid(1).String()}
	e := newListEnv(t, parent, child)
	body := e.list(t, RoleMaker, "")
	if got := rowOrder(body); !sameIDs(got, ids(1, 2)) {
		t.Errorf("rows = %v", got)
	}
	wantAll(t, body, `data-nested="true"`, "Showing 1-1 of 1")
}

func TestFindingsListTabsCounts(t *testing.T) {
	e := newListEnv(t, mixed()...)
	body := e.list(t, RoleViewer, "")
	for _, tab := range []struct{ label, count string }{
		{"All", "4"}, {"Open", "1"}, {"Needs review", "1"}, {"Accepted", "1"}, {"Dismissed", "1"},
	} {
		re := regexp.MustCompile(`<span>` + tab.label + `</span> <span class="ui-tab-count">` + tab.count + `</span>`)
		if !re.MatchString(body) {
			t.Errorf("tab %s lacks count %s", tab.label, tab.count)
		}
	}
	body = e.list(t, RoleViewer, "?status=open")
	wantAll(t, body, `aria-current="page"`, "status=needs_review")
	// Other filters narrow the counts; the status tab does not.
	body = e.list(t, RoleViewer, "?type=bank_charge&status=open")
	if !regexp.MustCompile(`<span>All</span> <span class="ui-tab-count">2</span>`).MatchString(body) {
		t.Error("All count ignores the type filter")
	}
}

func TestFindingsListRoles(t *testing.T) {
	e := newListEnv(t, mixed()...)
	maker := e.list(t, RoleMaker, "")
	if n := strings.Count(maker, `name="ids"`); n != 2 {
		t.Errorf("maker checkboxes = %d, want 2 (open and needs_review)", n)
	}
	for _, id := range ids(1, 2) {
		wantAll(t, maker, `value="`+id.String()+`"`)
	}
	for _, id := range ids(3, 4) {
		wantNone(t, maker, `name="ids" value="`+id.String()+`"`)
	}
	wantAll(t, maker, "Mark selected accepted", "/findings/accept-selected", `name="csrf_token"`)
	for _, r := range []Role{RoleViewer, RoleChecker} {
		wantNone(t, e.list(t, r, ""), `name="ids"`, "Mark selected accepted", "accept-selected")
	}
	w := e.ts.do(t, req{method: http.MethodGet, path: "/findings"})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous GET = %d %s", w.Code, w.Header().Get("Location"))
	}
	w = e.ts.do(t, req{method: http.MethodPost, path: "/findings/accept-selected"})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous POST = %d", w.Code)
	}
	// The row hooks of the run findings page carry over.
	wantAll(t, maker, `<td class="severity`, `data-severity="high"`, "findings-table")
}

func (e listEnv) accept(t testing.TB, role Role, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return e.ts.do(t, formPost(e.ts, role, "/findings/accept-selected", fields))
}

func TestFindingsBulkAccept(t *testing.T) {
	a := lf(1, "bank_charge", "high", "open", "Alpha")
	b := lf(2, "bank_charge", "high", "needs_review", "Bravo")
	c := lf(3, "bank_charge", "high", "open", "Charlie live")
	d := lf(4, "bank_charge", "high", "accepted", "Delta done")
	f := lf(5, "bank_charge", "high", "open", "Echo")
	e := newListEnv(t, a, b, c, d, f)
	e.ff.setLive(c.ID, store.JournalProposal{Status: "proposed"})

	w := e.accept(t, RoleMaker, url.Values{
		"ids":     {a.ID.String(), b.ID.String(), c.ID.String(), d.ID.String(), f.ID.String()},
		"company": {"acme"}, "month": {"2026-09"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	wantAll(t, body, "3 accepted", "2 not accepted",
		"Charlie live: this finding has a live proposal, so it can&#39;t be accepted or dismissed", "Delta done: this finding is already accepted")
	for _, id := range []uuid.UUID{a.ID, b.ID, f.ID} {
		if e.ff.status(id) != "accepted" {
			t.Errorf("%s = %s", id, e.ff.status(id))
		}
	}
	if e.ff.status(c.ID) != "open" || e.ff.status(d.ID) != "accepted" {
		t.Error("refused findings changed")
	}
	if e.ff.auditCount() != 3 {
		t.Fatalf("audit rows = %d, want one per accepted finding", e.ff.auditCount())
	}
	for i, ent := range e.ff.audits {
		if ent.Tool == nil || *ent.Tool != "finding.accept" || ent.Actor == "" {
			t.Errorf("audit %d = %+v", i, ent)
		}
		if !strings.Contains(string(ent.Args), `"to":"accepted"`) || !strings.Contains(string(ent.Args), `"run_id":"`+runID(1).String()+`"`) {
			t.Errorf("audit %d args = %s", i, ent.Args)
		}
	}
	// A store error on one finding does not stop the rest.
	e2 := newListEnv(t, lf(1, "x", "high", "open", "One"), lf(2, "x", "high", "open", "Two"))
	e2.ff.setErr = fmt.Errorf("db down")
	w = e2.accept(t, RoleMaker, url.Values{"ids": {fid(1).String(), fid(2).String()}})
	wantAll(t, w.Body.String(), "0 accepted", "2 not accepted", "One: could not be saved", "Two: could not be saved")
	wantNone(t, w.Body.String(), "db down")
}

func TestFindingsBulkAcceptValidation(t *testing.T) {
	a := lf(1, "x", "high", "open", "Alpha")
	e := newListEnv(t, a)
	// An open finding of another run.
	other := store.CloseRun{ID: runID(2), CompanyID: "globex", Month: "2026-08", Status: store.RunDone}
	o := lf(9, "x", "high", "open", "Other run")
	o.RunID = other.ID
	e.rs.put(other, o)
	e.ff.runs[other.ID] = other
	e.ff.findings[o.ID] = o

	untouched := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		if e.ff.status(id) != "open" || e.ff.auditCount() != 0 {
			t.Errorf("state changed: %s audits %d", e.ff.status(id), e.ff.auditCount())
		}
	}
	t.Run("zero ids", func(t *testing.T) {
		w := e.accept(t, RoleMaker, nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Select at least one finding") {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
		untouched(t, a.ID)
	})
	t.Run("too many", func(t *testing.T) {
		v := url.Values{}
		for i := 0; i < 51; i++ {
			v.Add("ids", uuid.New().String())
		}
		v.Add("ids", a.ID.String())
		w := e.accept(t, RoleMaker, v)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "at most 50") {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
		untouched(t, a.ID)
	})
	t.Run("exactly 50 is fine", func(t *testing.T) {
		v := url.Values{}
		for i := 0; i < 49; i++ {
			v.Add("ids", uuid.New().String())
		}
		v.Add("ids", a.ID.String())
		if w := e.accept(t, RoleMaker, v); w.Code != 200 {
			t.Errorf("= %d", w.Code)
		}
		e.ff.mu.Lock()
		e.ff.findings[a.ID] = a // put it back for the next cases
		e.ff.audits = nil
		e.ff.mu.Unlock()
	})
	t.Run("bad uuid", func(t *testing.T) {
		w := e.accept(t, RoleMaker, url.Values{"ids": {a.ID.String(), "not-a-uuid"}})
		if w.Code != 400 {
			t.Errorf("= %d", w.Code)
		}
		untouched(t, a.ID)
	})
	t.Run("other run", func(t *testing.T) {
		w := e.accept(t, RoleMaker, url.Values{"ids": {o.ID.String()}, "company": {"acme"}, "month": {"2026-09"}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), "not in this view") || !strings.Contains(w.Body.String(), "0 accepted") {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
		untouched(t, o.ID)
	})
	t.Run("posted company and month pick the run", func(t *testing.T) {
		// The id is valid for globex 2026-08, and only there.
		e.rs.list = append(e.rs.list, store.RunSummary{CloseRun: other})
		w := e.accept(t, RoleMaker, url.Values{"ids": {o.ID.String()}, "company": {"globex"}, "month": {"2026-08"}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), "1 accepted") {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
		e.ff.mu.Lock()
		e.ff.findings[o.ID] = o
		e.ff.audits = nil
		e.ff.mu.Unlock()
	})
	t.Run("duplicate", func(t *testing.T) {
		w := e.accept(t, RoleMaker, url.Values{"ids": {a.ID.String(), a.ID.String()}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), "1 accepted") || strings.Contains(w.Body.String(), "not accepted") {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
		if e.ff.auditCount() != 1 {
			t.Errorf("audit rows = %d, want 1", e.ff.auditCount())
		}
	})
	t.Run("no run", func(t *testing.T) {
		w := e.accept(t, RoleMaker, url.Values{"ids": {a.ID.String()}, "month": {"2020-01"}})
		if w.Code != 400 {
			t.Errorf("= %d", w.Code)
		}
	})
}

func TestFindingsBulkAcceptSecurity(t *testing.T) {
	a := lf(1, "x", "high", "open", "Alpha")
	e := newListEnv(t, a)
	noWrite := func(t *testing.T) {
		t.Helper()
		if e.ff.status(a.ID) != "open" || e.ff.auditCount() != 0 || len(e.ff.sets) != 0 {
			t.Errorf("a write happened: %s %d %v", e.ff.status(a.ID), e.ff.auditCount(), e.ff.sets)
		}
	}
	form := url.Values{"ids": {a.ID.String()}}
	t.Run("no csrf", func(t *testing.T) {
		ck, _ := e.ts.session(RoleMaker)
		w := e.ts.do(t, req{method: http.MethodPost, path: "/findings/accept-selected", body: form.Encode(),
			contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}})
		if w.Code != 403 {
			t.Errorf("= %d", w.Code)
		}
		noWrite(t)
	})
	t.Run("cross site", func(t *testing.T) {
		rq := formPost(e.ts, RoleMaker, "/findings/accept-selected", form)
		rq.header = map[string]string{"Sec-Fetch-Site": "cross-site"}
		if w := e.ts.do(t, rq); w.Code != 403 {
			t.Errorf("= %d", w.Code)
		}
		rq.header = map[string]string{"Origin": "https://evil.example"}
		if w := e.ts.do(t, rq); w.Code != 403 {
			t.Errorf("origin = %d", w.Code)
		}
		noWrite(t)
	})
	t.Run("roles", func(t *testing.T) {
		for _, r := range []Role{RoleViewer, RoleChecker} {
			if w := e.accept(t, r, form); w.Code != 403 {
				t.Errorf("%s = %d", r, w.Code)
			}
		}
		noWrite(t)
	})
	t.Run("return", func(t *testing.T) {
		// Only a /findings? path is honoured: the list shows its filters.
		ret := "/findings?" + url.Values{"company": {"acme"}, "month": {"2026-09"}, "type": {"x"}}.Encode()
		f := url.Values{"ids": {a.ID.String()}, "return": {ret}}
		body := e.accept(t, RoleMaker, f).Body.String()
		wantAll(t, body, `name="return"`, "type=x")
		for _, bad := range []string{"//evil.example/findings?x=1", "https://evil.example", "/runs?x=1", `/findings?"><script>alert(1)</script>`, "/findings?%0d%0a"} {
			e.ff.mu.Lock()
			e.ff.findings[a.ID] = a
			e.ff.mu.Unlock()
			w := e.accept(t, RoleMaker, url.Values{"ids": {a.ID.String()}, "return": {bad}})
			b := w.Body.String()
			wantNone(t, b, "evil.example", "<script>alert(1)", `value="/runs`)
			if w.Header().Get("Location") != "" {
				t.Errorf("redirect for %q", bad)
			}
		}
	})
}

func TestFindingsBulkAcceptNoSideChannel(t *testing.T) {
	a := lf(1, "x", "high", "open", "Alpha secret title")
	e := newListEnv(t, a, lf(2, "x", "high", "accepted", "Beta"))
	w := e.accept(t, RoleMaker, url.Values{"ids": {a.ID.String(), fid(2).String()}})
	if w.Code != 200 {
		t.Fatalf("= %d", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("cookies set: %v", w.Result().Cookies())
	}
	for k, v := range w.Header() {
		switch k {
		case "Content-Type", "Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Cache-Control", "Vary", "Permissions-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Strict-Transport-Security":
			continue
		}
		if strings.Contains(strings.Join(v, ","), "Alpha") || strings.Contains(strings.Join(v, ","), "accepted") {
			t.Errorf("header %s carries the result: %v", k, v)
		}
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q", loc)
	}
	// The next GET shows no trace of the result.
	body := e.list(t, RoleMaker, "")
	wantNone(t, body, "Bulk accept result", "1 accepted")
}

func TestFindingsListNotConfigured(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"nil findings": func(o *Options) {
			o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		},
		"nil runs": func(o *Options) { o.Runs = nil },
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, mod)
			if c := ts.get(t, RoleViewer, "/findings").Code; c != http.StatusNotImplemented {
				t.Errorf("GET = %d", c)
			}
			if c := ts.do(t, formPost(ts, RoleMaker, "/findings/accept-selected", url.Values{"ids": {fid(1).String()}})).Code; c != http.StatusNotImplemented {
				t.Errorf("POST = %d", c)
			}
			if c := ts.do(t, formPost(ts, RoleViewer, "/findings/accept-selected", nil)).Code; c != http.StatusForbidden {
				t.Errorf("viewer POST = %d (role check comes first)", c)
			}
		})
	}
}

func TestFindingsListEscaping(t *testing.T) {
	evil := `<script>alert(1)</script>`
	x := lf(1, evil, "high", "open", evil)
	e := newListEnv(t, x)
	body := e.list(t, RoleMaker, "?q="+url.QueryEscape(evil)+"&type="+url.QueryEscape(evil)+"&company="+url.QueryEscape(evil))
	wantNone(t, body, "<script>alert(1)")
	body = e.list(t, RoleMaker, "")
	wantNone(t, body, "<script>alert(1)")
	wantAll(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
	// The result card escapes refusal titles too.
	y := lf(2, "x", "high", "accepted", evil)
	e2 := newListEnv(t, y)
	w := e2.accept(t, RoleMaker, url.Values{"ids": {y.ID.String()}})
	wantNone(t, w.Body.String(), "<script>alert(1)")
	wantAll(t, w.Body.String(), "&lt;script&gt;")
	// An empty-state company can't be forged into the page.
	wantNone(t, e.list(t, RoleMaker, "?company="+url.QueryEscape(evil)+"&month=2020-01"), "<script>alert(1)")
}

func TestFindingsListRoutesTable(t *testing.T) {
	s := &Server{}
	rows := s.findingsListRoutes()
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].method != http.MethodGet || rows[0].path != "/findings" || len(rows[0].roles) != 3 {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].method != http.MethodPost || rows[1].path != "/findings/accept-selected" || len(rows[1].roles) != 1 || rows[1].roles[0] != RoleMaker {
		t.Errorf("row 1 = %+v", rows[1])
	}
}

// The filter form carries the company, month, status and sort so a filter
// change does not lose them.
func TestFindingsListFilterFormKeepsState(t *testing.T) {
	e := newListEnv(t, mixed()...)
	body := e.list(t, RoleViewer, "?status=open&sort=title&dir=desc")
	wantAll(t, body, `name="company" value="acme"`, `name="month" value="2026-09"`,
		`name="status" value="open"`, `name="sort" value="title"`, `name="dir" value="desc"`)
	wantNone(t, e.list(t, RoleViewer, ""), `name="status"`, `name="sort"`)
}

// The bulk form carries the token, the company and month, and a return
// value that is the current /findings? query.
func TestFindingsListBulkFormFields(t *testing.T) {
	e := newListEnv(t, mixed()...)
	body := e.list(t, RoleMaker, "?severity=high&page=1")
	wantAll(t, body, `name="return" value="/findings?`, "severity=high", `name="csrf_token"`,
		`action="/findings/accept-selected"`)
	// Checkbox labels name the finding.
	wantAll(t, body, `aria-label="Select Alpha charge"`)
}

// A POST to /findings/accept-selected reaches the bulk handler, not the
// {id}/accept pattern, and a GET of it is not the list.
func TestFindingsListMuxResolution(t *testing.T) {
	e := newListEnv(t, mixed()...)
	w := e.accept(t, RoleMaker, url.Values{"ids": {fid(1).String()}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Bulk accept result") {
		t.Errorf("POST = %d", w.Code)
	}
	if c := e.ts.get(t, RoleMaker, "/findings/accept-selected").Code; c != http.StatusNotFound {
		t.Errorf("GET /findings/accept-selected = %d, want 404", c)
	}
}
