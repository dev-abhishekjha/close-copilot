// Command seed is the seeder: generates the synthetic world, posts the books to ERPNext, writes bank.csv, gstr2b.json and ground truth.
//
// Built in CC-301 to CC-307. Today it has two subcommands:
//
//	seed world --company sharma --month 2026-09 [--small] [--config config/companies]
//
// prints the true world of one company-month as JSON (CC-302).
//
//	seed bootstrap --company sharma [--config config/companies] [--expect-no-changes]
//
// creates the company's master data in ERPNext with the seeder key and
// prints the report as JSON (CC-303). With --expect-no-changes it exits
// non-zero if anything was created or updated. Every other subcommand only
// checks its configuration.
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
	"strings"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

// erpEnv is what the ERPNext-writing subcommands need.
var erpEnv = []string{
	config.EnvERPBaseURL,
	config.EnvERPSite,
	config.EnvERPSeedAPIKey,
	config.EnvERPSeedAPISecret,
}

func main() {
	cli.Main("seed", requiredEnv(os.Args[1:]), newRun(os.Stdout))
}

// bootstrapFunc is seed.Bootstrap; tests replace it.
type bootstrapFunc func(ctx context.Context, c *frappe.Client, p seed.Profile) (seed.BootstrapReport, error)

// subcommand returns the first argument that isn't a flag.
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// requiredEnv lists the variables the subcommand in args needs: world is
// pure and needs none.
func requiredEnv(args []string) []string {
	if subcommand(args) == "world" {
		return nil
	}
	return erpEnv
}

func newRun(stdout io.Writer) cli.RunFunc { return newRunWith(stdout, seed.Bootstrap) }

func newRunWith(stdout io.Writer, bootstrap bootstrapFunc) cli.RunFunc {
	return func(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
		if len(args) > 0 {
			switch args[0] {
			case "world":
				return runWorld(args[1:], stdout)
			case "bootstrap":
				return runBootstrap(ctx, cfg, log, args[1:], stdout, bootstrap)
			}
		}
		log.Info("not implemented yet", "tickets", "CC-304 to CC-307", "args", args)
		return nil
	}
}

// loadCompany loads config/companies/<company>.yaml from dir.
func loadCompany(dir, company string) (seed.Profile, error) {
	if company == "" {
		return seed.Profile{}, errors.New("--company is required")
	}
	if strings.ContainsAny(company, `/\.`) {
		return seed.Profile{}, fmt.Errorf("%q is not a company id", company)
	}
	return seed.LoadProfile(filepath.Join(dir, company+".yaml"))
}

func runBootstrap(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer, bootstrap bootstrapFunc) error {
	fs := flag.NewFlagSet("seed bootstrap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	company := fs.String("company", "", "company id, such as sharma")
	dir := fs.String("config", "config/companies", "directory of company profiles")
	expectNoChanges := fs.Bool("expect-no-changes", false, "exit non-zero if anything was created or updated")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("bootstrap: unexpected arguments %q", fs.Args())
	}
	p, err := loadCompany(*dir, *company)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	c, err := frappe.New(cfg, cfg.ERPSeedAPIKey, frappe.Secret(cfg.ERPSeedAPISecret))
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	log.Info("bootstrapping", "company", p.ID, "erp_company", p.ERPCompany)
	rep, err := bootstrap(ctx, c, p)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("bootstrap: encode report: %w", err)
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("bootstrap: write: %w", err)
	}
	if n := rep.Changes(); *expectNoChanges && n > 0 {
		return fmt.Errorf("bootstrap: --expect-no-changes, but %d records were created or updated", n)
	}
	return nil
}

func runWorld(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed world", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	company := fs.String("company", "", "company id, such as sharma")
	month := fs.String("month", "", "month as YYYY-MM")
	small := fs.Bool("small", false, "scale invoice counts down for fast runs")
	dir := fs.String("config", "config/companies", "directory of company profiles")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("world: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("world: unexpected arguments %q", fs.Args())
	}
	if *company == "" || *month == "" {
		return errors.New("world: --company and --month are required")
	}
	p, err := loadCompany(*dir, *company)
	if err != nil {
		return fmt.Errorf("world: %w", err)
	}
	w, err := seed.Generate(p, *month, seed.Options{Small: *small})
	if err != nil {
		return fmt.Errorf("world: %w", err)
	}
	b, err := w.JSON()
	if err != nil {
		return fmt.Errorf("world: %w", err)
	}
	if _, err := stdout.Write(b); err != nil {
		return fmt.Errorf("world: write: %w", err)
	}
	return nil
}
