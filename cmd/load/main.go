// Command load is the loader: reads bank statements and GSTR-2B files into Postgres.
//
// Built in CC-402, CC-403; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("load", []string{
		config.EnvDatabaseURL,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-402, CC-403", "args", args)
	return nil
}
