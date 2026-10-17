package web

import "net/http"

// documentsRoutes is filled by UI-11; it adds that page's routes here, not in routes.go.
func (s *Server) documentsRoutes() []route {
	return []route{
		{http.MethodGet, "/documents", anyRole, s.documentsRoute(s.handleDocumentsPage)},
		{http.MethodGet, "/documents/{id}", anyRole, s.documentsRoute(s.handleDocumentPage)},
	}
}
