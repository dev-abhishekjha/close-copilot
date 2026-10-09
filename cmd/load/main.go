// Command load is the loader: reads bank statements and GSTR-2B files into Postgres.
//
// Built in CC-402, CC-403.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func main() {
	cli.Main("load", []string{config.EnvDatabaseURL}, newRun(os.Stdout))
}

func newRun(stdout io.Writer) cli.RunFunc {
	return func(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
		if len(args) == 0 {
			return errors.New("load: subcommand required (available: bank)")
		}

		switch args[0] {
		case "bank":
			return runBank(ctx, cfg, log, args[1:], stdout)
		default:
			return fmt.Errorf("load: unknown subcommand %q (available: bank)", args[0])
		}
	}
}

func runBank(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("load bank", flag.ContinueOnError)
	fs.SetOutput(stdout)
	company := fs.String("company", "", "company id, such as sharma")
	month := fs.String("month", "", "month as YYYY-MM")
	file := fs.String("file", "", "path to bank.csv (default data/external/<company>/<month>/bank.csv)")
	dataDir := fs.String("data-dir", "", "root directory of external data")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("load bank: %w", err)
	}

	if *company == "" || *month == "" {
		return errors.New("load bank: --company and --month are required")
	}

	if _, err := time.Parse("2006-01", *month); err != nil {
		return fmt.Errorf("load bank: invalid --month %q (want YYYY-MM): %w", *month, err)
	}

	csvPath := *file
	if csvPath == "" {
		root := *dataDir
		if root == "" {
			root = cfg.DataDir
		}
		if root == "" {
			root = "data/external"
		}
		csvPath = filepath.Join(root, *company, *month, "bank.csv")
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("load bank: connect database: %w", err)
	}
	defer st.Close()

	// Seed companies from profiles on startup so foreign keys resolve
	_ = st.SeedCompaniesFromDir(ctx, "config/companies")

	res, err := evidence.LoadBankFile(ctx, st, *company, *month, csvPath)
	if err != nil {
		return fmt.Errorf("load bank: %w", err)
	}

	if res.Warning != "" {
		log.Warn(res.Warning, "company", *company, "month", *month)
	}
	log.Info("loaded bank statement",
		"company", res.Company,
		"month", res.Month,
		"total", res.Total,
		"inserted", res.Inserted,
		"updated", res.Updated,
		"source", res.SourceFile,
	)

	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("load bank: encode summary: %w", err)
	}
	_, err = stdout.Write(append(b, '\n'))
	return err
}
