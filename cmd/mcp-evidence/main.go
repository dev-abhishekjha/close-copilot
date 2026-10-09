// Command mcp-evidence is the Evidence MCP server: bank lines, GSTR-2B entries and document search from Postgres.
//
// Built in CC-501, CC-503, CC-805.
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
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func main() {
	cli.Main("mcp-evidence", []string{
		config.EnvDatabaseURL,
		config.EnvMCPTokenAgent,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("mcp-evidence", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	transport := fs.String("transport", "http", "transport: http or stdio")
	addr := fs.String("addr", ":7002", "listen address for HTTP transport")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("mcp-evidence: parse flags: %w", err)
	}

	if *transport != "http" && *transport != "stdio" {
		return fmt.Errorf("mcp-evidence: unknown transport %q (want http or stdio)", *transport)
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("mcp-evidence: connect to database: %w", err)
	}
	defer st.Close()

	version := buildinfo.Version
	evidenceServer := mcpkit.NewServer("close-copilot-evidence", version)
	evidence.RegisterTools(evidenceServer, st)

	switch *transport {
	case "stdio":
		log.Info("starting mcp-evidence in stdio mode")
		return mcpkit.RunStdio(ctx, evidenceServer)
	case "http":
		routes := map[string]mcpkit.Route{
			"/mcp": mcpkit.NewAgentRoute(evidenceServer, cfg),
		}
		return mcpkit.ServeHTTP(ctx, *addr, routes, log)
	default:
		return fmt.Errorf("mcp-evidence: unknown transport %q (want http or stdio)", *transport)
	}
}
