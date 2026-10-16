package web

import (
	"net/http"

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
	{"ask", "Ask a question about the books", http.MethodPost, "/api/ask", ""},
	{"start-run", "Start a close run", http.MethodPost, "/api/close-runs", "/runs"},
	{"propose-entry", "Propose a journal entry for a finding", http.MethodPost, "/api/findings/{id}/proposals", ""},
	{"approve-proposal", "Approve and post a proposal", http.MethodPost, "/api/proposals/{id}/approve", ""},
	{"reject-proposal", "Reject a proposal", http.MethodPost, "/api/proposals/{id}/reject", ""},
	{"approvals", "Review pending proposals", http.MethodGet, "/approvals", "/approvals"},
	{"audit", "Audit log", http.MethodGet, "/audit", "/audit"},
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
		if !s.allows(p.user.Role, a.method, a.path) {
			continue
		}
		act := templates.Action{ID: a.id, Label: a.label, Method: a.method, Path: a.path}
		if a.href != "" && s.allows(p.user.Role, http.MethodGet, a.href) {
			act.Href = a.href
		}
		actions = append(actions, act)
	}
	s.render(w, r, http.StatusOK, templates.Home(templates.HomePage{
		User:         p.user.Name,
		Role:         string(p.user.Role),
		CSRFToken:    p.csrf,
		Actions:      actions,
		Destinations: s.homeDestinations(p.user.Role),
	}))
}
