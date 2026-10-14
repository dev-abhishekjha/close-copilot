// Command mcp-books is the Books MCP server: read-only ERPNext tools at /mcp and the approved-write tool at /mcp-admin.
//
// Built in CC-501, CC-502, CC-504b.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
)

func main() {
	cli.Main("mcp-books", []string{
		config.EnvERPBaseURL,
		config.EnvERPSite,
		config.EnvERPAPIKey,
		config.EnvERPAPISecret,
		config.EnvDatabaseURL,
		config.EnvMCPTokenAgent,
		config.EnvMCPTokenAdmin,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("mcp-books", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	transport := fs.String("transport", "http", "transport: http (scoped: needs MCP_SCOPE_KEY, and every /mcp request a valid X-Run-Scope) or stdio (unscoped: a local process with no network)")
	addr := fs.String("addr", ":7001", "listen address for HTTP transport")
	companies := fs.String("companies", "config/companies", "directory of company profiles (<id>.yaml) that map company IDs to ERPNext company names")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("mcp-books: parse flags: %w", err)
	}

	stopTrace, err := telemetry.Init(ctx, "mcp-books", cfg, log)
	if err != nil {
		return err
	}
	defer stopTrace()

	if *transport != "http" && *transport != "stdio" {
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}
	if *transport == "http" && cfg.MCPScopeKey.IsZero() {
		return errNoScopeKey
	}

	// Fail on bad company profiles before touching the database.
	if _, err := seed.LoadProfiles(*companies); err != nil {
		return fmt.Errorf("mcp-books: company profiles: %w", err)
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("mcp-books: connect to database: %w", err)
	}
	defer st.Close()

	switch *transport {
	case "stdio":
		// Unscoped: a local process with no network, driven by one user.
		booksServer, err := newBooksServer(cfg, *companies, log, nil, st)
		if err != nil {
			return err
		}
		log.Info("starting mcp-books in stdio mode")
		return mcpkit.RunStdio(ctx, booksServer)
	case "http":
		routes, opts, err := httpRoutes(cfg, *companies, log, st)
		if err != nil {
			return err
		}
		return mcpkit.ServeHTTP(ctx, *addr, routes, log, opts...)
	default:
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}
}

// errNoScopeKey refuses an HTTP server without MCP_SCOPE_KEY: /mcp would
// otherwise read any company and any month.
var errNoScopeKey = errors.New("mcp-books: --transport http needs " + config.EnvMCPScopeKey +
	" (see .env.example); without it /mcp can't bind calls to a run")

// AdminStore is what the HTTP server needs from Postgres: the audit sink of
// both routes and the proposals the admin tool posts. *store.Store
// satisfies it.
type AdminStore interface {
	mcpkit.AuditSink
	books.ProposalStore
}

// errNoAdminStore refuses an HTTP server without Postgres: the admin tool
// reads approved proposals from it and every call writes an audit row.
var errNoAdminStore = errors.New("mcp-books: --transport http needs the database for the admin tool and the audit log")

// httpRoutes is what --transport http serves: the scoped read-only books
// tools at /mcp and post_approved_journal_entry at /mcp-admin, which is
// never scoped. The same Scoper guards /mcp at the HTTP layer (401 without
// a valid X-Run-Scope) and in every tool handler (out_of_scope). Both
// servers audit every call to st, the admin one as "books-admin". The two
// routes use different ERPNext clients (CC-1007): /mcp reads with the bot
// key (ERP_API_KEY), /mcp-admin posts with the copilot-poster key
// (ERP_POST_API_KEY), so the bot key has no write path through Close
// Copilot. It refuses to build the routes when the posting pair is missing
// or shared with the bot or the seeder (see postingClient).
func httpRoutes(cfg config.Config, companiesDir string, log *slog.Logger, st AdminStore) (map[string]mcpkit.Route, []mcpkit.Option, error) {
	if cfg.MCPScopeKey.IsZero() {
		return nil, nil, errNoScopeKey
	}
	if st == nil {
		return nil, nil, errNoAdminStore
	}
	scoper, err := mcpkit.NewScoper(cfg.MCPScopeKey.Reveal())
	if err != nil {
		return nil, nil, fmt.Errorf("mcp-books: %w", err)
	}
	poster, err := postingClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	deps, err := booksDeps(cfg, companiesDir)
	if err != nil {
		return nil, nil, err
	}
	booksServer := buildBooksServer(deps, log, scoper, st)
	adminServer := mcpkit.NewServer("close-copilot-books-admin", buildinfo.Version)
	mcpkit.EnableAudit(adminServer, "books-admin", st)
	books.RegisterAdminTools(adminServer, books.AdminDeps{
		Client:    poster,
		Companies: deps.companies,
		Proposals: st,
		Now:       time.Now,
		Logger:    log,
	})
	routes := map[string]mcpkit.Route{
		"/mcp":           mcpkit.NewAgentRoute(booksServer, cfg),
		mcpkit.AdminPath: mcpkit.NewAdminRoute(adminServer, cfg),
	}
	opts := []mcpkit.Option{
		mcpkit.WithActors(map[string]string{"/mcp": "agent", mcpkit.AdminPath: "admin"}),
		mcpkit.WithScoper(scoper, "/mcp"),
	}
	return routes, opts, nil
}

// postingClient builds the ERPNext client of /mcp-admin from the
// copilot-poster pair (ERP_POST_API_KEY, ERP_POST_API_SECRET; CC-1007). It
// fails closed when either is empty, or when the key or the secret equals
// the bot's or the seeder's, so the admin route never posts with a shared
// key. Errors name the variables, never a value.
func postingClient(cfg config.Config) (*frappe.Client, error) {
	key, secret := cfg.ERPPostAPIKey, cfg.ERPPostAPISecret
	switch {
	case key.IsZero():
		return nil, fmt.Errorf("mcp-books: --transport http needs %s for the /mcp-admin route (see .env.example; deploy/erpnext/api-users.sh writes it to tmp/erp-keys.env)", config.EnvERPPostAPIKey)
	case secret.IsZero():
		return nil, fmt.Errorf("mcp-books: --transport http needs %s for the /mcp-admin route (see .env.example; deploy/erpnext/api-users.sh writes it to tmp/erp-keys.env)", config.EnvERPPostAPISecret)
	case sameSecret(key, cfg.ERPAPIKey):
		return nil, fmt.Errorf("mcp-books: %s must not equal %s: /mcp-admin needs its own ERPNext key", config.EnvERPPostAPIKey, config.EnvERPAPIKey)
	case sameSecret(key, cfg.ERPSeedAPIKey):
		return nil, fmt.Errorf("mcp-books: %s must not equal %s: /mcp-admin needs its own ERPNext key", config.EnvERPPostAPIKey, config.EnvERPSeedAPIKey)
	case sameSecret(secret, cfg.ERPAPISecret):
		return nil, fmt.Errorf("mcp-books: %s must not equal %s: /mcp-admin needs its own ERPNext key", config.EnvERPPostAPISecret, config.EnvERPAPISecret)
	case sameSecret(secret, cfg.ERPSeedAPISecret):
		return nil, fmt.Errorf("mcp-books: %s must not equal %s: /mcp-admin needs its own ERPNext key", config.EnvERPPostAPISecret, config.EnvERPSeedAPISecret)
	}
	client, err := frappe.New(cfg, key.Reveal(), secret)
	if err != nil {
		return nil, fmt.Errorf("mcp-books: ERPNext posting client: %w", err)
	}
	return client, nil
}

// sameSecret reports whether a and b are both set and equal, in constant
// time.
func sameSecret(a, b config.Secret) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.Reveal()), []byte(b.Reveal())) == 1
}

// serverDeps are the read tools' dependencies: the company resolver and
// the ERPNext client with the bot key. The admin tool's company resolver
// comes from here too; its client does not (see postingClient).
type serverDeps struct {
	client    *frappe.Client
	companies books.CompanyResolver
}

// booksDeps loads the company profiles from companiesDir and builds the
// ERPNext client with the bot key (ERP_API_KEY, ERP_API_SECRET), never the
// seeder key.
func booksDeps(cfg config.Config, companiesDir string) (serverDeps, error) {
	profiles, err := seed.LoadProfiles(companiesDir)
	if err != nil {
		return serverDeps{}, fmt.Errorf("mcp-books: company profiles: %w", err)
	}
	client, err := frappe.New(cfg, cfg.ERPAPIKey.Reveal(), cfg.ERPAPISecret)
	if err != nil {
		return serverDeps{}, fmt.Errorf("mcp-books: ERPNext client: %w", err)
	}
	return serverDeps{client: client, companies: books.ProfileCompanies(profiles)}, nil
}

// newBooksServer builds the /mcp server with the read-only books tools
// (see booksDeps). Failed tool calls are logged to log in full; the model
// sees fixed messages. scope binds calls to their run (nil only for stdio
// and tests).
func newBooksServer(cfg config.Config, companiesDir string, log *slog.Logger, scope *mcpkit.Scoper, sinks ...mcpkit.AuditSink) (*mcp.Server, error) {
	deps, err := booksDeps(cfg, companiesDir)
	if err != nil {
		return nil, err
	}
	return buildBooksServer(deps, log, scope, sinks...), nil
}

func buildBooksServer(deps serverDeps, log *slog.Logger, scope *mcpkit.Scoper, sinks ...mcpkit.AuditSink) *mcp.Server {
	s := mcpkit.NewServer("close-copilot-books", buildinfo.Version)
	for _, sink := range sinks { // the binary passes the store; unit tests pass none
		mcpkit.EnableAudit(s, "books", sink)
	}
	books.RegisterTools(s, books.ToolDeps{
		Client:    deps.client,
		Companies: deps.companies,
		Now:       time.Now,
		Logger:    log,
		Scope:     scope,
	})
	return s
}
