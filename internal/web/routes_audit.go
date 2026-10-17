package web

import "net/http"

// auditRoutes adds the audit CSV export; the audit page itself stays in routes.go.
func (s *Server) auditRoutes() []route {
	return []route{{http.MethodGet, "/audit/export.csv", checkerOnly, s.auditRoute(s.handleAuditExport)}}
}
