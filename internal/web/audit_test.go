package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// fakeAudit is an in-memory AuditStore with the filter and keyset
// semantics of store.ListAuditPage.
type fakeAudit struct {
	mu      sync.Mutex
	rows    []store.AuditLogEntry
	steps   map[uuid.UUID][]store.StepUsage
	stats   store.LastRunStats
	failAll error
}

func newFakeAudit() *fakeAudit { return &fakeAudit{steps: map[uuid.UUID][]store.StepUsage{}} }

func (f *fakeAudit) add(e store.AuditLogEntry) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = int64(len(f.rows) + 1)
	if e.At.IsZero() {
		e.At = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(e.ID) * time.Second)
	}
	f.rows = append(f.rows, e)
	return e.ID
}

func (f *fakeAudit) ListAuditPage(_ context.Context, fl store.AuditFilter) ([]store.AuditLogEntry, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, 0, f.failAll
	}
	limit := fl.Limit
	if limit <= 0 {
		limit = 50
	}
	var out []store.AuditLogEntry
	for _, e := range f.rows {
		switch {
		case fl.Actor != "" && e.Actor != fl.Actor,
			fl.Server != "" && (e.Server == nil || *e.Server != fl.Server),
			fl.Tool != "" && (e.Tool == nil || *e.Tool != fl.Tool),
			fl.From != nil && e.At.Before(*fl.From),
			fl.To != nil && !e.At.Before(*fl.To),
			fl.Before > 0 && e.ID >= fl.Before:
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b store.AuditLogEntry) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		return int(b.ID - a.ID)
	})
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

func (f *fakeAudit) ListAuditFacets(context.Context) (actors, servers, tools []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	add := func(l *[]string, v string) {
		if v != "" && !slices.Contains(*l, v) {
			*l = append(*l, v)
		}
	}
	for _, e := range f.rows {
		add(&actors, e.Actor)
		add(&servers, derefStr(e.Server))
		add(&tools, derefStr(e.Tool))
	}
	slices.Sort(actors)
	slices.Sort(servers)
	slices.Sort(tools)
	return actors, servers, tools, nil
}

func (f *fakeAudit) RunMetricsByStep(_ context.Context, id uuid.UUID) ([]store.StepUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps[id], f.failAll
}

func (f *fakeAudit) RunsSummaryStats(context.Context) (store.LastRunStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, f.failAll
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

func auditServer(t testing.TB, fa *fakeAudit) *testServer {
	t.Helper()
	return newTestServer(t, func(o *Options) { o.Audit = fa })
}

func TestAuditPageRender(t *testing.T) {
	fa := newFakeAudit()
	long := `{"q":"` + strings.Repeat("é", 200) + `"}`
	fa.add(store.AuditLogEntry{Actor: "agent:run/r1", Server: sp("evidence"), Tool: sp("get_bank_txns"),
		Args: []byte(long), ResultCount: ip(3), LatencyMS: ip(42)})
	fa.add(store.AuditLogEntry{Actor: "asha", Tool: sp("finding.dismiss"), Args: []byte(`{ "reason": "<script>alert(1)</script>" }`),
		Error: sp("boom <b>")})
	ts := auditServer(t, fa)
	w := ts.get(t, RoleChecker, "/audit")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "2026-10-01T09:00:01Z") {
		t.Error("time is not RFC3339 UTC")
	}
	for _, want := range []string{"agent:run/r1", "get_bank_txns", ">evidence<", `class="count">3<`, `class="latency">42<`, "finding.dismiss", "boom &lt;b&gt;", "<details>"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>") {
		t.Error("untrusted text is not escaped")
	}
	if strings.Contains(body, "title=") {
		t.Error("full arguments must not sit in a title attribute")
	}
	// Truncated by runes: 120 characters then an ellipsis; no broken rune.
	if !strings.Contains(body, "…") || strings.Contains(body, "�") {
		t.Error("truncation lost its ellipsis or cut a rune")
	}
	if strings.Contains(body, strings.Repeat("é", 120)+"é") && strings.Count(body, strings.Repeat("é", 200)) != 1 {
		t.Error("full arguments appear more than once")
	}
	// Newest first.
	if strings.Index(body, "finding.dismiss") > strings.Index(body, "get_bank_txns") {
		t.Error("rows not newest first")
	}
	// A NULL count and latency are blank.
	if !strings.Contains(body, `<td class="count"></td>`) || !strings.Contains(body, `<td class="latency"></td>`) {
		t.Error("NULL cells not blank")
	}
	// Facets fill the dropdowns.
	for _, want := range []string{`<option value="asha">`, `<option value="evidence">`, `<option value="finding.dismiss">`} {
		if !strings.Contains(body, want) {
			t.Errorf("facet %q missing", want)
		}
	}
}

func TestAuditTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"abc", 5, "abc", false}, {"abcde", 5, "abcde", false}, {"abcdef", 5, "abcde…", true},
		{"ééééé", 3, "ééé…", true}, {"", 3, "", false},
	}
	for _, tt := range tests {
		got, cut := truncateRunes(tt.in, tt.n)
		if got != tt.want || cut != tt.cut {
			t.Errorf("truncateRunes(%q, %d) = %q, %v", tt.in, tt.n, got, cut)
		}
	}
}

func TestAuditEmptyAndBadParams(t *testing.T) {
	ts := auditServer(t, newFakeAudit())
	if w := ts.get(t, RoleChecker, "/audit"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No audit entries match.") {
		t.Errorf("empty = %d %s", w.Code, w.Body)
	}
	for _, q := range []string{"from=yesterday", "to=2026-13-01", "from=2026-1-1", "before=abc", "before=-4", "before=1.5"} {
		w := ts.get(t, RoleChecker, "/audit?"+q)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) == "" {
			t.Errorf("?%s = %d %q, want 400 with a message", q, w.Code, w.Body)
		}
	}
	fa := newFakeAudit()
	fa.failAll = fmt.Errorf("db down")
	if w := auditServer(t, fa).get(t, RoleChecker, "/audit"); w.Code != http.StatusInternalServerError {
		t.Errorf("store error = %d", w.Code)
	}
}

func TestAuditPagination(t *testing.T) {
	fa := newFakeAudit()
	for i := range 120 {
		actor := "asha"
		if i%2 == 1 {
			actor = "ravi"
		}
		fa.add(store.AuditLogEntry{Actor: actor, Tool: sp(fmt.Sprintf("tool-%03d", i))})
	}
	ts := auditServer(t, fa)
	older := func(body string) string {
		_, rest, ok := strings.Cut(body, `class="older" href="`)
		if !ok {
			return ""
		}
		href, _, _ := strings.Cut(rest, `"`)
		return strings.ReplaceAll(href, "&amp;", "&")
	}
	var seen []string
	path := "/audit?actor=asha"
	for range 5 {
		w := ts.get(t, RoleChecker, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, w.Code)
		}
		body := w.Body.String()
		for i := range 120 {
			if strings.Contains(body, fmt.Sprintf(`class="tool">tool-%03d<`, i)) {
				seen = append(seen, fmt.Sprintf("%03d", i))
			}
		}
		if strings.Contains(body, "ravi</td>") {
			t.Fatal("actor filter leaked another actor's rows")
		}
		path = older(body)
		if path == "" {
			break
		}
		u, _ := url.Parse(path)
		if u.Query().Get("actor") != "asha" || u.Query().Get("before") == "" {
			t.Fatalf("older link %q drops the filter or lacks before", path)
		}
	}
	if len(seen) != 60 {
		t.Fatalf("paged %d rows, want 60", len(seen))
	}
	for i, v := range seen { // each page ascending, pages newest first
		if v == "" || (v[2]-'0')%2 != 0 || (i > 0 && i%50 != 0 && v <= seen[i-1]) {
			t.Errorf("row %d = %s", i, v)
		}
	}
	if seen[0] != "020" || seen[49] != "118" {
		t.Errorf("first page spans %s..%s, want the newest 50", seen[0], seen[49])
	}
	if path != "" {
		t.Errorf("last page still links older: %s", path)
	}
}

func TestAuditRoleMatrix(t *testing.T) {
	routes := []struct {
		path  string
		roles []Role
	}{
		{"/audit", []Role{RoleChecker}},
		{"/runs/" + runID(1).String() + "/metrics", []Role{RoleViewer, RoleMaker, RoleChecker}},
	}
	for _, configured := range []bool{false, true} {
		fs := newFakeRunsStore()
		fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-09", Status: store.RunDone})
		ts := newTestServer(t)
		if configured {
			fa := newFakeAudit()
			ts = newTestServer(t, func(o *Options) {
				o.Audit = fa
				o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
			})
		}
		for _, rt := range routes {
			for _, c := range callers {
				want := http.StatusOK
				switch {
				case c.role == "" || c.bearer:
					want = http.StatusSeeOther // pages need a cookie session
				case !slices.Contains(rt.roles, c.role):
					want = http.StatusForbidden
				case !configured:
					want = http.StatusNotImplemented
				}
				t.Run(fmt.Sprintf("configured=%v %s as %s", configured, rt.path, c.name), func(t *testing.T) {
					if got := ts.call(t, c, http.MethodGet, rt.path); got != want {
						t.Errorf("status = %d, want %d", got, want)
					}
				})
			}
		}
	}
}

// TestAuditDemoFlow drives accept, dismiss, propose and approve through the
// real handlers and an MCP-style row, then reads them back on /audit.
func TestAuditDemoFlow(t *testing.T) {
	ff := newFakeFindings()
	fa := newFakeAudit()
	ap := &auditedApprovals{fakeApprovals: &fakeApprovals{}, fa: fa}
	ts := findingsServer(t, ff, func(o *Options) { o.Approvals = ap; o.Audit = fa })
	f1, f2, f3 := ff.put(fnd(1, "variance", "open")), ff.put(fnd(2, "variance", "open")), ff.put(fnd(3, "bank_charge", "open"))

	day1 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fa.add(store.AuditLogEntry{At: day1, Actor: "agent:run/" + runID(1).String(), Server: sp("evidence"), Tool: sp("get_bank_txns"),
		Args: []byte(`{"company":"acme"}`), ResultCount: ip(7), LatencyMS: ip(12)})

	mustOK := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		if w.Code >= 400 {
			t.Fatalf("%s = %d", what, w.Code)
		}
	}
	mustOK(ts.do(t, formPost(ts, RoleMaker, "/findings/"+f1.ID.String()+"/accept", nil)), "accept")
	mustOK(ts.do(t, formPost(ts, RoleMaker, "/findings/"+f2.ID.String()+"/dismiss", url.Values{"reason": {"noise"}})), "dismiss")
	mustOK(ts.do(t, formPost(ts, RoleMaker, "/findings/"+f3.ID.String()+"/proposals", nil)), "propose")
	mustOK(ts.do(t, formPost(ts, RoleChecker, "/proposals/"+pid(9).String()+"/approve", nil)), "approve")
	// The handlers' audit rows live in the findings fake; the real store
	// has one table, so mirror them into the audit fake in order.
	ff.mu.Lock()
	for _, e := range ff.audits {
		fa.add(e)
	}
	ff.mu.Unlock()

	body := ts.get(t, RoleChecker, "/audit").Body.String()
	cell := func(tool string) string { return `class="tool">` + tool + "<" }
	for _, want := range []string{"finding.accept", "finding.dismiss", "proposal.create", "proposal.approve", "get_bank_txns"} {
		if n := strings.Count(body, cell(want)); n != 1 {
			t.Errorf("%s appears %d times, want once", want, n)
		}
	}
	rowFor := func(tool string) string {
		before, _, _ := strings.Cut(body, cell(tool))
		i := strings.LastIndex(before, `<tr class="audit-row">`)
		if i < 0 {
			return ""
		}
		return before[i:]
	}
	for tool, actor := range map[string]string{
		"finding.accept": "asha", "finding.dismiss": "asha", "proposal.create": "asha", "proposal.approve": "ravi",
		"get_bank_txns": "agent:run/" + runID(1).String(),
	} {
		if !strings.Contains(rowFor(tool), `<td class="actor">`+actor+`</td>`) {
			t.Errorf("%s row lacks actor %s: %s", tool, actor, rowFor(tool))
		}
	}

	narrow := func(q string, want int) {
		t.Helper()
		b := ts.get(t, RoleChecker, "/audit?"+q).Body.String()
		if n := strings.Count(b, `<tr class="audit-row">`); n != want {
			t.Errorf("?%s shows %d rows, want %d", q, n, want)
		}
	}
	narrow("actor=ravi", 1)
	narrow("actor=asha", 3)
	narrow("server=approvals", 2)
	narrow("tool=finding.dismiss", 1)
	narrow("server=evidence&tool=get_bank_txns", 1)
	narrow("from=2026-09-30&to=2026-09-30", 1)
	narrow("from=2026-10-01", 4)
	narrow("to=2026-09-29", 0)
}

// auditedApprovals is fakeApprovals that also writes the audit rows the
// real approvals service writes.
type auditedApprovals struct {
	*fakeApprovals
	fa *fakeAudit
}

func (a *auditedApprovals) Propose(ctx context.Context, actor string, id uuid.UUID) (store.JournalProposal, error) {
	a.fa.add(store.AuditLogEntry{Actor: actor, Server: sp("approvals"), Tool: sp("proposal.create"), Args: []byte(`{"result":"ok"}`)})
	return a.fakeApprovals.Propose(ctx, actor, id)
}

func (a *auditedApprovals) Approve(ctx context.Context, actor string, id uuid.UUID) (ApproveResult, error) {
	a.fa.add(store.AuditLogEntry{Actor: actor, Server: sp("approvals"), Tool: sp("proposal.approve"), Args: []byte(`{"result":"ok"}`)})
	return a.fakeApprovals.Approve(ctx, actor, id)
}
