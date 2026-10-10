package fakeerp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Token is the agent token the fake MCP servers accept by default.
//
//nolint:gosec // G101: a fixed token for loopback test servers.
const Token = "fakeerp-agent-token"

// Server is a fake ERPNext served over HTTP on loopback, with an ERPNext
// client pointed at it.
type Server struct {
	ERP    *ERP
	URL    string
	Client *frappe.Client
	srv    *httptest.Server
}

// Serve serves f on a loopback port.
func Serve(f *ERP) (*Server, error) {
	srv := httptest.NewServer(f)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "fakeerp-key", config.NewSecret("fakeerp-secret"))
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("fakeerp: client: %w", err)
	}
	return &Server{ERP: f, URL: srv.URL, Client: c, srv: srv}, nil
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Companies resolves the synthetic company IDs to their ERPNext names for
// the books tools.
type Companies map[string]string

// DefaultCompanies maps both synthetic companies.
func DefaultCompanies() Companies {
	return Companies{CompanyID: ERPCompany, OtherID: OtherERP}
}

// ERPCompany implements books.CompanyResolver.
func (c Companies) ERPCompany(id string) (string, error) {
	if n, ok := c[id]; ok {
		return n, nil
	}
	return "", fmt.Errorf("%w %q", books.ErrUnknownCompany, id)
}

// Discard is a logger that drops everything.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// BooksMCPServer is the real books MCP server (books.RegisterTools) over
// client.
func BooksMCPServer(client *frappe.Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-books", Version: "fakeerp"}, nil)
	books.RegisterTools(s, books.ToolDeps{Client: client, Companies: DefaultCompanies(), Logger: Discard()})
	return s
}

// EvidenceMCPServer is the real evidence MCP server over st.
func EvidenceMCPServer(st *store.Store) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-evidence", Version: "fakeerp"}, nil)
	evidence.RegisterTools(s, st)
	return s
}

// MCPEndpoint is an MCP server served at /mcp behind a token.
type MCPEndpoint struct {
	URL string // ends in /mcp
	srv *httptest.Server
}

// ServeMCP serves s at /mcp behind token on a loopback port.
func ServeMCP(s *mcp.Server, token string) *MCPEndpoint {
	srv := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, token)}, Discard()))
	return &MCPEndpoint{URL: srv.URL + "/mcp", srv: srv}
}

// Close stops the endpoint.
func (e *MCPEndpoint) Close() { e.srv.Close() }

// Stack is the synthetic month served end to end: the fake ERPNext, the
// books MCP server over it, and the evidence MCP server over a store.
type Stack struct {
	ERP         *Server
	BooksURL    string
	EvidenceURL string
	Token       string

	closers []func()
}

// Start serves NewMonth and both MCP servers, with the evidence server
// reading st. The agent reaches them with token ("" means Token).
func Start(st *store.Store, token string) (*Stack, error) {
	if st == nil {
		return nil, errors.New("fakeerp: start: nil store")
	}
	if token == "" {
		token = Token
	}
	erp, err := Serve(NewMonth())
	if err != nil {
		return nil, err
	}
	b := ServeMCP(BooksMCPServer(erp.Client), token)
	e := ServeMCP(EvidenceMCPServer(st), token)
	return &Stack{
		ERP: erp, BooksURL: b.URL, EvidenceURL: e.URL, Token: token,
		closers: []func(){e.Close, b.Close, erp.Close},
	}, nil
}

// Close stops every server of the stack.
func (s *Stack) Close() {
	for _, c := range s.closers {
		c()
	}
	s.closers = nil
}
