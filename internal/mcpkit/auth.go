package mcpkit

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// Route pairs an MCP server with the bearer token required to access it.
type Route struct {
	Server *mcp.Server
	Token  string
}

// NewRoute returns a route pairing a server with an explicit bearer token.
func NewRoute(server *mcp.Server, token string) Route {
	return Route{Server: server, Token: token}
}

// NewAgentRoute returns a route configured with the agent token from config.
func NewAgentRoute(server *mcp.Server, cfg config.Config) Route {
	return Route{Server: server, Token: cfg.MCPTokenAgent.Reveal()}
}

// NewAdminRoute returns a route configured with the admin token from config.
// The admintoken analyzer permits this access only inside internal/mcpkit/auth*.go.
func NewAdminRoute(server *mcp.Server, cfg config.Config) Route {
	return Route{Server: server, Token: cfg.MCPTokenAdmin.Reveal()}
}

// RequireBearer wraps an HTTP handler with constant-time Bearer token verification.
// Requests without a valid Bearer token receive HTTP 401 Unauthorized.
func RequireBearer(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", `Bearer error="missing_token"`)
			http.Error(w, "Unauthorized: missing Authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
			http.Error(w, "Unauthorized: malformed Authorization header, expected Bearer <token>", http.StatusUnauthorized)
			return
		}

		providedToken := parts[1]
		if subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "Unauthorized: invalid bearer token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
