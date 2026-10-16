package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// fakeRunsStore is an in-memory RunsStore.
type fakeRunsStore struct {
	mu        sync.Mutex
	companies []store.Company
	runs      map[uuid.UUID]store.CloseRun
	list      []store.RunSummary // returned by ListCloseRuns as given
	findings  map[uuid.UUID][]store.Finding
	created   []store.CloseRun
}

func newFakeRunsStore() *fakeRunsStore {
	return &fakeRunsStore{
		companies: []store.Company{{ID: "acme"}, {ID: "globex"}},
		runs:      map[uuid.UUID]store.CloseRun{},
		findings:  map[uuid.UUID][]store.Finding{},
	}
}

func (f *fakeRunsStore) CreateCloseRun(_ context.Context, r store.CloseRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = r
	f.created = append(f.created, r)
	return nil
}

func (f *fakeRunsStore) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return store.CloseRun{}, fmt.Errorf("%w: run %s", store.ErrNotFound, id)
	}
	return r, nil
}

func (f *fakeRunsStore) ListCloseRuns(context.Context, int) ([]store.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), nil
}

func (f *fakeRunsStore) ListFindingsByRun(_ context.Context, id uuid.UUID) ([]store.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.findings[id]), nil
}

func (f *fakeRunsStore) ListCompanies(context.Context) ([]store.Company, error) {
	return f.companies, nil
}

func (f *fakeRunsStore) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

// put adds a run (and its findings) the pages can read.
func (f *fakeRunsStore) put(r store.CloseRun, fs ...store.Finding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = r
	f.findings[r.ID] = fs
}

func runID(n byte) uuid.UUID { return uuid.UUID{0: 0xaa, 15: n} }
func fid(n byte) uuid.UUID   { return uuid.UUID{0: 0xbb, 15: n} }

func paise(n int64) *money.Paise { p := money.Paise(n); return &p }

func finding(run uuid.UUID, n byte, typ, sev string, amt *money.Paise, status string) store.Finding {
	return store.Finding{ID: fid(n), RunID: run, Type: typ, Severity: sev, Title: "title " + typ + fmt.Sprint(n),
		AmountPaise: amt, Status: status, Keys: map[string]string{}}
}

func runsServer(t testing.TB, fs *fakeRunsStore, exec RunExecutor) *testServer {
	t.Helper()
	if exec == nil {
		exec = func(context.Context, uuid.UUID) error { return nil }
	}
	return newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: fs, Execute: exec} })
}

// as builds a request from a cookie session of role r (with its CSRF token).
func (ts *testServer) as(r Role, method, path, body, ct string) req {
	ck, csrf := ts.session(r)
	return req{method: method, path: path, body: body, contentType: ct,
		cookies: []*http.Cookie{ck}, header: map[string]string{csrfHeaderName: csrf}}
}

func (ts *testServer) get(t testing.TB, r Role, path string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.do(t, ts.as(r, http.MethodGet, path, "", ""))
}

func (ts *testServer) startJSON(t testing.TB, company, month string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"company":%q,"month":%q}`, company, month)
	return ts.do(t, ts.as(RoleMaker, http.MethodPost, "/api/close-runs", body, "application/json"))
}

// waitIdle waits for every background run to end.
func waitIdle(t testing.TB, ts *testServer) {
	t.Helper()
	done := make(chan struct{})
	go func() { ts.runs.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background runs did not finish")
	}
}

func decodeRunID(t testing.TB, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	id, err := uuid.Parse(out.RunID)
	if err != nil {
		t.Fatalf("run_id %q: %v", out.RunID, err)
	}
	return id
}

func TestRunsStart(t *testing.T) {
	fs := newFakeRunsStore()
	type call struct {
		id     uuid.UUID
		status string
	}
	calls := make(chan call, 2)
	ts := runsServer(t, fs, func(ctx context.Context, id uuid.UUID) error {
		r, err := fs.GetCloseRun(ctx, id)
		if err != nil {
			t.Errorf("row missing when Execute starts: %v", err)
		}
		calls <- call{id, r.Status}
		return nil
	})

	w := ts.startJSON(t, "acme", "2026-09")
	if w.Code != http.StatusCreated {
		t.Fatalf("JSON start = %d: %s", w.Code, w.Body)
	}
	id := decodeRunID(t, w)
	if c := <-calls; c.id != id || c.status != store.RunQueued {
		t.Errorf("Execute got %+v, want id %s queued", c, id)
	}

	ck, csrf := ts.session(RoleMaker)
	form := url.Values{"company": {"globex"}, "month": {"2026-08"}, "csrf_token": {csrf}} // the form field carries the token
	w = ts.do(t, req{method: http.MethodPost, path: "/runs", body: form.Encode(),
		contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("form start = %d: %s", w.Code, w.Body)
	}
	c := <-calls
	if loc := w.Header().Get("Location"); loc != "/runs/"+c.id.String() || c.status != store.RunQueued {
		t.Errorf("Location %q, Execute %+v", loc, c)
	}
	waitIdle(t, ts)
	if got := fs.createdCount(); got != 2 {
		t.Errorf("created %d rows, want 2", got)
	}
}

func TestRunsOnePerCompany(t *testing.T) {
	fs := newFakeRunsStore()
	gates := map[string]chan struct{}{"acme": make(chan struct{})}
	started := make(chan string, 10)
	ts := runsServer(t, fs, func(ctx context.Context, id uuid.UUID) error {
		r, _ := fs.GetCloseRun(ctx, id)
		started <- r.CompanyID
		if g := gates[r.CompanyID]; g != nil {
			<-g
		}
		if r.CompanyID == "globex" && r.Month == "2026-01" {
			panic("boom")
		}
		return nil
	})

	if w := ts.startJSON(t, "acme", "2026-09"); w.Code != http.StatusCreated {
		t.Fatalf("first start = %d", w.Code)
	}
	<-started
	before := fs.createdCount()
	if w := ts.startJSON(t, "acme", "2026-08"); w.Code != http.StatusConflict {
		t.Errorf("second start for a busy company = %d, want 409", w.Code)
	}
	form := url.Values{"company": {"acme"}, "month": {"2026-08"}}
	if w := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/runs", form.Encode(), "application/x-www-form-urlencoded")); w.Code != http.StatusConflict {
		t.Errorf("form start for a busy company = %d, want 409", w.Code)
	}
	if got := fs.createdCount(); got != before {
		t.Errorf("a refused start created a row (%d -> %d)", before, got)
	}
	if w := ts.startJSON(t, "globex", "2026-09"); w.Code != http.StatusCreated {
		t.Errorf("another company = %d, want 201", w.Code)
	}

	close(gates["acme"])
	waitIdle(t, ts)
	if w := ts.startJSON(t, "acme", "2026-08"); w.Code != http.StatusCreated {
		t.Errorf("after the first returned = %d, want 201", w.Code)
	}
	waitIdle(t, ts)

	// A panicking executor releases the lock and is logged.
	if w := ts.startJSON(t, "globex", "2026-01"); w.Code != http.StatusCreated {
		t.Fatalf("start = %d", w.Code)
	}
	waitIdle(t, ts)
	if w := ts.startJSON(t, "globex", "2026-02"); w.Code != http.StatusCreated {
		t.Errorf("after a panic = %d, want 201", w.Code)
	}
	waitIdle(t, ts)
	if !strings.Contains(ts.logs.String(), "panicked") {
		t.Error("panic not logged")
	}
}

func TestRunsStartValidation(t *testing.T) {
	fs := newFakeRunsStore()
	var executed atomic.Int32
	ts := runsServer(t, fs, func(context.Context, uuid.UUID) error { executed.Add(1); return nil })

	tests := []struct{ name, body string }{
		{"unknown company", `{"company":"nope","month":"2026-09"}`},
		{"month 13", `{"company":"acme","month":"2026-13"}`},
		{"month one digit", `{"company":"acme","month":"2026-9"}`},
		{"month empty", `{"company":"acme","month":""}`},
		{"month with day", `{"company":"acme","month":"2026-09-01"}`},
		{"empty company", `{"company":"","month":"2026-09"}`},
		{"unknown field", `{"company":"acme","month":"2026-09","extra":1}`},
		{"malformed", `{"company":`},
		{"not an object", `[1]`},
		{"two objects", `{"company":"acme","month":"2026-09"}{}`},
		{"empty body", ``},
	}
	for _, tt := range tests {
		t.Run("json "+tt.name, func(t *testing.T) {
			w := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/api/close-runs", tt.body, "application/json"))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
			}
			var e map[string]string
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e["error"] == "" {
				t.Errorf("body %q is not an error object", w.Body)
			}
		})
	}
	for _, f := range []url.Values{
		{"company": {"nope"}, "month": {"2026-09"}},
		{"company": {"acme"}, "month": {"2026-13"}},
		{"company": {"acme"}, "month": {""}},
		{},
	} {
		w := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/runs", f.Encode(), "application/x-www-form-urlencoded"))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `role="alert"`) {
			t.Errorf("form %v = %d, want 400 with a message", f, w.Code)
		}
	}
	waitIdle(t, ts)
	if n := fs.createdCount(); n != 0 || executed.Load() != 0 {
		t.Errorf("created %d rows, executed %d, want none", n, executed.Load())
	}
}

func TestRunsRoles(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone})
	ts := runsServer(t, fs, nil)

	for _, role := range []Role{RoleViewer, RoleChecker} {
		w := ts.do(t, ts.as(role, http.MethodPost, "/api/close-runs", `{"company":"acme","month":"2026-09"}`, "application/json"))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s POST /api/close-runs = %d, want 403", role, w.Code)
		}
		w = ts.do(t, ts.as(role, http.MethodPost, "/runs", "company=acme&month=2026-09", "application/x-www-form-urlencoded"))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s POST /runs = %d, want 403", role, w.Code)
		}
	}
	paths := []string{"/runs", "/runs/" + rid.String(), "/runs/" + rid.String() + "/progress", "/runs/" + rid.String() + "/findings",
		"/api/close-runs/" + rid.String(), "/api/close-runs/" + rid.String() + "/findings"}
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		for _, p := range paths {
			if w := ts.get(t, role, p); w.Code != http.StatusOK {
				t.Errorf("%s GET %s = %d, want 200", role, p, w.Code)
			}
		}
		form := strings.Contains(ts.get(t, role, "/runs").Body.String(), `class="start-run"`)
		if form != (role == RoleMaker) {
			t.Errorf("%s sees the start form = %v", role, form)
		}
	}
	if n := fs.createdCount(); n != 0 {
		t.Errorf("forbidden callers created %d runs", n)
	}

	w := ts.do(t, req{method: http.MethodGet, path: "/runs"})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous GET /runs = %d %q", w.Code, w.Header().Get("Location"))
	}
	w = ts.do(t, req{method: http.MethodPost, path: "/runs", body: "company=acme", contentType: "application/x-www-form-urlencoded"})
	if w.Code != http.StatusSeeOther {
		t.Errorf("anonymous POST /runs = %d", w.Code)
	}
	for _, rq := range []req{
		{method: http.MethodGet, path: "/api/close-runs/" + rid.String()},
		{method: http.MethodPost, path: "/api/close-runs", body: `{}`},
	} {
		if w := ts.do(t, rq); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", rq.method, rq.path, w.Code)
		}
	}
}

func TestRunsNotConfigured(t *testing.T) {
	ts := newTestServer(t)
	id := runID(1).String()
	rows := []struct {
		method, path string
		api          bool
		makerOnly    bool
	}{
		{http.MethodGet, "/runs", false, false},
		{http.MethodPost, "/runs", false, true},
		{http.MethodGet, "/runs/" + id, false, false},
		{http.MethodGet, "/runs/" + id + "/progress", false, false},
		{http.MethodGet, "/runs/" + id + "/findings", false, false},
		{http.MethodPost, "/api/close-runs", true, true},
		{http.MethodGet, "/api/close-runs/" + id, true, false},
		{http.MethodGet, "/api/close-runs/" + id + "/findings", true, false},
	}
	for _, row := range rows {
		for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
			want := http.StatusNotImplemented
			if row.makerOnly && role != RoleMaker {
				want = http.StatusForbidden
			}
			w := ts.do(t, ts.as(role, row.method, row.path, "", ""))
			if w.Code != want {
				t.Errorf("%s %s as %s = %d, want %d", row.method, row.path, role, w.Code, want)
			}
			if row.api && want == http.StatusNotImplemented && !strings.Contains(w.Body.String(), `"error":"not implemented"`) {
				t.Errorf("%s %s body = %q", row.method, row.path, w.Body)
			}
		}
	}
}

func TestRunsPageList(t *testing.T) {
	fs := newFakeRunsStore()
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(2*time.Minute + 30*time.Second)
	fs.list = []store.RunSummary{
		{CloseRun: store.CloseRun{ID: runID(2), CompanyID: "globex", Month: "2026-09", Status: store.RunRunning, StartedAt: &t0, CostUSD: 0.5}, Findings: 0},
		{CloseRun: store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-08", Status: store.RunDone, StartedAt: &t0, FinishedAt: &t1, CostUSD: 1.23456}, Findings: 7},
	}
	ts := runsServer(t, fs, nil)
	body := ts.get(t, RoleViewer, "/runs").Body.String()

	first, second := strings.Index(body, runID(2).String()), strings.Index(body, runID(1).String())
	if first < 0 || second < 0 || first > second {
		t.Errorf("rows not in the store's order (positions %d, %d)", first, second)
	}
	for _, want := range []string{
		`href="/runs/` + runID(1).String() + `"`, `href="/runs/` + runID(2).String() + `"`,
		`<td class="findings">7</td>`, `<td class="cost">$1.2346</td>`, `<td class="duration">2m30s</td>`,
		`<td class="duration"></td>`, `acme`, `2026-08`, store.RunDone, store.RunRunning,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runs page lacks %q", want)
		}
	}

	fs.mu.Lock()
	fs.list = nil
	fs.mu.Unlock()
	if !strings.Contains(ts.get(t, RoleViewer, "/runs").Body.String(), "No runs yet") {
		t.Error("empty list not explained")
	}
}

func TestRunProgress(t *testing.T) {
	fs := newFakeRunsStore()
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(90 * time.Second)
	msg := "<b>boom</b>"
	mk := func(n byte, status string, fin *time.Time, errText *string) uuid.UUID {
		id := runID(n)
		fs.put(store.CloseRun{ID: id, CompanyID: "acme", Month: "2026-09", Status: status, StartedAt: &t0, FinishedAt: fin,
			Error: errText, InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 56, CostUSD: 0.0123},
			finding(id, 1, "variance", "low", nil, "open"), finding(id, 2, "variance", "low", nil, "open"),
			finding(id, 3, "unrecorded_bank_charge", "high", paise(1000), "open"))
		return id
	}
	running := mk(1, store.RunRunning, nil, nil)
	queued := mk(2, store.RunQueued, nil, nil)
	done := mk(3, store.RunDone, &t1, nil)
	partial := mk(4, store.RunPartial, &t1, nil)
	failed := mk(5, store.RunFailed, &t1, &msg)
	ts := runsServer(t, fs, nil)
	ts.clock.Set(t0.Add(10 * time.Second))

	for _, id := range []uuid.UUID{running, queued} {
		w := ts.get(t, RoleViewer, "/runs/"+id.String()+"/progress")
		body := w.Body.String()
		if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "" {
			t.Errorf("active run: %d, HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
		}
		for _, want := range []string{`hx-trigger="every 2s"`, `hx-get="/runs/` + id.String() + `/progress"`, `hx-swap="outerHTML"`, "10s"} {
			if !strings.Contains(body, want) {
				t.Errorf("active partial lacks %q", want)
			}
		}
	}
	w := ts.get(t, RoleViewer, "/runs/"+running.String()+"/progress")
	for _, want := range []string{`data-step="running" class="step current"`, `data-step="queued" class="step"`,
		`data-type="variance">variance: 2<`, `data-type="unrecorded_bank_charge">unrecorded_bank_charge: 1<`,
		`class="input">1200<`, `class="output">340<`, `class="cache">56<`, `$0.0123`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("partial lacks %q\n%s", want, w.Body)
		}
	}

	for _, id := range []uuid.UUID{done, partial, failed} {
		w := ts.get(t, RoleViewer, "/runs/"+id.String()+"/progress")
		if w.Header().Get("HX-Redirect") != "/runs/"+id.String()+"/findings" {
			t.Errorf("terminal HX-Redirect = %q", w.Header().Get("HX-Redirect"))
		}
		if strings.Contains(w.Body.String(), "hx-trigger") || !strings.Contains(w.Body.String(), "1m30s") {
			t.Errorf("terminal partial polls or lacks the duration: %s", w.Body)
		}
		page := ts.get(t, RoleViewer, "/runs/"+id.String())
		if page.Header().Get("HX-Redirect") != "" || strings.Contains(page.Body.String(), "hx-trigger") {
			t.Errorf("terminal run page redirects or polls")
		}
	}
	if body := ts.get(t, RoleViewer, "/runs/"+failed.String()+"/progress").Body.String(); strings.Contains(body, "<b>boom") || !strings.Contains(body, "&lt;b&gt;boom&lt;/b&gt;") {
		t.Errorf("error text not escaped: %s", body)
	}
	page := ts.get(t, RoleViewer, "/runs/"+running.String()).Body.String()
	if !strings.Contains(page, `id="run-progress"`) || !strings.Contains(page, `hx-trigger="every 2s"`) {
		t.Error("running run page lacks the polling panel")
	}
}

func TestRunFindingsTable(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	a := finding(rid, 1, "unrecorded_bank_charge", "high", paise(12345678), "open")
	a.Verified = true
	b := finding(rid, 2, "missing_accrual", "medium", nil, "needs_review")
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone}, a, b)
	ts := runsServer(t, fs, nil)
	body := ts.get(t, RoleChecker, "/runs/"+rid.String()+"/findings").Body.String()

	for _, want := range []string{"<th>Severity</th>", "<th>Type</th>", "<th>Title</th>", "<th>Amount</th>", "<th>Status</th>", "<th>Verified</th>",
		`<td class="amount">₹1,23,456.78</td>`, `<td class="amount"></td>`, `badge verified">verified<`, `badge unverified">unverified<`,
		`<td class="status">needs_review</td>`, `<td class="title">title unrecorded_bank_charge1</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("findings page lacks %q", want)
		}
	}
}

func TestRunFindingsFilters(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone},
		finding(rid, 1, "missing_accrual", "high", nil, "open"),
		finding(rid, 2, "missing_accrual", "low", nil, "dismissed"),
		finding(rid, 3, "wrong_period_posting", "low", nil, "open"))
	ts := runsServer(t, fs, nil)
	base := "/runs/" + rid.String() + "/findings"
	rows := func(q string) int {
		return strings.Count(ts.get(t, RoleViewer, base+q).Body.String(), `data-finding="`)
	}
	tests := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?type=missing_accrual", 2},
		{"?type=wrong_period_posting", 1},
		{"?status=open", 2},
		{"?type=missing_accrual&status=open", 1},
		{"?type=nope", 3},
		{"?status=accepted", 3}, // valid status, but none in this run: ignored
		{"?type=nope&status=dismissed", 1},
	}
	for _, tt := range tests {
		if got := rows(tt.query); got != tt.want {
			t.Errorf("%q: %d rows, want %d", tt.query, got, tt.want)
		}
	}
	body := ts.get(t, RoleViewer, base+"?type=missing_accrual").Body.String()
	for _, want := range []string{`<option value="missing_accrual" selected>`, `<option value="wrong_period_posting">`,
		`<option value="open">`, `<option value="dismissed">`, `method="get"`} {
		if !strings.Contains(body, want) {
			t.Errorf("filter form lacks %q", want)
		}
	}
	if strings.Contains(body, `<option value="accepted"`) || strings.Contains(body, `<option value="variance"`) {
		t.Error("filter options include values absent from the run")
	}
}

func TestRunFindingsOrder(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	// Expected order by id number.
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone},
		finding(rid, 9, "t", "mystery", paise(999999), "open"),
		finding(rid, 8, "t", "low", paise(5), "open"),
		finding(rid, 7, "t", "medium", nil, "open"),
		finding(rid, 6, "t", "medium", paise(-50), "open"),
		finding(rid, 5, "t", "high", nil, "open"),
		finding(rid, 4, "t", "high", paise(200), "open"),
		finding(rid, 3, "t", "high", paise(200), "open"),
		finding(rid, 2, "t", "high", paise(900), "open"),
		finding(rid, 1, "t", "medium", paise(10), "open"),
	)
	ts := runsServer(t, fs, nil)
	want := []byte{2, 3, 4, 5, 1, 6, 7, 8, 9}

	body := ts.get(t, RoleViewer, "/runs/"+rid.String()+"/findings").Body.String()
	pos := -1
	for _, n := range want {
		i := strings.Index(body, fid(n).String())
		if i < pos {
			t.Fatalf("finding %d out of order in HTML", n)
		}
		pos = i
	}
	var out struct {
		Findings []store.Finding `json:"findings"`
	}
	w := ts.get(t, RoleViewer, "/api/close-runs/"+rid.String()+"/findings")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, f := range out.Findings {
		got = append(got, f.ID[15])
	}
	if !slices.Equal(got, want) {
		t.Errorf("JSON order = %v, want %v", got, want)
	}
}

func TestRunFindingsVarianceNesting(t *testing.T) {
	fs := newFakeRunsStore()
	rid, other := runID(1), runID(2)
	parent := finding(rid, 1, "missing_accrual", "high", paise(500), "open")
	child := finding(rid, 2, "variance", "low", nil, "open")
	child.Keys = map[string]string{"related": fid(1).String()}
	orphanMissing := finding(rid, 3, "variance", "low", nil, "open")
	orphanMissing.Keys = map[string]string{"related": fid(99).String()}
	orphanForeign := finding(rid, 4, "variance", "low", nil, "open")
	orphanForeign.Keys = map[string]string{"related": fid(50).String()}
	foreign := finding(other, 50, "missing_accrual", "high", nil, "open")
	orphanBad := finding(rid, 5, "variance", "low", nil, "dismissed")
	orphanBad.Keys = map[string]string{"related": "not-a-uuid"}
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone},
		parent, child, orphanMissing, orphanForeign, orphanBad)
	fs.put(store.CloseRun{ID: other, CompanyID: "acme", Month: "2026-08", Status: store.RunDone}, foreign)
	ts := runsServer(t, fs, nil)
	base := "/runs/" + rid.String() + "/findings"

	body := ts.get(t, RoleViewer, base).Body.String()
	rowsIn := func(b string) []string {
		var out []string
		for _, line := range strings.Split(b, "<tr ") {
			if i := strings.Index(line, `data-finding="`); i >= 0 {
				id := line[i+len(`data-finding="`) : i+len(`data-finding="`)+36]
				kind := "top"
				if strings.Contains(line[:strings.Index(line, ">")], "nested") {
					kind = "nested"
				}
				out = append(out, kind+":"+id)
			}
		}
		return out
	}
	got := rowsIn(body)
	if len(got) != 5 {
		t.Fatalf("rows = %v", got)
	}
	if got[0] != "top:"+fid(1).String() || got[1] != "nested:"+fid(2).String() {
		t.Errorf("child not nested under parent: %v", got[:2])
	}
	for _, r := range got[2:] {
		if !strings.HasPrefix(r, "top:") {
			t.Errorf("orphan %s not top level", r)
		}
	}

	// A filtered-out parent hides its nested variance; orphans are filtered
	// like any other row.
	got = rowsIn(ts.get(t, RoleViewer, base+"?type=variance").Body.String())
	if len(got) != 3 || slices.Contains(got, "nested:"+fid(2).String()) || slices.Contains(got, "top:"+fid(2).String()) {
		t.Errorf("type=variance rows = %v", got)
	}
	got = rowsIn(ts.get(t, RoleViewer, base+"?status=dismissed").Body.String())
	if len(got) != 1 || got[0] != "top:"+fid(5).String() {
		t.Errorf("status=dismissed rows = %v", got)
	}

	var out struct {
		Findings []store.Finding `json:"findings"`
	}
	w := ts.get(t, RoleViewer, "/api/close-runs/"+rid.String()+"/findings")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Findings) != 5 {
		t.Errorf("JSON list has %d findings (%v), want a flat 5", len(out.Findings), err)
	}
}

func TestRunsAPIJSON(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunRunning, StartedAt: &t0,
		InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CostUSD: 0.25},
		finding(rid, 1, "missing_accrual", "high", paise(100), "open"),
		finding(rid, 2, "missing_accrual", "low", nil, "open"))
	ts := runsServer(t, fs, nil)

	w := ts.get(t, RoleViewer, "/api/close-runs/"+rid.String())
	if w.Code != http.StatusOK {
		t.Fatalf("GET run = %d", w.Code)
	}
	var run map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "company", "month", "status", "started_at", "finished_at", "error", "finding_counts",
		"findings_total", "input_tokens", "output_tokens", "cache_read_tokens", "cost_usd"} {
		if _, ok := run[k]; !ok {
			t.Errorf("run JSON lacks %q", k)
		}
	}
	if run["company"] != "acme" || run["status"] != store.RunRunning || run["findings_total"] != float64(2) ||
		run["finished_at"] != nil || run["cost_usd"] != 0.25 {
		t.Errorf("run JSON = %v", run)
	}
	if c, _ := run["finding_counts"].(map[string]any); c["missing_accrual"] != float64(2) {
		t.Errorf("finding_counts = %v", run["finding_counts"])
	}

	w = ts.get(t, RoleViewer, "/api/close-runs/"+rid.String()+"/findings")
	var fl struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Findings) != 2 {
		t.Fatalf("findings JSON: %v %s", err, w.Body)
	}
	for _, k := range []string{"id", "run_id", "type", "severity", "title", "keys", "evidence", "verified", "status"} {
		if _, ok := fl.Findings[0][k]; !ok {
			t.Errorf("finding JSON lacks %q", k)
		}
	}
	if w := ts.get(t, RoleViewer, "/api/close-runs/"+rid.String()+"/findings?type=none"); !strings.Contains(w.Body.String(), "findings") || w.Code != 200 {
		t.Errorf("unknown filter: %d %s", w.Code, w.Body)
	}

	// 404: bad and unknown ids look the same.
	bad := ts.get(t, RoleViewer, "/api/close-runs/not-a-uuid")
	unknown := ts.get(t, RoleViewer, "/api/close-runs/"+runID(77).String())
	badF := ts.get(t, RoleViewer, "/api/close-runs/not-a-uuid/findings")
	if bad.Code != 404 || unknown.Code != 404 || badF.Code != 404 || bad.Body.String() != unknown.Body.String() {
		t.Errorf("404s: %d %q / %d %q", bad.Code, bad.Body, unknown.Code, unknown.Body)
	}
	hb, hu := ts.get(t, RoleViewer, "/runs/not-a-uuid"), ts.get(t, RoleViewer, "/runs/"+runID(77).String()+"/findings")
	if hb.Code != 404 || hu.Code != 404 || hb.Body.String() != hu.Body.String() {
		t.Errorf("HTML 404s: %d %q / %d %q", hb.Code, hb.Body, hu.Code, hu.Body)
	}

	// Start: bearer maker needs no CSRF token; a cookie maker does.
	rq := req{method: http.MethodPost, path: "/api/close-runs", body: `{"company":"acme","month":"2026-09"}`, contentType: "application/json",
		header: map[string]string{"Authorization": "Bearer " + ts.fix.tokens["asha"]}}
	w = ts.do(t, rq)
	if w.Code != http.StatusCreated {
		t.Fatalf("bearer start = %d: %s", w.Code, w.Body)
	}
	_ = decodeRunID(t, w)
	waitIdle(t, ts)
	ck, _ := ts.session(RoleMaker)
	rq = req{method: http.MethodPost, path: "/api/close-runs", body: `{"company":"globex","month":"2026-09"}`, contentType: "application/json", cookies: []*http.Cookie{ck}}
	if w := ts.do(t, rq); w.Code != http.StatusForbidden {
		t.Errorf("cookie start without a token = %d, want 403", w.Code)
	}
}

func TestRunsEscaping(t *testing.T) {
	fs := newFakeRunsStore()
	rid := runID(1)
	evil := "<script>alert(1)</script> **bold** [x](javascript:alert(1))"
	f := finding(rid, 1, "missing_accrual", "high", paise(1), "open")
	f.Title = evil
	f.Explanation, f.Action = &evil, &evil
	fs.put(store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunFailed, Error: &evil}, f)
	fs.list = []store.RunSummary{{CloseRun: store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunFailed, Error: &evil}, Findings: 1}}
	ts := runsServer(t, fs, nil)

	id := rid.String()
	for _, p := range []string{"/runs", "/runs/" + id, "/runs/" + id + "/progress", "/runs/" + id + "/findings"} {
		body := ts.get(t, RoleMaker, p).Body.String()
		if strings.Contains(body, "<script>alert") {
			t.Errorf("%s: raw script from data", p)
		}
		if n := strings.Count(body, "<script"); n > 2 || unexpectedScripts(body) != 0 {
			t.Errorf("%s: unexpected <script tags (%d)", p, n)
		}
		if strings.Contains(body, "javascript:") && !strings.Contains(body, "&lt;") {
			t.Errorf("%s: javascript: outside escaped text", p)
		}
		if strings.Contains(body, `href="javascript:`) {
			t.Errorf("%s: javascript: link", p)
		}
	}
	for _, p := range []string{"/runs/" + id, "/runs/" + id + "/progress"} {
		if !strings.Contains(ts.get(t, RoleMaker, p).Body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s: error text not shown escaped", p)
		}
	}
	if !strings.Contains(ts.get(t, RoleMaker, "/runs/"+id+"/findings").Body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("title not shown escaped")
	}
	// A validation message echoing user input is escaped too.
	form := url.Values{"company": {"<script>x</script>"}, "month": {"<script>y</script>"}}
	body := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/runs", form.Encode(), "application/x-www-form-urlencoded")).Body.String()
	if strings.Contains(body, "<script>y") || strings.Contains(body, "<script>x") {
		t.Error("form echo not escaped")
	}
}

func TestRunsShutdown(t *testing.T) {
	fs := newFakeRunsStore()
	var finished atomic.Bool
	started := make(chan struct{})
	ts := runsServer(t, fs, func(ctx context.Context, _ uuid.UUID) error {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // shutdown must wait for this
		finished.Store(true)
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- ts.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for ts.runs.baseCtx() == context.Background() {
		if time.Now().After(deadline) {
			t.Fatal("Run never set the runs context")
		}
		time.Sleep(time.Millisecond)
	}
	if w := ts.startJSON(t, "acme", "2026-09"); w.Code != http.StatusCreated {
		t.Fatalf("start = %d", w.Code)
	}
	<-started
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !finished.Load() {
		t.Error("Run returned before the executor did")
	}
	if !strings.Contains(ts.logs.String(), "context canceled") {
		t.Errorf("executor's error not logged: %v", errors.New(ts.logs.String()))
	}
}
