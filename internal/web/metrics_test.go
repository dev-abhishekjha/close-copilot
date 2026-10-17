package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func metricsServer(t testing.TB, fs *fakeRunsStore, fa *fakeAudit) *testServer {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.Audit = fa
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
}

func TestVerifiedRate(t *testing.T) {
	tests := []struct {
		v, n int
		want string
	}{
		{0, 0, "n/a"}, {0, 4, "0 of 4 (0%)"}, {3, 8, "3 of 8 (38%)"}, {1, 3, "1 of 3 (33%)"}, {2, 3, "2 of 3 (67%)"}, {5, 5, "5 of 5 (100%)"},
	}
	for _, tt := range tests {
		if got := verifiedRate(tt.v, tt.n); got != tt.want {
			t.Errorf("verifiedRate(%d, %d) = %q, want %q", tt.v, tt.n, got, tt.want)
		}
	}
}

func TestRunMetricsPage(t *testing.T) {
	fs := newFakeRunsStore()
	fa := newFakeAudit()
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(95 * time.Second)
	trace := "trace-<abc>"
	run := store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-08", Status: store.RunDone, StartedAt: &t0, FinishedAt: &t1,
		InputTokens: 1830, OutputTokens: 330, CacheReadTokens: 60, CostUSD: 0.0123, TraceID: &trace}
	f := []store.Finding{
		finding(runID(1), 1, "bank_charge", "high", nil, "open"),
		finding(runID(1), 2, "bank_charge", "low", nil, "open"),
		finding(runID(1), 3, "variance", "low", nil, "open"),
	}
	f[0].Verified, f[2].Verified = true, true
	fs.put(run, f...)
	fa.steps[runID(1)] = []store.StepUsage{
		{Kind: "explain", InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 50, CostUSD: "0.0100", Calls: 2, LatencyMS: 900},
		{Kind: "verify", InputTokens: 300, OutputTokens: 30, CacheReadTokens: 10, CostUSD: "0.0023", Calls: 1, LatencyMS: 100},
	}
	ts := metricsServer(t, fs, fa)
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		w := ts.get(t, role, "/runs/"+runID(1).String()+"/metrics")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", role, w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{
			`>Done<`, `Status`, `ui-stat-value">1m35s<`, `ui-stat-value">1830<`, `ui-stat-value">330<`,
			`ui-stat-value">60<`, `ui-stat-value">$0.0123<`, `ui-stat-value">2 of 3 (67%)<`, "Trace", "<code>trace-&lt;abc&gt;</code>",
			`data-step="explain"`, `data-step="verify"`, `<td class="input">1500</td>`, `<td class="cost">$0.0100</td>`, `<td class="calls">2</td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: metrics page lacks %q", role, want)
			}
		}
		if strings.Contains(body, `data-step="investigate"`) || strings.Contains(body, "http://") {
			t.Error("shows an absent step or an external link")
		}
	}
}

func TestRunMetricsNoFindingsNoTrace(t *testing.T) {
	fs := newFakeRunsStore()
	fs.put(store.CloseRun{ID: runID(2), CompanyID: "acme", Month: "2026-08", Status: store.RunRunning})
	ts := metricsServer(t, fs, newFakeAudit())
	body := ts.get(t, RoleViewer, "/runs/"+runID(2).String()+"/metrics").Body.String()
	for _, want := range []string{`ui-stat-value">n/a<`, "No model calls recorded.", `ui-stat-value">n/a<`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, "NaN") || strings.Contains(body, "0%") {
		t.Error("a run without findings must not show a percentage")
	}
}

func TestRunMetricsErrors(t *testing.T) {
	fs := newFakeRunsStore()
	ts := metricsServer(t, fs, newFakeAudit())
	for path, want := range map[string]int{
		"/runs/" + runID(9).String() + "/metrics": http.StatusNotFound,
		"/runs/not-a-uuid/metrics":                http.StatusBadRequest,
	} {
		if got := ts.get(t, RoleViewer, path).Code; got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	fs.put(store.CloseRun{ID: runID(3), CompanyID: "acme", Month: "2026-08", Status: store.RunDone})
	fa := newFakeAudit()
	fa.failAll = http.ErrAbortHandler
	ts = metricsServer(t, fs, fa)
	if got := ts.get(t, RoleViewer, "/runs/"+runID(3).String()+"/metrics").Code; got != http.StatusInternalServerError {
		t.Errorf("store error = %d", got)
	}
}

func TestRunMetricsLinkedFromRunPage(t *testing.T) {
	fs := newFakeRunsStore()
	fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-08", Status: store.RunDone})
	ts := runsServer(t, fs, nil)
	body := ts.get(t, RoleViewer, "/runs/"+runID(1).String()).Body.String()
	if !strings.Contains(body, `href="/runs/`+runID(1).String()+`/metrics"`) {
		t.Error("run page does not link to the metrics page")
	}
}

func TestRunsSummaryCard(t *testing.T) {
	fs := newFakeRunsStore()
	fa := newFakeAudit()
	ts := metricsServer(t, fs, fa)
	body := ts.get(t, RoleViewer, "/runs").Body.String()
	if !strings.Contains(body, "No finished runs yet.") {
		t.Error("empty summary not explained")
	}
	fa.stats = store.LastRunStats{Found: true, RunID: runID(1), CompanyID: "acme", Month: "2026-08", Total: 8, Verified: 6, OpenHigh: 2}
	body = ts.get(t, RoleViewer, "/runs").Body.String()
	for _, want := range []string{
		`class="summary-run" href="/runs/` + runID(1).String() + `"`, "acme 2026-08",
		`class="summary-verified" href="/runs/` + runID(1).String() + `/metrics">6 of 8 (75%)<`,
		`class="summary-high" href="/runs/` + runID(1).String() + `/findings">2<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	// No findings: n/a, not 0%.
	fa.stats.Total, fa.stats.Verified = 0, 0
	if b := ts.get(t, RoleViewer, "/runs").Body.String(); !strings.Contains(b, ">n/a<") {
		t.Error("summary without findings lacks n/a")
	}
	// Without the audit backend the runs page has no summary at all.
	if b := runsServer(t, fs, nil).get(t, RoleViewer, "/runs").Body.String(); strings.Contains(b, "runs-summary") {
		t.Error("summary shown without a backend")
	}
	fa.failAll = http.ErrAbortHandler
	if got := ts.get(t, RoleViewer, "/runs").Code; got != http.StatusInternalServerError {
		t.Errorf("stats error = %d", got)
	}
}

func metricsFixture(t *testing.T) (*testServer, *fakeAudit, string) {
	t.Helper()
	fs := newFakeRunsStore()
	fa := newFakeAudit()
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(95 * time.Second)
	trace := "trace-<abc>"
	fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-08", Status: store.RunDone, StartedAt: &t0, FinishedAt: &t1,
		InputTokens: 1830, OutputTokens: 330, CacheReadTokens: 60, CostUSD: 0.0123, TraceID: &trace})
	fa.steps[runID(1)] = []store.StepUsage{
		{Kind: "explain", InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 50, CostUSD: "0.0100", Calls: 2, LatencyMS: 900},
		{Kind: "verify", InputTokens: 300, OutputTokens: 30, CacheReadTokens: 10, CostUSD: "0.0023", Calls: 1, LatencyMS: 100},
	}
	return metricsServer(t, fs, fa), fa, "/runs/" + runID(1).String() + "/metrics"
}

func TestMetricsPageTiles(t *testing.T) {
	ts, _, path := metricsFixture(t)
	body := ts.get(t, RoleViewer, path).Body.String()
	for _, want := range []string{
		"Status", ">Done<", `ui-stat-value">1m35s<`, `ui-stat-value">1830<`, `ui-stat-value">330<`, `ui-stat-value">60<`,
		`ui-stat-value">$0.0123<`, "Trace:", "<code>trace-&lt;abc&gt;</code>", `href="/runs"`, `href="/runs/` + runID(1).String() + `/findings"`,
		`data-step="explain"`, `data-step="verify"`, `<td class="input">1500</td>`, `<td class="cost">$0.0100</td>`, `<td class="calls">2</td>`,
		`<td class="latency">900</td>`, `id="metrics"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	// No findings: n/a in the verified tile, not a percentage.
	if !strings.Contains(body, `ui-stat-value">n/a<`) || strings.Contains(body, "NaN") {
		t.Error("verified rate n/a missing")
	}
}

func TestMetricsPageCharts(t *testing.T) {
	ts, fa, path := metricsFixture(t)
	body := ts.get(t, RoleViewer, path).Body.String()
	for _, want := range []string{
		`<title id=`, ">Tokens by step<", ">Cost by step<", `aria-labelledby=`,
		"<td>1500</td><td>300</td><td>50</td>", "<td>0.01</td>", "<td>0.0023</td>", "<summary>Data</summary>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("charts lack %q", want)
		}
	}
	// 2 steps x 3 series + 2 steps x 1 series.
	if n := strings.Count(body, `<rect class="chart-bar`); n != 8 {
		t.Errorf("bars = %d, want 8", n)
	}
	if strings.Contains(body, "No usage recorded") {
		t.Error("empty state shown with usage")
	}
	// An all-zero run.
	fa.steps[runID(1)] = []store.StepUsage{{Kind: "explain", CostUSD: "0", Calls: 1}}
	body = ts.get(t, RoleViewer, path).Body.String()
	if strings.Count(body, "No usage recorded") != 2 || strings.Contains(body, `<rect class="chart-bar`) {
		t.Error("all-zero run should show No usage recorded for both charts")
	}
	// No steps at all: no chart cards.
	delete(fa.steps, runID(1))
	body = ts.get(t, RoleViewer, path).Body.String()
	if strings.Contains(body, "chart-svg") || !strings.Contains(body, "No model calls recorded.") {
		t.Error("no-steps page wrong")
	}
}

func TestMetricsPageTrend(t *testing.T) {
	ts, fa, path := metricsFixture(t)
	pt := func(n int, cost float64, f, v int) store.RunTrendPoint {
		return store.RunTrendPoint{RunID: runID(byte(n)), CompanyID: "acme", Month: "2026-0" + string(rune('0'+n)),
			StartedAt: time.Date(2026, 9, n, 9, 0, 0, 0, time.UTC), CostUSD: cost, Findings: f, Verified: v}
	}
	get := func() string { return ts.get(t, RoleViewer, path).Body.String() }

	for _, n := range []int{0, 1} {
		fa.trend = []store.RunTrendPoint{pt(3, 1, 4, 2)}[:n]
		if strings.Contains(get(), "Recent runs") {
			t.Errorf("trend shown with %d points", n)
		}
	}
	fa.trend = []store.RunTrendPoint{pt(3, 3, 4, 2), pt(2, 2, 0, 0), pt(1, 1, 5, 5)} // newest first
	body := get()
	for _, want := range []string{"Recent runs", ">Cost per run<", ">Verified rate per run<", "<polyline", "<td>50</td>", "<td>0</td>", "<td>100</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("trend lacks %q", want)
		}
	}
	if strings.Index(body, "acme 2026-01") > strings.Index(body, "acme 2026-03") {
		t.Error("newest run is not on the right")
	}

	fa.trendErr = http.ErrAbortHandler
	w := ts.get(t, RoleViewer, path)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Recent runs") {
		t.Errorf("trend error: %d", w.Code)
	}
	if strings.Contains(ts.logs.String(), "acme") {
		t.Error("log contains run data")
	}

	// A store without the optional method.
	fs := newFakeRunsStore()
	fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-08", Status: store.RunDone})
	bare := newTestServer(t, func(o *Options) {
		o.Audit = struct{ AuditStore }{fa}
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
	fa.trendErr = nil
	if b := bare.get(t, RoleViewer, path).Body.String(); strings.Contains(b, "Recent runs") {
		t.Error("trend shown without the optional method")
	}
}
