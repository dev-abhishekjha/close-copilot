package web

import "net/http"

// findingsListRoutes adds the findings list and its bulk accept (UI-06). The
// literal /findings/accept-selected beats /findings/{id}/accept: a POST
// there has two segments and the pattern needs three.
func (s *Server) findingsListRoutes() []route {
	return []route{
		{http.MethodGet, "/findings", anyRole, s.findingsHTML(s.handleFindingsList)},
		{http.MethodPost, "/findings/accept-selected", makerOnly, s.findingsHTML(s.handleFindingsAcceptSelected)},
	}
}
