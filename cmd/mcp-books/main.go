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
)

func main() {
	cli.Main("mcp-books", []string{
		config.EnvERPBaseURL,
		config.EnvERPSite,
		config.EnvERPAPIKey,
		config.EnvERPAPISecret,
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

	if *transport != "http" && *transport != "stdio" {
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}

	booksServer, err := newBooksServer(cfg, *companies, log)
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
		return mcpkit.ServeHTTP(ctx, *addr, routes, log)
	default:
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}
}

// newBooksServer builds the /mcp server with the read-only books tools: it
// loads the company profiles from companiesDir and an ERPNext client with
// the bot key (ERP_API_KEY, ERP_API_SECRET), never the seeder key. Failed
// tool calls are logged to log in full; the model sees fixed messages.
func newBooksServer(cfg config.Config, companiesDir string, log *slog.Logger) (*mcp.Server, error) {
	profiles, err := seed.LoadProfiles(companiesDir)
	if err != nil {
		return nil, fmt.Errorf("mcp-books: company profiles: %w", err)
	}
	client, err := frappe.New(cfg, cfg.ERPAPIKey.Reveal(), cfg.ERPAPISecret)
	if err != nil {
		return nil, fmt.Errorf("mcp-books: ERPNext client: %w", err)
	}
	s := mcpkit.NewServer("close-copilot-books", buildinfo.Version)
	books.RegisterTools(s, books.ToolDeps{
		Client:    client,
		Companies: books.ProfileCompanies(profiles),
		Now:       time.Now,
		Logger:    log,
	})
	return s, nil
}
