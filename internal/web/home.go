package web

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// homeAction is an action the home page may offer. Whether it is shown is
// decided by the same route table the middleware enforces. href is the UI
// page for it, empty when there is none yet; it is linked only when the
// role may GET it.
type homeAction struct {
	id, label, method, path, href string
}

var homeActions = []homeAction{
	{"view-run", "View a close run's status", http.MethodGet, "/api/close-runs/{id}", "/runs"},
	{"view-findings", "Review a run's findings", http.MethodGet, "/api/close-runs/{id}/findings", "/runs"},
	{"ask", "Ask a question about the books", http.MethodPost, "/api/ask", "/ask"},
	{"start-run", "Start a close run", http.MethodPost, "/api/close-runs", "/runs"},
	{"propose-entry", "Propose a journal entry for a finding", http.MethodPost, "/api/findings/{id}/proposals", ""},
	{"approve-proposal", "Approve and post a proposal", http.MethodPost, "/api/proposals/{id}/approve", ""},
	{"reject-proposal", "Reject a proposal", http.MethodPost, "/api/proposals/{id}/reject", ""},
	{"approvals", "Review pending proposals", http.MethodGet, "/approvals", "/approvals"},
	{"audit", "Audit log", http.MethodGet, "/audit", "/audit"},
}

// Dashboard bounds: every list is capped and every call uses the request
// context, with no goroutines and no caches.
const (
	dashRunLimit      = 50 // one ListCloseRuns call
	dashCompanyLimit  = 12 // companies shown in the latest-run table
	dashWaitingLimit  = 5  // linked items in the waiting card
	dashSectionFailed = "This section could not be loaded. Try again shortly."
	dashNotConfigured = "Close runs are not configured on this server"
)

// handleHome is the dashboard. Each section is built by its own helper from
// a source that may be missing or failing, so one failure never breaks the
// page. The display is a convenience; the middleware stays the authority.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	ctx := r.Context()
	role := p.user.Role
	company, month := s.contextFor(r)
	v := templates.DashboardView{
		User: p.user.Name, Role: string(role), CSRFToken: p.csrf,
		Company: company, Month: month,
		Actions: s.homeQuickActions(role),
		Pages:   s.homeDestinations(role),
	}
	if s.allows(role, http.MethodPost, "/runs") && company != "" {
		v.StartHref = templates.DashStartHref(company, month)
	}
	if s.allows(role, http.MethodGet, "/ask") {
		v.AskHref = "/ask"
	}

	var runs []store.RunSummary
	var runsErr error
	if s.runs != nil {
		runs, runsErr = s.runs.b.Store.ListCloseRuns(ctx, dashRunLimit)
		if runsErr != nil {
			s.log.ErrorContext(ctx, "web: dashboard: list runs", "err", runsErr)
		}
	}
	cur := currentRun(runs, company, month)
	var findings []store.Finding
	var findingsErr error
	if s.runs != nil && runsErr == nil && cur != nil {
		findings, findingsErr = s.runs.b.Store.ListFindingsByRun(ctx, cur.ID)
		if findingsErr != nil {
			s.log.ErrorContext(ctx, "web: dashboard: list findings", "run", cur.ID, "err", findingsErr)
		}
	}

	v.Context = s.dashContext(role, company, cur, findings, findingsErr, runsErr)
	v.Latest = s.dashLatest(r, role, runs, runsErr)
	v.Waiting = s.dashWaiting(ctx, role, cur, findings, findingsErr)
	s.render(w, r, http.StatusOK, templates.Home(v))
}

// homeQuickActions are the actions the role may take, with a link where a
// page exists and the role may GET it.
func (s *Server) homeQuickActions(role Role) []templates.Action {
	var actions []templates.Action
	for _, a := range homeActions {
		if !s.allows(role, a.method, a.path) {
			continue
		}
		act := templates.Action{ID: a.id, Label: a.label, Method: a.method, Path: a.path}
		if a.href != "" && s.allows(role, http.MethodGet, a.href) {
			act.Href = a.href
		}
		actions = append(actions, act)
	}
	return actions
}

// currentRun is the newest run for company and month in runs (newest first).
func currentRun(runs []store.RunSummary, company, month string) *store.RunSummary {
	for i := range runs {
		if runs[i].CompanyID == company && runs[i].Month == month {
			return &runs[i]
		}
	}
	return nil
}

var dashSeverities = []string{"high", "medium", "low"}
var dashStatuses = []string{"open", "needs_review", "accepted", "dismissed"}

// dashContext summarises the context run.
func (s *Server) dashContext(role Role, company string, cur *store.RunSummary, fs []store.Finding, fsErr, runsErr error) templates.DashContext {
	switch {
	case s.runs == nil:
		return templates.DashContext{Off: true}
	case runsErr != nil:
		return templates.DashContext{Err: dashSectionFailed}
	case company == "":
		return templates.DashContext{NoCompany: true}
	case cur == nil:
		return templates.DashContext{}
	}
	c := templates.DashContext{HasRun: true, RunID: cur.ID.String(), Status: cur.Status}
	if s.allows(role, http.MethodGet, "/runs/{id}") {
		c.RunHref = "/runs/" + cur.ID.String()
	}
	if fsErr != nil {
		c.FindingsErr = dashSectionFailed
		return c
	}
	sev := map[string]int{}
	st := map[string]int{}
	for _, f := range fs {
		sev[strings.ToLower(f.Severity)]++
		st[f.Status]++
		if !f.Verified {
			c.Unverified++
		}
	}
	c.Total = len(fs)
	c.High = sev["high"]
	c.NeedsReview = st["needs_review"]
	for _, k := range dashSeverities {
		c.Severity = append(c.Severity, templates.DashBar{Severity: k, Count: sev[k], Max: max(c.Total, 1)})
	}
	for _, k := range dashStatuses {
		c.Statuses = append(c.Statuses, templates.DashCount{Status: k, Count: st[k]})
	}
	return c
}

// dashLatest is the newest run per company, with companies that have no run.
func (s *Server) dashLatest(r *http.Request, role Role, runs []store.RunSummary, runsErr error) templates.DashLatest {
	if s.runs == nil {
		return templates.DashLatest{Off: true}
	}
	if runsErr != nil {
		return templates.DashLatest{Err: dashSectionFailed}
	}
	latest := map[string]store.RunSummary{}
	var order []string
	for _, id := range s.shellCompanies(r.Context()) {
		if _, ok := latest[id]; !ok && !slices.Contains(order, id) {
			order = append(order, id)
		}
	}
	for _, run := range runs {
		if _, ok := latest[run.CompanyID]; ok {
			continue
		}
		latest[run.CompanyID] = run
		if !slices.Contains(order, run.CompanyID) {
			order = append(order, run.CompanyID)
		}
	}
	link := s.allows(role, http.MethodGet, "/runs/{id}")
	out := templates.DashLatest{}
	for i, id := range order {
		if i >= dashCompanyLimit {
			out.More = len(order) - dashCompanyLimit
			break
		}
		row := templates.DashCompanyRow{Company: id}
		if run, ok := latest[id]; ok {
			row.HasRun, row.Link = true, link
			row.RunID, row.Month, row.Status, row.Findings = run.ID.String(), run.Month, run.Status, run.Findings
			row.Started = "Queued"
			if run.StartedAt != nil {
				row.Started = run.StartedAt.UTC().Format("2006-01-02 15:04")
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

// dashWaiting is the work waiting on the user, by role.
func (s *Server) dashWaiting(ctx context.Context, role Role, cur *store.RunSummary, fs []store.Finding, fsErr error) templates.DashWaiting {
	switch role {
	case RoleChecker:
		w := templates.DashWaiting{Kind: "checker"}
		if s.approvals == nil {
			w.Off = true
			return w
		}
		list, err := s.approvals.Pending(ctx)
		if err != nil {
			s.log.ErrorContext(ctx, "web: dashboard: pending proposals", "err", err)
			w.Err = dashSectionFailed
			return w
		}
		w.Count = len(list)
		for i, p := range list {
			if i >= dashWaitingLimit {
				break
			}
			// There is no page for one proposal yet: link to the list.
			w.Items = append(w.Items, templates.DashItem{Label: p.CompanyID + " by " + p.Maker, Href: "/approvals"})
		}
		w.ViewAll, w.ViewAllLabel = "/approvals", "Review pending proposals"
		return w
	case RoleMaker:
		w := templates.DashWaiting{Kind: "maker"}
		switch {
		case s.runs == nil:
			w.Off = true
		case fsErr != nil:
			w.Err = dashSectionFailed
		case cur != nil:
			for _, f := range fs {
				if f.Status != "open" && f.Status != "needs_review" {
					continue
				}
				w.Count++
				if len(w.Items) < dashWaitingLimit {
					w.Items = append(w.Items, templates.DashItem{Label: f.Title, Href: "/runs/" + cur.ID.String() + "/findings"})
				}
			}
			w.ViewAll, w.ViewAllLabel = "/runs/"+cur.ID.String()+"/findings", "Open the findings"
		}
		return w
	}
	return templates.DashWaiting{Kind: "viewer"}
}
