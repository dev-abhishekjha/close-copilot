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

	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
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

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("mcp-books: parse flags: %w", err)
	}

	version := buildinfo.Version
	booksServer := mcpkit.NewServer("close-copilot-books", version)

	switch *transport {
	case "stdio":
		log.Info("starting mcp-books in stdio mode")
		return mcpkit.RunStdio(ctx, booksServer)
	case "http":
		adminServer := mcpkit.NewServer("close-copilot-books-admin", version)
		routes := map[string]mcpkit.Route{
			"/mcp":       mcpkit.NewAgentRoute(booksServer, cfg),
			"/mcp-admin": mcpkit.NewAdminRoute(adminServer, cfg),
		}
		return mcpkit.ServeHTTP(ctx, *addr, routes, log)
	default:
		return fmt.Errorf("mcp-books: unknown transport %q (want http or stdio)", *transport)
	}
}
