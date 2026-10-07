// Command mcp-books is the Books MCP server: read-only ERPNext tools at /mcp and the approved-write tool at /mcp-admin.
//
// Built in CC-501, CC-502, CC-504b; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
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
	log.Info("not implemented yet", "tickets", "CC-501, CC-502, CC-504b", "args", args)
	return nil
}
