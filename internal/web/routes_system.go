package web

import "net/http"

// systemRoutes is filled by UI-12; it adds that page's routes here, not in routes.go.
func (s *Server) systemRoutes() []route {
	return []route{{http.MethodGet, "/system", anyRole, s.systemRoute(s.handleSystemPage)}}
}
