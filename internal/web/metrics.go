package web

import (
	"errors"
	"fmt"
	"net/http"

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
	s.render(w, r, http.StatusOK, templates.Metrics(page))
}
