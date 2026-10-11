// Command mcp-books is the Books MCP server: read-only ERPNext tools at /mcp and the approved-write tool at /mcp-admin.
//
// Built in CC-501, CC-502, CC-504b.
package main

import (
	"context"
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
	transport := fs.String("transport", "http", "transport: http or stdio")
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

	// Fail on bad company profiles before touching the database.
	if _, err := seed.LoadProfiles(*companies); err != nil {
		return fmt.Errorf("mcp-books: company profiles: %w", err)
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("mcp-books: connect to database: %w", err)
	}
	defer st.Close()

	booksServer, err := newBooksServer(cfg, *companies, log, st)
	if err != nil {
		return err
	}

	switch *transport {
	case "stdio":
		log.Info("starting mcp-books in stdio mode")
		return mcpkit.RunStdio(ctx, booksServer)
	case "http":
		adminServer := mcpkit.NewServer("close-copilot-books-admin", buildinfo.Version)
		routes := map[string]mcpkit.Route{
			"/mcp":       mcpkit.NewAgentRoute(booksServer, cfg),
			"/mcp-admin": mcpkit.NewAdminRoute(adminServer, cfg),
		}
		return mcpkit.ServeHTTP(ctx, *addr, routes, log, mcpkit.WithActors(map[string]string{
			"/mcp":       "agent",
			"/mcp-admin": "admin",
		}))
	default:
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}
}

// newBooksServer builds the /mcp server with the read-only books tools: it
// loads the company profiles from companiesDir and an ERPNext client with
// the bot key (ERP_API_KEY, ERP_API_SECRET), never the seeder key. Failed
// tool calls are logged to log in full; the model sees fixed messages.
func newBooksServer(cfg config.Config, companiesDir string, log *slog.Logger, sinks ...mcpkit.AuditSink) (*mcp.Server, error) {
	profiles, err := seed.LoadProfiles(companiesDir)
	if err != nil {
		return nil, fmt.Errorf("mcp-books: company profiles: %w", err)
	}
	client, err := frappe.New(cfg, cfg.ERPAPIKey.Reveal(), cfg.ERPAPISecret)
	if err != nil {
		return nil, fmt.Errorf("mcp-books: ERPNext client: %w", err)
	}
	s := mcpkit.NewServer("close-copilot-books", buildinfo.Version)
	for _, sink := range sinks { // the binary passes the store; unit tests pass none
		mcpkit.EnableAudit(s, "books", sink)
	}
	books.RegisterTools(s, books.ToolDeps{
		Client:    client,
		Companies: books.ProfileCompanies(profiles),
		Now:       time.Now,
		Logger:    log,
	})
	return s, nil
}
