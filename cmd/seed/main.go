// Command seed is the seeder: generates the synthetic world, posts the books to ERPNext, writes bank.csv, gstr2b.json and ground truth.
//
// Built in CC-301 to CC-307. It has six subcommands:
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
//
//	seed plant [--company sharma --month 2026-09] [--suite suite-skeleton] [--small]
//	           [--config config/companies] [--scenarios evals/scenarios] [--out evals]
//
// applies planted errors to the world and writes ground truth JSON (CC-306).
//
//	seed all [--suite suite-skeleton] [--small] [--config config/companies]
//	         [--scenarios evals/scenarios] [--out <dir>] [--expect-no-changes]
//
// rebuilds everything for all companies and months defined in the suite:
// bootstraps masters, posts books to ERPNext, writes bank.csv and gstr2b.json,
// and emits ground truth (CC-307).
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
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

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

// requiredEnv lists the variables the subcommand in args needs: world, evidence,
// and plant are pure and need none.
func requiredEnv(args []string) []string {
	switch subcommand(args) {
	case "world", "evidence", "plant":
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
			case "plant":
				return runPlant(args[1:], stdout)
			case "all":
				return runAll(ctx, cfg, log, args[1:], stdout, d)
			}
		}
		log.Info("unknown subcommand", "args", args)
		return errors.New("seed: unknown subcommand; available: world, bootstrap, books, evidence, plant, all")
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

// loadScenarioConfig reads a scenario YAML file.
func loadScenarioConfig(path string) (seed.ScenarioConfig, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the scenario file
	if err != nil {
		return seed.ScenarioConfig{}, fmt.Errorf("read scenario config: %w", err)
	}
	var cfg seed.ScenarioConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return seed.ScenarioConfig{}, fmt.Errorf("parse scenario %s: %w", path, err)
	}
	return cfg, nil
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
		return fmt.Errorf("evidence: write summary: %w", err)
	}
	return nil
}

type plantSummary struct {
	Suite       string   `json:"suite"`
	Files       []string `json:"files"`
	ErrorsCount int      `json:"errors_count"`
}

func runPlant(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed plant", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	company := fs.String("company", "", "company id, such as sharma")
	month := fs.String("month", "", "month as YYYY-MM")
	suite := fs.String("suite", "suite-skeleton", "eval suite scenario name")
	small := fs.Bool("small", false, "scale invoice counts down for fast runs")
	configDir := fs.String("config", "config/companies", "directory of company profiles")
	scenariosDir := fs.String("scenarios", "evals/scenarios", "directory of suite scenario definitions")
	out := fs.String("out", "evals", "output root directory for ground truth")
	mapsDir := fs.String("maps", "data/out", "root directory of erp maps")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("plant: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("plant: unexpected arguments %q", fs.Args())
	}

	scenarioPath := filepath.Join(*scenariosDir, *suite+".yaml")
	suiteCfg, err := loadScenarioConfig(scenarioPath)
	if err != nil {
		if *suite == "suite-skeleton" {
			suiteCfg = seed.DefaultSkeletonConfig()
		} else {
			return fmt.Errorf("plant: %w", err)
		}
	}
	isSmall := *small || suiteCfg.Small

	type targetCM struct {
		company, month string
	}
	var targets []targetCM
	if *company != "" && *month != "" {
		targets = append(targets, targetCM{*company, *month})
	} else {
		if suiteCfg.CleanControl.Company != "" && suiteCfg.CleanControl.Month != "" {
			targets = append(targets, targetCM{suiteCfg.CleanControl.Company, suiteCfg.CleanControl.Month})
		}
		for _, ev := range suiteCfg.Evaluated {
			for _, m := range ev.Months {
				targets = append(targets, targetCM{ev.Company, m})
			}
		}
	}

	if len(targets) == 0 {
		return errors.New("plant: no company-month targets found")
	}

	var writtenFiles []string
	totalErrors := 0

	for _, tgt := range targets {
		p, err := loadCompany(*configDir, tgt.company)
		if err != nil {
			return fmt.Errorf("plant load company: %w", err)
		}
		start, err := seed.ParseMonth(tgt.month)
		if err != nil {
			return fmt.Errorf("plant parse month: %w", err)
		}
		prevMonth := start.AddDate(0, -1, 0).Format("2006-01")
		prev, err := seed.Generate(p, prevMonth, seed.Options{Small: isSmall})
		if err != nil {
			return fmt.Errorf("plant prev world: %w", err)
		}
		w, err := seed.Generate(p, tgt.month, seed.Options{Small: isSmall})
		if err != nil {
			return fmt.Errorf("plant world: %w", err)
		}
		pw, err := seed.PlantErrors(w, suiteCfg)
		if err != nil {
			return fmt.Errorf("plant errors: %w", err)
		}
		lines, err := seed.BankLines(pw.BankWorld)
		if err != nil {
			return fmt.Errorf("plant bank lines: %w", err)
		}
		g2b, err := seed.BuildGSTR2B(p, tgt.month, []seed.World{prev, pw.BankWorld}, seed.DefaultGSTR2BOptions())
		if err != nil {
			return fmt.Errorf("plant gstr2b: %w", err)
		}

		var erpMap seed.ERPMap
		mapPath := filepath.Join(*mapsDir, *suite, tgt.company+"-"+tgt.month, seed.ERPMapFile)
		if m, err := seed.LoadERPMap(mapPath); err == nil {
			erpMap = m
		}

		gt, err := seed.BuildGroundTruth(seed.GroundTruthOptions{
			PlantedWorld: pw,
			BankLines:    lines,
			ERPMap:       erpMap,
			GSTR2B:       &g2b,
		})
		if err != nil {
			return fmt.Errorf("plant build ground truth: %w", err)
		}
		fPath, err := seed.WriteGroundTruth(*out, gt)
		if err != nil {
			return fmt.Errorf("plant write ground truth: %w", err)
		}
		writtenFiles = append(writtenFiles, fPath)
		totalErrors += len(gt.Planted)
	}

	b, err := json.Marshal(plantSummary{
		Suite:       *suite,
		Files:       writtenFiles,
		ErrorsCount: totalErrors,
	})
	if err != nil {
		return fmt.Errorf("plant summary marshal: %w", err)
	}
	_, err = stdout.Write(append(b, '\n'))
	return err
}

type allSummary struct {
	Suite       string   `json:"suite"`
	Companies   []string `json:"companies"`
	MonthsCount int      `json:"months_count"`
	Small       bool     `json:"small"`
	Status      string   `json:"status"`
}

func runAll(ctx context.Context, cfg config.Config, log *slog.Logger, args []string, stdout io.Writer, d deps) error {
	fs := flag.NewFlagSet("seed all", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	suite := fs.String("suite", "suite-skeleton", "eval suite scenario name")
	small := fs.Bool("small", false, "scale invoice counts down for fast runs")
	configDir := fs.String("config", "config/companies", "directory of company profiles")
	scenariosDir := fs.String("scenarios", "evals/scenarios", "directory of suite scenario definitions")
	out := fs.String("out", "", "output root directory")
	expectNoChanges := fs.Bool("expect-no-changes", false, "exit non-zero if any document was created or submitted")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("all: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("all: unexpected arguments %q", fs.Args())
	}

	scenarioPath := filepath.Join(*scenariosDir, *suite+".yaml")
	suiteCfg, err := loadScenarioConfig(scenarioPath)
	if err != nil {
		if *suite == "suite-skeleton" {
			suiteCfg = seed.DefaultSkeletonConfig()
		} else {
			return fmt.Errorf("all load scenario: %w", err)
		}
	}
	isSmall := *small || suiteCfg.Small

	mapsRoot := "data/out"
	externalRoot := "data/external"
	evalsRoot := "evals"
	if *out != "" && *out != "data" {
		mapsRoot = filepath.Join(*out, "out")
		externalRoot = filepath.Join(*out, "external")
		evalsRoot = filepath.Join(*out, "evals")
	}

	var companies []string
	companySet := make(map[string]bool)
	addCompany := func(c string) {
		if c != "" && !companySet[c] {
			companySet[c] = true
			companies = append(companies, c)
		}
	}
	if suiteCfg.CleanControl.Company != "" {
		addCompany(suiteCfg.CleanControl.Company)
	}
	for _, ev := range suiteCfg.Evaluated {
		addCompany(ev.Company)
	}

	c, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), frappe.Secret(cfg.ERPSeedAPISecret.Reveal()))
	if err != nil {
		return fmt.Errorf("all frappe client: %w", err)
	}

	bootReports := make(map[string]seed.BootstrapReport)
	totalChanges := 0
	for _, comp := range companies {
		p, err := loadCompany(*configDir, comp)
		if err != nil {
			return fmt.Errorf("all load company %s: %w", comp, err)
		}
		log.Info("bootstrapping", "company", p.ID, "erp_company", p.ERPCompany)
		rep, err := d.bootstrap(ctx, c, p)
		if err != nil {
			return fmt.Errorf("all bootstrap %s: %w", comp, err)
		}
		totalChanges += rep.Changes()
		bootReports[comp] = rep
	}

	monthsProcessed := 0
	for _, comp := range companies {
		p, err := loadCompany(*configDir, comp)
		if err != nil {
			return fmt.Errorf("all load company %s: %w", comp, err)
		}
		rep := bootReports[comp]

		monthSet := make(map[string]bool)
		var months []string
		addMonth := func(m string) {
			if m != "" && !monthSet[m] {
				monthSet[m] = true
				months = append(months, m)
			}
		}

		for _, m := range suiteCfg.HistoryMonths {
			addMonth(m)
		}
		if suiteCfg.CleanControl.Company == comp {
			addMonth(suiteCfg.CleanControl.Month)
		}
		for _, ev := range suiteCfg.Evaluated {
			if ev.Company == comp {
				for _, m := range ev.Months {
					addMonth(m)
				}
			}
		}
		slices.Sort(months)

		for _, month := range months {
			log.Info("processing month", "company", comp, "month", month, "suite", *suite)
			start, err := seed.ParseMonth(month)
			if err != nil {
				return fmt.Errorf("all parse month %s: %w", month, err)
			}
			prevMonth := start.AddDate(0, -1, 0).Format("2006-01")
			prev, err := seed.Generate(p, prevMonth, seed.Options{Small: isSmall})
			if err != nil {
				return fmt.Errorf("all generate prev %s: %w", prevMonth, err)
			}
			w, err := seed.Generate(p, month, seed.Options{Small: isSmall})
			if err != nil {
				return fmt.Errorf("all generate world %s: %w", month, err)
			}

			pw, err := seed.PlantErrors(w, suiteCfg)
			if err != nil {
				return fmt.Errorf("all plant errors %s %s: %w", comp, month, err)
			}

			mapDir, err := seed.ERPMapDir(mapsRoot, suiteCfg.Suite, comp, month)
			if err != nil {
				return fmt.Errorf("all map dir %s %s: %w", comp, month, err)
			}
			res, err := d.books(ctx, c, rep, pw.BooksWorld, seed.BooksOptions{Suppliers: p.Suppliers, Log: log, Workers: 1})
			if err != nil {
				return fmt.Errorf("all books %s %s: %w", comp, month, err)
			}
			totalChanges += res.Changes()
			if err := seed.WriteERPMap(mapDir, res.Map); err != nil {
				return fmt.Errorf("all write erp map %s %s: %w", comp, month, err)
			}

			lines, err := seed.BankLines(pw.BankWorld)
			if err != nil {
				return fmt.Errorf("all bank lines %s %s: %w", comp, month, err)
			}
			g2b, err := seed.BuildGSTR2B(p, month, []seed.World{prev, pw.BankWorld}, seed.DefaultGSTR2BOptions())
			if err != nil {
				return fmt.Errorf("all gstr2b %s %s: %w", comp, month, err)
			}
			monthDir := filepath.Join(externalRoot, comp, month)
			if err := seed.WriteBankCSV(filepath.Join(monthDir, seed.BankCSVFile), lines); err != nil {
				return fmt.Errorf("all write bank csv %s %s: %w", comp, month, err)
			}
			if err := seed.WriteGSTR2B(filepath.Join(monthDir, seed.GSTR2BFile), g2b); err != nil {
				return fmt.Errorf("all write gstr2b %s %s: %w", comp, month, err)
			}

			gt, err := seed.BuildGroundTruth(seed.GroundTruthOptions{
				PlantedWorld: pw,
				BankLines:    lines,
				ERPMap:       res.Map,
				GSTR2B:       &g2b,
			})
			if err != nil {
				return fmt.Errorf("all build ground truth %s %s: %w", comp, month, err)
			}
			if _, err := seed.WriteGroundTruth(evalsRoot, gt); err != nil {
				return fmt.Errorf("all write ground truth %s %s: %w", comp, month, err)
			}
			monthsProcessed++
		}
	}

	if *expectNoChanges && totalChanges > 0 {
		return fmt.Errorf("all: --expect-no-changes, but %d documents/records were created or updated", totalChanges)
	}

	b, err := json.Marshal(allSummary{
		Suite:       *suite,
		Companies:   companies,
		MonthsCount: monthsProcessed,
		Small:       isSmall,
		Status:      "ok",
	})
	if err != nil {
		return fmt.Errorf("all summary marshal: %w", err)
	}
	_, err = stdout.Write(append(b, '\n'))
	return err
}
