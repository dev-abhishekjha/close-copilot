package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// metricsRoute answers 501 until both the runs and the audit backends are
// set; the role check has run by then.
func (s *Server) metricsRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runs == nil || s.audit == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		h(w, r)
	})
}

// verifiedRate formats verified of total as "N of M (P%)", or "n/a" when
// there are no findings.
func verifiedRate(verified, total int) string {
	if total <= 0 {
		return "n/a"
	}
	pct := (verified*100 + total/2) / total
	return fmt.Sprintf("%d of %d (%d%%)", verified, total, pct)
}

func (s *Server) handleRunMetrics(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	run, err := s.runs.b.Store.GetCloseRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), id)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	steps, err := s.audit.RunMetricsByStep(r.Context(), id)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	verified := 0
	for _, f := range fs {
		if f.Verified {
			verified++
		}
	}
	page := templates.MetricsPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		RunID: id.String(), Company: run.CompanyID, Month: run.Month, Status: run.Status,
		Duration: runDuration(run), Input: run.InputTokens, Output: run.OutputTokens, Cache: run.CacheReadTokens,
		Cost: formatCost(run.CostUSD), VerifiedRate: verifiedRate(verified, len(fs)),
	}
	if page.Duration == "" {
		page.Duration = "n/a"
	}
	if run.TraceID != nil {
		page.TraceID = *run.TraceID
	}
	for _, u := range steps {
		page.Steps = append(page.Steps, templates.StepRow{
			Kind: u.Kind, Input: u.InputTokens, Output: u.OutputTokens, Cache: u.CacheReadTokens,
			Cost: "$" + u.CostUSD, Calls: u.Calls, Latency: u.LatencyMS,
		})
	}
	page.TokenChart, page.CostChart = stepCharts(steps)
	page.Trend = s.trendView(r.Context())
	s.render(w, r, http.StatusOK, templates.Metrics(page))
}

// trendLimit is how many recent runs the trend charts show.
const trendLimit = 12

// runTrender is the optional trend read of the audit backend.
type runTrender interface {
	RecentRunTrend(context.Context, int) ([]store.RunTrendPoint, error)
}

// stepCharts builds the tokens and cost bar charts from the step usage.
func stepCharts(steps []store.StepUsage) (tokens, cost templates.ChartProps) {
	var cats []string
	var in, out, cache, usd []float64
	for _, u := range steps {
		cats = append(cats, u.Kind)
		in = append(in, float64(u.InputTokens))
		out = append(out, float64(u.OutputTokens))
		cache = append(cache, float64(u.CacheReadTokens))
		c, err := strconv.ParseFloat(u.CostUSD, 64)
		if err != nil {
			c = 0
		}
		usd = append(usd, c)
	}
	tokens = templates.ChartProps{
		Title: "Tokens by step", Desc: "Input, output and cache-read tokens for each step family of this run.", Unit: "tokens",
		Categories: cats, Series: []templates.Series{{Name: "Input", Values: in}, {Name: "Output", Values: out}, {Name: "Cache read", Values: cache}},
	}
	cost = templates.ChartProps{
		Title: "Cost by step", Desc: "Model cost in US dollars for each step family of this run.", Unit: "USD",
		Categories: cats, Series: []templates.Series{{Name: "Cost", Values: usd}},
	}
	return tokens, cost
}

// trendView returns the recent-runs charts, or nil when the store does not
// offer them, fails, or has fewer than two finished runs.
func (s *Server) trendView(ctx context.Context) *templates.TrendView {
	tr, ok := s.audit.(runTrender)
	if !ok {
		return nil
	}
	pts, err := tr.RecentRunTrend(ctx, trendLimit)
	if err != nil {
		s.log.WarnContext(ctx, "web: run trend unavailable")
		return nil
	}
	if len(pts) < 2 {
		return nil
	}
	var cats []string
	var cost, pct []float64
	for i := len(pts) - 1; i >= 0; i-- { // newest on the right
		p := pts[i]
		cats = append(cats, p.CompanyID+" "+p.Month)
		cost = append(cost, p.CostUSD)
		v := 0.0
		if p.Findings > 0 {
			v = float64(p.Verified) * 100 / float64(p.Findings)
		}
		pct = append(pct, v)
	}
	return &templates.TrendView{
		Cost: templates.ChartProps{
			Title: "Cost per run", Desc: "Model cost in US dollars of the most recent finished runs.", Unit: "USD",
			Categories: cats, Series: []templates.Series{{Name: "Cost", Values: cost}},
		},
		Verified: templates.ChartProps{
			Title: "Verified rate per run", Desc: "Percent of findings verified in the most recent finished runs.", Unit: "%",
			Categories: cats, Series: []templates.Series{{Name: "Verified", Values: pct}},
		},
	}
}
