// Command mcp-evidence is the Evidence MCP server: bank lines, GSTR-2B entries and document search from Postgres.
//
// Built in CC-501, CC-503, CC-805; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("mcp-evidence", []string{
		config.EnvDatabaseURL,
		config.EnvMCPTokenAgent,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-501, CC-503, CC-805", "args", args)
	return nil
}
