// Command seed is the seeder: generates the synthetic world, posts the books to ERPNext, writes bank.csv, gstr2b.json and ground truth.
//
// Built in CC-301 to CC-307. Today it has four subcommands:
//
//	seed world --company sharma --month 2026-09 [--small] [--config config/companies]
//
// prints the true world of one company-month as JSON (CC-302).
//
//	seed bootstrap --company sharma [--config config/companies] [--expect-no-changes]
//
// creates the company's master data in ERPNext with the seeder key and
// prints the report as JSON (CC-303). With --expect-no-changes it exits
// non-zero if anything was created or updated.
//
//	seed books --company sharma --month 2026-09 [--small] [--suite suite-skeleton]
//	           [--config config/companies] [--out data/out] [--expect-no-changes]
//
// posts the month's book-side events to ERPNext as submitted documents
// (CC-304). It runs Bootstrap first, which must report zero changes, then
// generates the world, posts it, prints the result as JSON and writes
// <out>/<suite>/<company>-<month>/erp_map.json. With --expect-no-changes it
// exits non-zero if any document was created or submitted.
//
//	seed evidence --company sharma --month 2026-09 [--small]
//	              [--config config/companies] [--out data/external]
//
// writes the month's bank statement and GSTR-2B, derived from the true
// world and never from the books, to <out>/<company>/<month>/bank.csv and
// gstr2b.json (CC-305), and prints a one-line JSON summary. It generates
// the previous month's world too, for the invoices filed late. Like world,
// it needs no ERPNext variables. Every other subcommand only checks its
// configuration.
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

// booksFunc is seed.WriteBooks; tests replace it.
type booksFunc func(ctx context.Context, c *frappe.Client, rep seed.BootstrapReport, w seed.World, opt seed.BooksOptions) (seed.BooksResult, error)

// deps are the ERPNext-writing steps the subcommands call.
type deps struct {
	bootstrap bootstrapFunc
	books     booksFunc
}

// subcommand returns the first argument that isn't a flag.
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// requiredEnv lists the variables the subcommand in args needs: world and
// evidence are pure and need none.
func requiredEnv(args []string) []string {
	switch subcommand(args) {
	case "world", "evidence":
		return nil
	}
	return erpEnv
}

func newRun(stdout io.Writer) cli.RunFunc {
	return newRunDeps(stdout, deps{bootstrap: seed.Bootstrap, books: seed.WriteBooks})
}

func newRunWith(stdout io.Writer, bootstrap bootstrapFunc) cli.RunFunc {
	return newRunDeps(stdout, deps{bootstrap: bootstrap, books: seed.WriteBooks})
}

func newRunDeps(stdout io.Writer, d deps) cli.RunFunc {
	return func(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
		if len(args) > 0 {
			switch args[0] {
			case "world":
				return runWorld(args[1:], stdout)
			case "bootstrap":
				return runBootstrap(ctx, cfg, log, args[1:], stdout, d.bootstrap)
			case "books":
				return runBooks(ctx, cfg, log, args[1:], stdout, d)
			case "evidence":
				return runEvidence(args[1:], stdout)
			}
		}
		log.Info("not implemented yet", "tickets", "CC-306 to CC-307", "args", args)
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
	c, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), frappe.Secret(cfg.ERPSeedAPISecret.Reveal()))
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

func runBooks(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer, d deps) error {
	fs := flag.NewFlagSet("seed books", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	company := fs.String("company", "", "company id, such as sharma")
	month := fs.String("month", "", "month as YYYY-MM")
	small := fs.Bool("small", false, "scale invoice counts down for fast runs")
	suite := fs.String("suite", "suite-skeleton", "eval suite the outputs belong to")
	dir := fs.String("config", "config/companies", "directory of company profiles")
	out := fs.String("out", "data/out", "output root")
	expectNoChanges := fs.Bool("expect-no-changes", false, "exit non-zero if any document was created or submitted")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("books: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("books: unexpected arguments %q", fs.Args())
	}
	if *company == "" || *month == "" {
		return errors.New("books: --company and --month are required")
	}
	p, err := loadCompany(*dir, *company)
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	mapDir, err := seed.ERPMapDir(*out, *suite, p.ID, *month)
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	w, err := seed.Generate(p, *month, seed.Options{Small: *small})
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	c, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), frappe.Secret(cfg.ERPSeedAPISecret.Reveal()))
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	rep, err := d.bootstrap(ctx, c, p)
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	if n := rep.Changes(); n > 0 {
		return fmt.Errorf("books: bootstrap created or updated %d records; run seed bootstrap first", n)
	}
	log.Info("writing books", "company", p.ID, "month", *month, "small", *small, "events", len(w.Events))
	res, err := d.books(ctx, c, rep, w, seed.BooksOptions{Suppliers: p.Suppliers, Log: log})
	if err != nil {
		return fmt.Errorf("books: %w", err)
	}
	if err := seed.WriteERPMap(mapDir, res.Map); err != nil {
		return fmt.Errorf("books: %w", err)
	}
	log.Info("wrote erp map", "path", filepath.Join(mapDir, seed.ERPMapFile), "entries", len(res.Map))
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("books: encode result: %w", err)
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("books: write: %w", err)
	}
	if n := res.Changes(); *expectNoChanges && n > 0 {
		return fmt.Errorf("books: --expect-no-changes, but %d documents were created or submitted", n)
	}
	return nil
}

// evidenceSummary is the line seed evidence prints. Amounts are rupees.
type evidenceSummary struct {
	Company   string      `json:"company"`
	Month     string      `json:"month"`
	BankLines int         `json:"bank_lines"`
	Opening   json.Number `json:"opening"`
	Closing   json.Number `json:"closing"`
	Invoices  int         `json:"invoices"`
	Late      int         `json:"late"`
	Deferred  int         `json:"deferred"`
}

func runEvidence(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed evidence", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	company := fs.String("company", "", "company id, such as sharma")
	month := fs.String("month", "", "month as YYYY-MM")
	small := fs.Bool("small", false, "scale invoice counts down for fast runs")
	dir := fs.String("config", "config/companies", "directory of company profiles")
	out := fs.String("out", "data/external", "output root")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("evidence: unexpected arguments %q", fs.Args())
	}
	if *company == "" || *month == "" {
		return errors.New("evidence: --company and --month are required")
	}
	start, err := seed.ParseMonth(*month)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	p, err := loadCompany(*dir, *company)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	opt := seed.Options{Small: *small}
	prev, err := seed.Generate(p, start.AddDate(0, -1, 0).Format("2006-01"), opt)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	w, err := seed.Generate(p, *month, opt)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	lines, err := seed.BankLines(w)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	g, err := seed.BuildGSTR2B(p, *month, []seed.World{prev, w}, seed.DefaultGSTR2BOptions())
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	monthDir := filepath.Join(*out, p.ID, *month)
	if err := seed.WriteBankCSV(filepath.Join(monthDir, seed.BankCSVFile), lines); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if err := seed.WriteGSTR2B(filepath.Join(monthDir, seed.GSTR2BFile), g); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	b, err := json.Marshal(evidenceSummary{
		Company:   p.ID,
		Month:     *month,
		BankLines: len(lines),
		Opening:   json.Number(w.OpeningBank.Rupees()),
		Closing:   json.Number(w.ClosingBank.Rupees()),
		Invoices:  len(g.Included),
		Late:      len(g.Late),
		Deferred:  len(g.Deferred),
	})
	if err != nil {
		return fmt.Errorf("evidence: encode summary: %w", err)
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("evidence: write: %w", err)
	}
	return nil
}
