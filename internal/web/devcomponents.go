package web

import (
	"net/http"

	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// handleDevComponents serves /dev/components, the page that shows every
// shared UI component. It is routed only when Options.DevComponents is set.
func (s *Server) handleDevComponents(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	s.render(w, r, http.StatusOK, templates.DevComponents(templates.DevComponentsPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
	}))
}
