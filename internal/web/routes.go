package web

import (
	"net/http"
	"slices"

	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// Allowed role sets. Roles are not a ladder: maker routes allow only makers
// and checker routes only checkers (separation of duties).
var (
	anyRole     = []Role{RoleViewer, RoleMaker, RoleChecker}
	makerOnly   = []Role{RoleMaker}
	checkerOnly = []Role{RoleChecker}
)

// route is one row of the route table. roles is the allowed set; nil means
// the route is public (no session needed).
type route struct {
	method  string
	path    string
	roles   []Role
	handler http.Handler
}

// routeBodyLimits caps the body of some routes below the server-wide limit,
// before anything (the CSRF check parses a form body) reads it.
var routeBodyLimits = map[string]int64{
	http.MethodPost + " /findings/{id}/proposals":     approvalsBodyLimit,
	http.MethodPost + " /proposals/{id}/approve":      approvalsBodyLimit,
	http.MethodPost + " /proposals/{id}/reject":       approvalsBodyLimit,
	http.MethodPost + " /api/findings/{id}/proposals": approvalsBodyLimit,
	http.MethodPost + " /api/proposals/{id}/approve":  approvalsBodyLimit,
	http.MethodPost + " /api/proposals/{id}/reject":   approvalsBodyLimit,
}

// routeTable is every route the server serves with its minimum role. The
// API rows come from the shared HTTP API spec; rows owned by later tickets
// answer 501 after the role check so the role matrix is testable now.
func (s *Server) routeTable(static http.Handler) []route {
	return []route{
		// Public.
		{http.MethodGet, "/healthz", nil, http.HandlerFunc(handleHealthz)},
		{http.MethodGet, "/login", nil, http.HandlerFunc(s.handleLoginForm)},
		{http.MethodPost, "/login", nil, http.HandlerFunc(s.handleLogin)},
		{http.MethodGet, "/static/", nil, static},

		// Pages.
		{http.MethodGet, "/{$}", anyRole, http.HandlerFunc(s.handleHome)},
		{http.MethodPost, "/logout", anyRole, http.HandlerFunc(s.handleLogout)},
		// Any other GET needs a session before it learns the page is
		// missing, so anonymous callers can't map the site.
		{http.MethodGet, "/", anyRole, http.NotFoundHandler()},

		// Close runs (CC-1002). They answer 501 until Options.Runs is set.
		{http.MethodGet, "/runs", anyRole, s.runsHTML(s.handleRunsPage)},
		{http.MethodPost, "/runs", makerOnly, s.runsHTML(s.handleRunsStartForm)},
		{http.MethodGet, "/runs/{id}", anyRole, s.runsHTML(s.handleRunPage)},
		{http.MethodGet, "/runs/{id}/progress", anyRole, s.runsHTML(s.handleRunProgress)},
		{http.MethodGet, "/runs/{id}/findings", anyRole, s.runsHTML(s.handleRunFindingsPage)},

		// Finding detail (CC-1003). They answer 501 until Runs.Findings is set.
		{http.MethodGet, "/findings/{id}", anyRole, s.findingsHTML(s.handleFindingPage)},
		{http.MethodPost, "/findings/{id}/accept", makerOnly, s.findingsHTML(s.handleFindingAccept)},
		{http.MethodPost, "/findings/{id}/dismiss", makerOnly, s.findingsHTML(s.handleFindingDismiss)},

		// Maker-checker approvals (CC-1004). They answer 501 until
		// Options.Approvals is set. The HTML form posts are twins of the API.
		{http.MethodPost, "/findings/{id}/proposals", makerOnly, s.approvalsRoute(s.handleProposalCreate)},
		{http.MethodGet, "/approvals", checkerOnly, s.approvalsRoute(s.handleApprovalsPage)},
		{http.MethodPost, "/proposals/{id}/approve", checkerOnly, s.approvalsRoute(s.handleProposalApprove)},
		{http.MethodPost, "/proposals/{id}/reject", checkerOnly, s.approvalsRoute(s.handleProposalReject)},

		// Audit log and run metrics (CC-1005). They answer 501 until
		// Options.Audit is set. Audit rows hold unmasked tool arguments, so
		// the audit page is for checkers.
		{http.MethodGet, "/audit", checkerOnly, s.auditRoute(s.handleAuditPage)},
		{http.MethodGet, "/runs/{id}/metrics", anyRole, s.metricsRoute(s.handleRunMetrics)},

		// JSON API (shared spec).
		{http.MethodPost, "/api/close-runs", makerOnly, s.runsAPI(s.handleAPIStartRun)},
		{http.MethodGet, "/api/close-runs/{id}", anyRole, s.runsAPI(s.handleAPIRun)},
		{http.MethodGet, "/api/close-runs/{id}/findings", anyRole, s.runsAPI(s.handleAPIRunFindings)},
		{http.MethodPost, "/api/findings/{id}/proposals", makerOnly, s.approvalsRoute(s.handleProposalCreate)},
		{http.MethodPost, "/api/proposals/{id}/approve", checkerOnly, s.approvalsRoute(s.handleProposalApprove)},
		{http.MethodPost, "/api/proposals/{id}/reject", checkerOnly, s.approvalsRoute(s.handleProposalReject)},
		{http.MethodPost, "/api/ask", anyRole, s.askAPI(s.handleAPIAsk)},
	}
}

// wrap puts a route's handler behind authentication, the CSRF check and its
// role check. Public routes are served as they are.
func (s *Server) wrap(rt route) http.Handler {
	if rt.roles == nil {
		return rt.handler
	}
	n, capped := routeBodyLimits[rt.method+" "+rt.path]
	inner := requireRole(rt.roles...)(rt.handler)
	if !capped {
		// After the CSRF check, which reads a form body first.
		inner = capAPIBody(inner)
	}
	inner = s.csrfProtect(inner)
	// The per-user limiter runs after authenticate, on /api/* only.
	h := s.authenticate(s.rateLimit(inner))
	if capped {
		h = capBody(n, h)
	}
	return h
}

// allows reports whether role may call method path per the route table.
func (s *Server) allows(role Role, method, path string) bool {
	for _, rt := range s.routes {
		if rt.method == method && rt.path == path {
			return rt.roles != nil && slices.Contains(rt.roles, role)
		}
	}
	return false
}

// homeAction is an action the home page may offer. Whether it is shown is
// decided by the same route table the middleware enforces.
type homeAction struct {
	id, label, method, path string
}

var homeActions = []homeAction{
	{"view-run", "View a close run's status", http.MethodGet, "/api/close-runs/{id}"},
	{"view-findings", "Review a run's findings", http.MethodGet, "/api/close-runs/{id}/findings"},
	{"ask", "Ask a question about the books", http.MethodPost, "/api/ask"},
	{"start-run", "Start a close run", http.MethodPost, "/api/close-runs"},
	{"propose-entry", "Propose a journal entry for a finding", http.MethodPost, "/api/findings/{id}/proposals"},
	{"approve-proposal", "Approve and post a proposal", http.MethodPost, "/api/proposals/{id}/approve"},
	{"reject-proposal", "Reject a proposal", http.MethodPost, "/api/proposals/{id}/reject"},
	{"approvals", "Review pending proposals", http.MethodGet, "/approvals"},
	{"audit", "Audit log", http.MethodGet, "/audit"},
}

// handleHome shows the user's name, role and the actions their role allows.
// The display is a convenience; the middleware stays the authority.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	var actions []templates.Action
	for _, a := range homeActions {
		if s.allows(p.user.Role, a.method, a.path) {
			actions = append(actions, templates.Action{ID: a.id, Label: a.label, Method: a.method, Path: a.path})
		}
	}
	s.render(w, r, http.StatusOK, templates.Home(templates.HomePage{
		User:      p.user.Name,
		Role:      string(p.user.Role),
		CSRFToken: p.csrf,
		Actions:   actions,
	}))
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// notImplemented is the stub for API rows a later ticket implements. It runs
// after the role check.
var notImplemented = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
})
