// Command probe checks the ERPNext API credentials from CC-201 and dumps the
// DocType schemas committed under docs/erpnext-schema/.
//
// Usage:
//
//	probe auth                 the bot key logs in as copilot-bot@example.com
//	probe perms                the bot's read, Journal Entry and refusal checks
//	probe schema --out <dir>   write one <kebab-name>.json per DocType
//
// Every request goes through internal/httpx with
// "Authorization: token <key>:<secret>", "Accept: application/json" and the
// ERP_SITE as the Host header. No command prints a secret.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

const usage = "usage: probe auth | probe perms | probe schema --out <dir>"

func main() {
	cli.Main("probe", []string{
		config.EnvERPBaseURL,
		config.EnvERPSite,
		config.EnvERPAPIKey,
		config.EnvERPAPISecret,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	return dispatch(ctx, cfg, log, args, os.Stdout)
}

// dispatch runs one subcommand, writing its report to stdout.
func dispatch(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("probe: no subcommand; %s", usage)
	}
	bot, err := newClient(cfg, cfg.ERPAPIKey, cfg.ERPAPISecret)
	if err != nil {
		return err
	}
	switch args[0] {
	case "auth":
		if len(args) > 1 {
			return fmt.Errorf("probe auth: unexpected arguments %q; %s", args[1:], usage)
		}
		return runAuth(ctx, bot, stdout)
	case "perms":
		if len(args) > 1 {
			return fmt.Errorf("probe perms: unexpected arguments %q; %s", args[1:], usage)
		}
		return runPerms(ctx, bot, stdout)
	case "schema":
		return runSchema(ctx, cfg, log, bot, args[1:], stdout)
	default:
		return fmt.Errorf("probe: unknown subcommand %q; %s", args[0], usage)
	}
}
