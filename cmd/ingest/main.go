// Command ingest converts the document corpus with docling, chunks it, embeds it with TEI and stores it in Postgres.
//
// Built in CC-803; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("ingest", []string{
		config.EnvDatabaseURL,
		config.EnvTEIEmbedURL,
		config.EnvDoclingURL,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-803", "args", args)
	return nil
}
