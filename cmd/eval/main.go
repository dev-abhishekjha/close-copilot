// Command eval is the eval harness: runs closes on a seeded suite and scores them against ground truth.
//
// Built in CC-901, CC-902, CC-905; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("eval", []string{
		config.EnvDatabaseURL,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-901, CC-902, CC-905", "args", args)
	return nil
}
