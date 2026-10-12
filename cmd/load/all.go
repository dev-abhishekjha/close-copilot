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

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// monthLoader loads one evidence file of one company-month and returns its
// summary.
type monthLoader func(ctx context.Context, company, month, path string) (any, error)

// allSummary is one line of `load all` output: a company-month's two loads.
type allSummary struct {
	Company string `json:"company"`
	Month   string `json:"month"`
	Bank    any    `json:"bank"`
	GSTR2B  any    `json:"gstr2b"`
}

// allTotal is the final line of `load all` output.
type allTotal struct {
	Suite       string `json:"suite"`
	CompanyMths int    `json:"company_months"`
}

func runAll(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("load all", flag.ContinueOnError)
	fs.SetOutput(stdout)
	suiteName := fs.String("suite", "", "suite name, such as suite-v1")
	dataDir := fs.String("data-dir", "", "root directory of external data")
	suitesDir := fs.String("suites-dir", "evals/scenarios", "directory of suite files")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("load all: %w", err)
	}
	if *suiteName == "" {
		return errors.New("load all: --suite is required")
	}
	path, err := evals.SuitePath(*suitesDir, *suiteName)
	if err != nil {
		return fmt.Errorf("load all: %w", err)
	}
	suite, err := evals.LoadSuite(path)
	if err != nil {
		return fmt.Errorf("load all: %w", err)
	}

	root := *dataDir
	if root == "" {
		root = cfg.DataDir
	}
	if root == "" {
		root = "data/external"
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("load all: connect database: %w", err)
	}
	defer st.Close()

	// Seed companies from profiles on startup so foreign keys resolve
	_ = st.SeedCompaniesFromDir(ctx, "config/companies")

	bank := func(ctx context.Context, company, month, p string) (any, error) {
		res, err := evidence.LoadBankFile(ctx, st, company, month, p)
		if res.Warning != "" {
			log.Warn(res.Warning, "company", company, "month", month)
		}
		return res, err
	}
	gstr := func(ctx context.Context, company, month, p string) (any, error) {
		res, err := evidence.LoadGSTR2BFile(ctx, st, company, month, p)
		if res.Warning != "" {
			log.Warn(res.Warning, "company", company, "month", month)
		}
		return res, err
	}
	return loadSuite(ctx, suite, root, bank, gstr, stdout)
}

// loadSuite loads every seeded company-month of suite, in month order per
// company, printing one JSON summary per company-month and a total. A missing
// data file stops the run with an error naming the company and month, after
// the months already loaded are printed.
func loadSuite(ctx context.Context, suite evals.Suite, root string, bank, gstr monthLoader, stdout io.Writer) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	n := 0
	for _, company := range suite.Companies() {
		for _, month := range suite.SeededMonths(company) {
			dir := filepath.Join(root, company, month)
			bankPath := filepath.Join(dir, "bank.csv")
			gstrPath := filepath.Join(dir, "gstr2b.json")
			for _, p := range []string{bankPath, gstrPath} {
				if _, err := os.Stat(p); err != nil {
					return fmt.Errorf("load all: %s %s: %w", company, month, err)
				}
			}
			b, err := bank(ctx, company, month, bankPath)
			if err != nil {
				return fmt.Errorf("load all: %s %s: bank: %w", company, month, err)
			}
			g, err := gstr(ctx, company, month, gstrPath)
			if err != nil {
				return fmt.Errorf("load all: %s %s: gstr2b: %w", company, month, err)
			}
			if err := enc.Encode(allSummary{Company: company, Month: month, Bank: b, GSTR2B: g}); err != nil {
				return fmt.Errorf("load all: encode summary: %w", err)
			}
			n++
		}
	}
	return enc.Encode(allTotal{Suite: suite.Name, CompanyMths: n})
}
