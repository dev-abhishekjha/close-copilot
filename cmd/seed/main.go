// Command seed is the seeder: generates the synthetic world, posts the books to ERPNext, writes bank.csv, gstr2b.json and ground truth.
//
// Built in CC-301 to CC-307; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("seed", []string{
		config.EnvERPBaseURL,
		config.EnvERPSite,
		config.EnvERPSeedAPIKey,
		config.EnvERPSeedAPISecret,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-301 to CC-307", "args", args)
	return nil
}
