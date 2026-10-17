package web

import (
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	mu       sync.Mutex
	rows     []store.AuditLogEntry
	steps    map[uuid.UUID][]store.StepUsage
	stats    store.LastRunStats
	failAll  error
	trend    []store.RunTrendPoint
	trendErr error
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

func (f *fakeAudit) RecentRunTrend(context.Context, int) ([]store.RunTrendPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trend, f.trendErr
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
		i := strings.LastIndex(before, `<tr data-audit-row="1">`)
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
		if n := strings.Count(b, `<tr data-audit-row="1">`); n != want {
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

// pagingAudit records how the export pages the store.
type pagingAudit struct {
	*fakeAudit
	limits  []int
	befores []int64
	cancel  context.CancelFunc // called after the first page when set
}

func (p *pagingAudit) ListAuditPage(ctx context.Context, f store.AuditFilter) ([]store.AuditLogEntry, int64, error) {
	p.mu.Lock()
	p.limits = append(p.limits, f.Limit)
	p.befores = append(p.befores, f.Before)
	cancel := p.cancel
	p.mu.Unlock()
	rows, next, err := p.fakeAudit.ListAuditPage(ctx, f)
	if cancel != nil {
		cancel()
	}
	return rows, next, err
}

func chipLinks(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`class="audit-chip" href="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

func TestAuditPageChips(t *testing.T) {
	fa := newFakeAudit()
	fa.add(store.AuditLogEntry{Actor: "asha", Server: sp("evidence"), Tool: sp("get_bank_txns")})
	ts := auditServer(t, fa)

	plain := ts.get(t, RoleChecker, "/audit").Body.String()
	if strings.Contains(plain, "audit-chip") || strings.Contains(plain, "Clear all") {
		t.Error("chips or Clear all shown without filters")
	}
	if !strings.Contains(plain, `href="/audit/export.csv"`) {
		t.Error("export link missing")
	}

	w := ts.get(t, RoleChecker, "/audit?actor=asha&tool=get_bank_txns&from=2026-09-01&before=9")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	links := chipLinks(body)
	if len(links) != 3 {
		t.Fatalf("chip links = %v, want 3", links)
	}
	wantDropped := []string{"actor", "tool", "from"}
	for i, l := range links {
		u, err := url.Parse(l)
		if err != nil || u.Path != "/audit" {
			t.Fatalf("chip link %q", l)
		}
		q := u.Query()
		if q.Has("before") {
			t.Errorf("%q keeps before", l)
		}
		for _, k := range []string{"actor", "tool", "from"} {
			if want := k != wantDropped[i]; q.Has(k) != want {
				t.Errorf("%q: has %s = %v, want %v", l, k, q.Has(k), want)
			}
		}
	}
	if !strings.Contains(body, "Clear all") {
		t.Error("Clear all missing with filters")
	}
	if !strings.Contains(body, `href="/audit/export.csv?actor=asha&amp;from=2026-09-01&amp;tool=get_bank_txns"`) {
		t.Errorf("export link wrong: %s", regexp.MustCompile(`href="/audit/export[^"]*"`).FindString(body))
	}

	evil := `a<b>"&x`
	body = ts.get(t, RoleChecker, "/audit?actor="+url.QueryEscape(evil)).Body.String()
	if strings.Contains(body, evil) || strings.Contains(body, "<b>") {
		t.Error("filter value not escaped")
	}
	if l := chipLinks(body); len(l) != 1 || l[0] != "/audit" {
		t.Errorf("chip link for a lone filter = %v", l)
	}

	for _, r := range []Role{RoleViewer, RoleMaker} {
		if got := ts.get(t, r, "/audit").Code; got != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", r, got)
		}
	}
}

func TestAuditPageFilterErrors(t *testing.T) {
	ts := auditServer(t, newFakeAudit())
	for _, tt := range []struct{ path, msg string }{
		{"/audit?from=yesterday", "from must be a date as YYYY-MM-DD"},
		{"/audit?to=2026-13-01", "to must be a date as YYYY-MM-DD"},
		{"/audit?before=abc", "before must be a row number"},
		{"/audit?before=-1", "before must be a row number"},
		{"/audit/export.csv?from=nope", "from must be a date as YYYY-MM-DD"},
		{"/audit/export.csv?before=x", "before must be a row number"},
	} {
		w := ts.get(t, RoleChecker, tt.path)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != tt.msg {
			t.Errorf("%s = %d %q, want 400 %q", tt.path, w.Code, w.Body, tt.msg)
		}
	}
}

func readCSV(t *testing.T, body string) [][]string {
	t.Helper()
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v\n%s", err, body)
	}
	return recs
}

func TestAuditExportCSV(t *testing.T) {
	fa := newFakeAudit()
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	long := `{"q":"` + strings.Repeat("x", 300) + `"}`
	fa.add(store.AuditLogEntry{At: day(10, 9), Actor: "asha", Server: sp("evidence"), Tool: sp("get_bank_txns"), Args: []byte(long), ResultCount: ip(3), LatencyMS: ip(7)})
	fa.add(store.AuditLogEntry{At: day(11, 9), Actor: "ravi", Server: sp("approvals"), Tool: sp("proposal.approve"), Args: []byte(`{ "a": 1 }`), Error: sp("boom")})
	fa.add(store.AuditLogEntry{At: day(12, 9), Actor: "asha", Tool: sp("finding.dismiss")})
	ts := auditServer(t, fa)

	w := ts.get(t, RoleChecker, "/audit/export.csv")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !regexp.MustCompile(`^attachment; filename="audit-\d{8}\.csv"$`).MatchString(cd) {
		t.Errorf("disposition = %q", cd)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Export-Truncated") != "" {
		t.Errorf("headers = %v", w.Header())
	}
	recs := readCSV(t, w.Body.String())
	if strings.Join(recs[0], ",") != "time_utc,actor,server,tool,args,result_count,latency_ms,error" {
		t.Errorf("header = %v", recs[0])
	}
	if len(recs) != 4 {
		t.Fatalf("rows = %d, want 3 + header", len(recs)-1)
	}
	if recs[1][3] != "finding.dismiss" || recs[2][3] != "proposal.approve" || recs[3][3] != "get_bank_txns" {
		t.Errorf("order = %v %v %v", recs[1][3], recs[2][3], recs[3][3])
	}
	if recs[3][0] != "2026-09-10T09:00:00Z" || recs[3][4] != long || recs[3][5] != "3" || recs[3][6] != "7" {
		t.Errorf("row = %v", recs[3])
	}
	if recs[2][4] != `{"a":1}` || recs[2][7] != "boom" {
		t.Errorf("row = %v", recs[2])
	}

	for q, want := range map[string]int{
		"actor=asha": 2, "actor=ravi": 1, "server=evidence": 1, "tool=finding.dismiss": 1,
		"from=2026-09-11": 2, "to=2026-09-11": 2, "from=2026-09-11&to=2026-09-11": 1,
	} {
		got := readCSV(t, ts.get(t, RoleChecker, "/audit/export.csv?"+q).Body.String())
		if len(got)-1 != want {
			t.Errorf("?%s = %d rows, want %d", q, len(got)-1, want)
		}
	}
}

func TestAuditExportFormulaGuard(t *testing.T) {
	for in, want := range map[string]string{
		"=1+1": "'=1+1", "+1": "'+1", "-2": "'-2", "@SUM(A1)": "'@SUM(A1)", "\tx": "'\tx", "\rx": "'\rx",
		"plain": "plain", "": "", "a=b": "a=b",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
	fa := newFakeAudit()
	fa.add(store.AuditLogEntry{Actor: "=cmd", Server: sp("+x"), Tool: sp("-y"), Error: sp("@z"), Args: []byte(`"=1"`)})
	fa.add(store.AuditLogEntry{Actor: "a,b", Tool: sp("line\nbreak"), Error: sp(`say "hi"`)})
	ts := auditServer(t, fa)
	body := ts.get(t, RoleChecker, "/audit/export.csv").Body.String()
	recs := readCSV(t, body)
	if len(recs) != 3 {
		t.Fatalf("records = %d", len(recs))
	}
	if r := recs[2]; r[1] != "'=cmd" || r[2] != "'+x" || r[3] != "'-y" || r[7] != "'@z" || r[4] != `"=1"` {
		t.Errorf("guarded row = %q", r)
	}
	if r := recs[1]; r[1] != "a,b" || r[3] != "line\nbreak" || r[7] != `say "hi"` {
		t.Errorf("quoted row = %q", r)
	}
	if !strings.Contains(body, `"a,b"`) || !strings.Contains(body, `"say ""hi"""`) {
		t.Errorf("encoding/csv quoting missing:\n%s", body)
	}
}

func TestAuditExportCap(t *testing.T) {
	fa := newFakeAudit()
	for i := range auditExportCap + 300 {
		fa.add(store.AuditLogEntry{Actor: "asha", Tool: sp(fmt.Sprintf("t%d", i))})
	}
	pa := &pagingAudit{fakeAudit: fa}
	ts := newTestServer(t, func(o *Options) { o.Audit = pa })
	w := ts.get(t, RoleChecker, "/audit/export.csv")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := len(readCSV(t, w.Body.String())) - 1; got != auditExportCap {
		t.Errorf("data rows = %d, want %d", got, auditExportCap)
	}
	if w.Header().Get("X-Export-Truncated") != "true" {
		t.Error("truncated header missing")
	}
	for i, l := range pa.limits {
		if l > 200 || l <= 0 {
			t.Errorf("page %d limit = %d", i, l)
		}
		if i > 0 && pa.befores[i] >= pa.befores[i-1] && pa.befores[i-1] != 0 {
			t.Errorf("before did not advance: %v", pa.befores)
		}
	}
	if len(pa.befores) < 2 || pa.befores[0] != 0 || pa.befores[1] == 0 {
		t.Errorf("befores = %v", pa.befores)
	}

	// Exactly at the cap and below it: no truncation header.
	small := newFakeAudit()
	for i := range auditExportCap {
		small.add(store.AuditLogEntry{Actor: "a", Tool: sp(fmt.Sprintf("t%d", i))})
	}
	w = auditServer(t, small).get(t, RoleChecker, "/audit/export.csv")
	if got := len(readCSV(t, w.Body.String())) - 1; got != auditExportCap || w.Header().Get("X-Export-Truncated") != "" {
		t.Errorf("at cap: %d rows, truncated=%q", got, w.Header().Get("X-Export-Truncated"))
	}
}

func TestAuditExportRoles(t *testing.T) {
	fa := newFakeAudit()
	fa.add(store.AuditLogEntry{Actor: "secret-actor-zed", Args: []byte(`{"q":"secret-args-zed"}`)})
	ts := auditServer(t, fa)
	if got := ts.get(t, RoleChecker, "/audit/export.csv").Code; got != http.StatusOK {
		t.Errorf("checker = %d", got)
	}
	for _, r := range []Role{RoleMaker, RoleViewer} {
		if got := ts.get(t, r, "/audit/export.csv").Code; got != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", r, got)
		}
	}
	if got := ts.do(t, req{method: http.MethodGet, path: "/audit/export.csv"}).Code; got != http.StatusSeeOther {
		t.Errorf("anonymous = %d, want 303", got)
	}
	if got := ts.get(t, RoleChecker, "/audit/export.csv?to=bad").Code; got != http.StatusBadRequest {
		t.Errorf("bad filter = %d", got)
	}
	none := newTestServer(t)
	if got := none.get(t, RoleChecker, "/audit/export.csv").Code; got != http.StatusNotImplemented {
		t.Errorf("no audit backend = %d, want 501", got)
	}
	if got := none.get(t, RoleViewer, "/audit/export.csv").Code; got != http.StatusForbidden {
		t.Errorf("no backend, viewer = %d, want 403 before 501", got)
	}
	logs := ts.logs.String()
	if strings.Contains(logs, "secret-actor-zed") || strings.Contains(logs, "secret-args-zed") {
		t.Error("log contains audit data")
	}
	if !strings.Contains(logs, "audit export") {
		t.Error("export row count not logged")
	}
}

func TestAuditExportCancel(t *testing.T) {
	fa := newFakeAudit()
	for i := range 450 {
		fa.add(store.AuditLogEntry{Actor: "a", Tool: sp(fmt.Sprintf("t%d", i))})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pa := &pagingAudit{fakeAudit: fa, cancel: cancel}
	ts := newTestServer(t, func(o *Options) { o.Audit = pa })
	rq := ts.as(RoleChecker, http.MethodGet, "/audit/export.csv", "", "")
	r := httptest.NewRequest(http.MethodGet, rq.path, nil).WithContext(ctx)
	for _, c := range rq.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	ts.ServeHTTP(w, r)
	if n := len(pa.limits); n != 1 {
		t.Errorf("store paged %d times after cancel, want 1", n)
	}
	if strings.Contains(w.Body.String(), "time_utc") {
		t.Error("cancelled export still wrote a body")
	}
}
