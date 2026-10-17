package web

import "net/http"

// proposalsRoutes is filled by UI-09: the proposal page, for any signed-in
// role. It answers 501 until Options.Approvals is set.
func (s *Server) proposalsRoutes() []route {
	return []route{
		{http.MethodGet, "/proposals/{id}", anyRole, s.approvalsRoute(s.handleProposalPage)},
	}
}
