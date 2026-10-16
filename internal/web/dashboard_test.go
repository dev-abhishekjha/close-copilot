package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// dashStore counts the calls the dashboard makes and can fail each one.
type dashStore struct {
	*fakeRunsStore
	mu                              sync.Mutex
	runsCalls, compCalls, findCalls int
	runsErr, findErr                error
}

func (d *dashStore) ListCloseRuns(ctx context.Context, n int) ([]store.RunSummary, error) {
	d.mu.Lock()
	d.runsCalls++
	err := d.runsErr
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.fakeRunsStore.ListCloseRuns(ctx, n)
}

func (d *dashStore) ListCompanies(ctx context.Context) ([]store.Company, error) {
	d.mu.Lock()
	d.compCalls++
	d.mu.Unlock()
	return d.fakeRunsStore.ListCompanies(ctx)
}

func (d *dashStore) ListFindingsByRun(ctx context.Context, id uuid.UUID) ([]store.Finding, error) {
	d.mu.Lock()
	d.findCalls++
	err := d.findErr
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.fakeRunsStore.ListFindingsByRun(ctx, id)
}

func (d *dashStore) counts() (runs, comps, finds int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runsCalls, d.compCalls, d.findCalls
}

func dashServer(t testing.TB, ds *dashStore, fa *fakeApprovals) *testServer {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		if ds != nil {
			o.Runs = &RunsBackend{Store: ds, Execute: noopExec}
		}
		if fa != nil {
			o.Approvals = fa
		}
	})
}

func newDashStore() *dashStore { return &dashStore{fakeRunsStore: newFakeRunsStore()} }

var dashT0 = time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)

// addRun adds a run to the list (newest first is the caller's order).
func (d *dashStore) addRun(id byte, company, month, status string, started *time.Time, fs ...store.Finding) store.RunSummary {
	r := store.CloseRun{ID: runID(id), CompanyID: company, Month: month, Status: status, StartedAt: started}
	d.put(r, fs...)
	rs := store.RunSummary{CloseRun: r, Findings: len(fs)}
	d.list = append(d.list, rs)
	return rs
}

func dashFinding(run uuid.UUID, n byte, sev, status string, verified bool) store.Finding {
	f := finding(run, n, "bank_charge", sev, nil, status)
	f.Verified = verified
	return f
}

func dashBody(t testing.TB, ts *testServer, role Role, cookies ...*http.Cookie) string {
	t.Helper()
	ck, _ := ts.session(role)
	return okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: append([]*http.Cookie{ck}, cookies...)})
}

func dashMain(t testing.TB, ts *testServer, role Role, cookies ...*http.Cookie) string {
	t.Helper()
	return mainContent(t, dashBody(t, ts, role, cookies...))
}

func TestDashboardContextSummary(t *testing.T) {
	ds := newDashStore()
	ds.companies = []store.Company{{ID: "acme"}, {ID: "globex"}}
	run := runID(1)
	ds.addRun(1, "acme", "2026-09", "done", &dashT0,
		dashFinding(run, 1, "high", "open", true),
		dashFinding(run, 2, "high", "needs_review", false),
		dashFinding(run, 3, "medium", "open", false),
		dashFinding(run, 4, "low", "accepted", true),
		dashFinding(run, 5, "low", "dismissed", true),
	)
	ts := dashServer(t, ds, nil)
	main := dashMain(t, ts, RoleViewer)
	if !strings.Contains(main, "acme 2026-09") {
		t.Errorf("subtitle lacks the context: %s", main)
	}
	tile := func(label string) string {
		i := strings.Index(main, `<span class="ui-stat-label">`+label+`</span>`)
		if i < 0 {
			t.Fatalf("no %q tile", label)
		}
		return main[i : i+160]
	}
	for label, want := range map[string]string{"Findings": ">5<", "High severity": ">2<", "Needs review": ">1<", "Unverified": ">2<"} {
		if !strings.Contains(tile(label), want) {
			t.Errorf("%s tile = %s, want %s", label, tile(label), want)
		}
	}
	for sev, want := range map[string]string{"high": `max="5" value="2"`, "medium": `max="5" value="1"`, "low": `max="5" value="2"`} {
		row := between(t, main, `data-severity="`+sev+`"`, "</li>")
		if !strings.Contains(row, "<meter") || !strings.Contains(row, want) {
			t.Errorf("severity %s row = %s, want %s", sev, row, want)
		}
	}
	for status, n := range map[string]int{"open": 2, "needs_review": 1, "accepted": 1, "dismissed": 1} {
		row := between(t, main, `data-status="`+status+`"`, "</tr>")
		if !strings.Contains(row, fmt.Sprintf(">%d<", n)) {
			t.Errorf("status %s row = %s, want count %d", status, row, n)
		}
	}
	if !strings.Contains(main, `href="/runs/`+run.String()+`"`) {
		t.Error("status tile does not link to the run")
	}

	// A hostile company name is escaped everywhere it shows.
	ds2 := newDashStore()
	ds2.companies = []store.Company{{ID: `<script>x</script>`}}
	ds2.addRun(2, `<script>x</script>`, "2026-09", "done", &dashT0)
	body := dashBody(t, dashServer(t, ds2, nil), RoleViewer)
	if strings.Contains(body, "<script>x") || !strings.Contains(body, "&lt;script&gt;x") {
		t.Errorf("hostile company not escaped: %s", mainContent(t, body))
	}
}

func TestDashboardLatestPerCompany(t *testing.T) {
	ds := newDashStore()
	ds.companies = []store.Company{{ID: "acme"}, {ID: "globex"}, {ID: "idle"}}
	later := dashT0.Add(time.Hour)
	ds.addRun(1, "globex", "2026-09", "running", nil)
	ds.addRun(2, "acme", "2026-09", "done", &later, dashFinding(runID(2), 1, "high", "open", true))
	ds.addRun(3, "acme", "2026-08", "failed", &dashT0)
	ts := dashServer(t, ds, nil)
	main := dashMain(t, ts, RoleViewer)
	acme := between(t, main, `data-company="acme"`, "</tr>")
	if !strings.Contains(acme, `href="/runs/`+runID(2).String()+`"`) || strings.Contains(acme, runID(3).String()) || !strings.Contains(acme, "2026-10-01 09:30") {
		t.Errorf("acme row is not its newest run: %s", acme)
	}
	if g := between(t, main, `data-company="globex"`, "</tr>"); !strings.Contains(g, "Queued") {
		t.Errorf("globex row lacks Queued: %s", g)
	}
	if idle := between(t, main, `data-company="idle"`, "</tr>"); !strings.Contains(idle, "No runs yet") {
		t.Errorf("idle row lacks 'No runs yet': %s", idle)
	}
	if strings.Contains(main, " more</p>") {
		t.Error("'and N more' shown for 3 companies")
	}

	// 15 companies: 12 shown, "and 3 more".
	many := newDashStore()
	many.companies = nil
	for i := range 15 {
		many.companies = append(many.companies, store.Company{ID: fmt.Sprintf("co%02d", i)})
	}
	ts = dashServer(t, many, nil)
	main = dashMain(t, ts, RoleViewer)
	if n := strings.Count(main, `data-company="`); n != 12 {
		t.Errorf("%d company rows, want 12", n)
	}
	if !strings.Contains(main, "and 3 more") {
		t.Errorf("no 'and 3 more': %s", main)
	}
}

func TestDashboardWaitingByRole(t *testing.T) {
	ds := newDashStore()
	run := runID(1)
	ds.addRun(1, "acme", "2026-09", "done", &dashT0,
		dashFinding(run, 1, "high", "open", true),
		dashFinding(run, 2, "low", "needs_review", true),
		dashFinding(run, 3, "low", "accepted", true),
	)
	fa := &fakeApprovals{pending: []store.JournalProposal{
		{ID: pid(1), CompanyID: "acme", Maker: "asha"}, {ID: pid(2), CompanyID: "acme", Maker: "asha"},
		{ID: pid(3), CompanyID: "acme", Maker: "asha"}, {ID: pid(4), CompanyID: "acme", Maker: "asha"},
		{ID: pid(5), CompanyID: "acme", Maker: "asha"}, {ID: pid(6), CompanyID: "acme", Maker: "asha"},
	}}
	tests := []struct {
		role  Role
		want  []string
		deny  []string
		items int
	}{
		{RoleChecker, []string{"6</strong> proposals await your decision", "Review pending proposals"}, []string{"title bank_charge1"}, 5},
		{RoleMaker, []string{"2</strong> findings are still open or need review", "title bank_charge1", "title bank_charge2", "/runs/" + run.String() + "/findings"}, []string{"title bank_charge3", "proposals await"}, 2},
		{RoleViewer, []string{"Nothing is waiting on you"}, []string{"title bank_charge1", "proposals await"}, 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			ds.findCalls, ds.runsCalls = 0, 0
			ts := dashServer(t, ds, fa)
			main := dashMain(t, ts, tt.role)
			card := between(t, main, `id="dash-waiting"`, "</section>")
			for _, w := range tt.want {
				if !strings.Contains(card, w) {
					t.Errorf("waiting card lacks %q: %s", w, card)
				}
			}
			for _, w := range tt.deny {
				if strings.Contains(card, w) {
					t.Errorf("waiting card has %q: %s", w, card)
				}
			}
			if n := strings.Count(card, `<li><a href=`); n != tt.items {
				t.Errorf("%d linked items, want %d", n, tt.items)
			}
			if _, _, finds := ds.counts(); finds != 1 {
				t.Errorf("ListFindingsByRun called %d times, want 1", finds)
			}
		})
	}
}

// pendingCounter counts Pending calls.
type pendingCounter struct {
	*fakeApprovals
	mu sync.Mutex
	n  int
}

func (p *pendingCounter) Pending(ctx context.Context) ([]store.JournalProposal, error) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return p.fakeApprovals.Pending(ctx)
}

func TestDashboardOneCallPerSource(t *testing.T) {
	ds := newDashStore()
	ds.addRun(1, "acme", "2026-09", "done", &dashT0, dashFinding(runID(1), 1, "high", "open", true))
	pc := &pendingCounter{fakeApprovals: &fakeApprovals{pending: []store.JournalProposal{{ID: pid(1), CompanyID: "acme", Maker: "asha"}}}}
	ts := newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: ds, Execute: noopExec}
		o.Approvals = pc
	})
	dashMain(t, ts, RoleChecker)
	if pc.n != 1 {
		t.Errorf("Pending called %d times, want 1", pc.n)
	}
	if runs, _, finds := ds.counts(); runs != 1 || finds != 1 {
		t.Errorf("runs=%d findings=%d calls, want 1 and 1", runs, finds)
	}
}

func TestDashboardNotConfigured(t *testing.T) {
	const off = "Close runs are not configured on this server"
	tests := []struct {
		name string
		ds   *dashStore
		fa   *fakeApprovals
		role Role
		want int // occurrences of the empty state
	}{
		{"no runs, no approvals, checker", nil, nil, RoleChecker, 3},
		{"no runs, maker", nil, &fakeApprovals{}, RoleMaker, 3},
		{"no runs, viewer", nil, nil, RoleViewer, 2},
		{"runs but no approvals, checker", newDashStore(), nil, RoleChecker, 1},
		{"runs but no approvals, maker", newDashStore(), nil, RoleMaker, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := dashServer(t, tt.ds, tt.fa)
			ck, _ := ts.session(tt.role)
			w := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
			if w.Code != http.StatusOK {
				t.Fatalf("GET / = %d", w.Code)
			}
			main := mainContent(t, w.Body.String())
			if n := strings.Count(main, off); n != tt.want {
				t.Errorf("%d empty states, want %d: %s", n, tt.want, main)
			}
			if !strings.Contains(main, "Quick actions") {
				t.Error("quick actions missing")
			}
		})
	}
}

func TestDashboardSectionErrors(t *testing.T) {
	const generic = "This section could not be loaded"
	secret := errors.New("pq: password authentication failed for user secretuser")

	t.Run("list runs fails", func(t *testing.T) {
		ds := newDashStore()
		ds.runsErr = secret
		fa := &fakeApprovals{pending: []store.JournalProposal{{ID: pid(1), CompanyID: "acme", Maker: "asha"}}}
		ts := dashServer(t, ds, fa)
		main := dashMain(t, ts, RoleChecker)
		if n := strings.Count(main, generic); n != 2 {
			t.Errorf("%d error states, want 2 (context, latest): %s", n, main)
		}
		if !strings.Contains(main, "1</strong> proposal awaits") || !strings.Contains(main, "Quick actions") {
			t.Errorf("other sections did not render: %s", main)
		}
		if strings.Contains(main, "secretuser") {
			t.Error("cause leaked to the page")
		}
		if !strings.Contains(ts.logs.String(), "secretuser") {
			t.Error("cause not logged")
		}
	})
	t.Run("findings fail", func(t *testing.T) {
		ds := newDashStore()
		ds.addRun(1, "acme", "2026-09", "done", &dashT0, dashFinding(runID(1), 1, "high", "open", true))
		ds.findErr = secret
		ts := dashServer(t, ds, nil)
		main := dashMain(t, ts, RoleMaker)
		if n := strings.Count(main, generic); n != 2 {
			t.Errorf("%d error states, want 2 (context findings, waiting): %s", n, main)
		}
		if !strings.Contains(main, `id="dash-latest"`) || !strings.Contains(main, "acme") || strings.Contains(main, "secretuser") {
			t.Errorf("latest section broken or leaking: %s", main)
		}
		if !strings.Contains(ts.logs.String(), "secretuser") {
			t.Error("cause not logged")
		}
	})
	t.Run("pending fails", func(t *testing.T) {
		ds := newDashStore()
		ds.addRun(1, "acme", "2026-09", "done", &dashT0, dashFinding(runID(1), 1, "high", "open", true))
		fa := &fakeApprovals{pendingErr: secret}
		ts := dashServer(t, ds, fa)
		main := dashMain(t, ts, RoleChecker)
		if n := strings.Count(main, generic); n != 1 {
			t.Errorf("%d error states, want 1: %s", n, main)
		}
		if !strings.Contains(main, "High severity") || strings.Contains(main, "secretuser") {
			t.Errorf("rest of the page broken or leaking: %s", main)
		}
		if !strings.Contains(ts.logs.String(), "secretuser") {
			t.Error("cause not logged")
		}
	})
}

func TestDashboardQuickActions(t *testing.T) {
	ds := newDashStore()
	tests := []struct {
		role      Role
		want      []string
		deny      []string
		start     bool
		askButton bool
	}{
		{RoleViewer, []string{"view-run", "view-findings", "ask"}, []string{"start-run", "approvals", "audit"}, false, true},
		{RoleMaker, []string{"view-run", "ask", "start-run", "propose-entry"}, []string{"approve-proposal", "audit"}, true, true},
		{RoleChecker, []string{"view-run", "approvals", "audit", "approve-proposal"}, []string{"start-run"}, false, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			ts := dashServer(t, ds, &fakeApprovals{})
			main := dashMain(t, ts, tt.role)
			for _, id := range tt.want {
				if !strings.Contains(main, `data-action="`+id+`"`) {
					t.Errorf("lacks action %s", id)
				}
			}
			for _, id := range tt.deny {
				if strings.Contains(main, `data-action="`+id+`"`) {
					t.Errorf("shows action %s", id)
				}
			}
			hasStart := strings.Contains(main, "Start a close")
			if hasStart != tt.start {
				t.Errorf("Start button shown = %v, want %v", hasStart, tt.start)
			}
			if got := strings.Contains(main, `>Ask a question</span>`) || strings.Contains(main, "Ask a question<"); got != tt.askButton {
				t.Errorf("Ask button shown = %v, want %v", got, tt.askButton)
			}
		})
	}

	// The Start link carries the context, query-escaped.
	ds2 := newDashStore()
	ds2.companies = []store.Company{{ID: "a&b=c d"}}
	ts := dashServer(t, ds2, nil)
	ck := &http.Cookie{Name: ctxCookieName, Value: "company=a%26b%3Dc+d&month=2026-07"}
	main := dashMain(t, ts, RoleMaker, ck)
	if !strings.Contains(main, `href="/runs?company=a%26b%3Dc+d&amp;month=2026-07"`) {
		t.Errorf("Start link is not the escaped context link: %s", main)
	}
}

func TestDashboardNoQueryCountGrowth(t *testing.T) {
	ds := newDashStore()
	ds.companies = nil
	for i := range 12 {
		ds.companies = append(ds.companies, store.Company{ID: fmt.Sprintf("co%02d", i)})
	}
	for i := range 40 {
		ds.addRun(byte(i+1), fmt.Sprintf("co%02d", i%12), fmt.Sprintf("2026-%02d", i%9+1), "done", &dashT0,
			dashFinding(runID(byte(i+1)), byte(i+1), "high", "open", true))
	}
	ts := dashServer(t, ds, &fakeApprovals{})
	dashMain(t, ts, RoleMaker)
	if runs, comps, finds := ds.counts(); runs != 1 || comps != 1 || finds != 0 && finds != 1 {
		t.Errorf("ListCloseRuns=%d ListCompanies=%d ListFindingsByRun=%d, want 1, 1, <=1", runs, comps, finds)
	}
	// The default context (2026-09) has a run for co08 only if it is first;
	// make the context one with a run and check exactly one findings call.
	ds2 := newDashStore()
	for i := range 40 {
		ds2.addRun(byte(i+1), "acme", "2026-09", "done", &dashT0, dashFinding(runID(byte(i+1)), byte(i+1), "high", "open", true))
	}
	ts = dashServer(t, ds2, nil)
	dashMain(t, ts, RoleViewer)
	if runs, comps, finds := ds2.counts(); runs != 1 || comps != 1 || finds != 1 {
		t.Errorf("ListCloseRuns=%d ListCompanies=%d ListFindingsByRun=%d, want 1, 1, 1", runs, comps, finds)
	}
}

func TestDashboardNoAPIPaths(t *testing.T) {
	ds := newDashStore()
	ds.addRun(1, "acme", "2026-09", "done", &dashT0, dashFinding(runID(1), 1, "high", "open", true))
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		t.Run(string(role), func(t *testing.T) {
			ts := dashServer(t, ds, &fakeApprovals{})
			main := dashMain(t, ts, role)
			if strings.Contains(main, "/api/") || strings.Contains(main, "<code>") {
				t.Errorf("dashboard shows API paths: %s", main)
			}
			for _, m := range []string{"POST /", "GET /"} {
				if strings.Contains(main, m) {
					t.Errorf("dashboard shows a method and path %q", m)
				}
			}
		})
	}
}

func TestDashboardEscaping(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>`
	ds := newDashStore()
	ds.companies = []store.Company{{ID: "acme"}}
	run := runID(1)
	ds.addRun(1, "acme", "2026-09", "failed", &dashT0, func() store.Finding {
		f := dashFinding(run, 1, "high", "open", true)
		f.Title = evil
		return f
	}())
	ds.list[0].Error = &[]string{evil}[0]
	ts := dashServer(t, ds, &fakeApprovals{pending: []store.JournalProposal{{ID: pid(1), CompanyID: evil, Maker: evil}}})
	for _, role := range []Role{RoleMaker, RoleChecker} {
		main := dashMain(t, ts, role, &http.Cookie{Name: ctxCookieName, Value: "company=acme&month=2026-09"})
		if strings.Contains(main, "<img") || strings.Contains(main, "onerror=alert(1)>") {
			t.Errorf("%s: hostile text appears raw: %s", role, main)
		}
	}
	// A hostile month in the cookie falls back to the default, never echoed.
	main := dashMain(t, ts, RoleViewer, &http.Cookie{Name: ctxCookieName, Value: "company=acme&month=" + `%3Cscript%3E`})
	if strings.Contains(main, "<script") {
		t.Errorf("hostile month appears raw: %s", main)
	}
}
