// Command probe checks the ERPNext API keys and dumps DocType schemas to docs/erpnext-schema/.
//
// Built in CC-201; until then it only checks its configuration.
package main

import (
	"context"
	"log/slog"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

func main() {
	cli.Main("probe", []string{
		config.EnvERPBaseURL,
		config.EnvERPSite,
		config.EnvERPAPIKey,
		config.EnvERPAPISecret,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	log.Info("not implemented yet", "tickets", "CC-201", "args", args)
	return nil
}
