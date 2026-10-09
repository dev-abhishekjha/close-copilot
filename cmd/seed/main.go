// Command seed is the seeder: generates the synthetic world, posts the books to ERPNext, writes bank.csv, gstr2b.json and ground truth.
//
// Built in CC-301 to CC-307. Today it has one subcommand:
//
//	seed world --company sharma --month 2026-09 [--small] [--config config/companies]
//
// prints the true world of one company-month as JSON (CC-302). Every other
// subcommand only checks its configuration.
package main

import (
	"context"
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

func newRun(stdout io.Writer) cli.RunFunc {
	return func(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
		if len(args) > 0 && args[0] == "world" {
			return runWorld(args[1:], stdout)
		}
		log.Info("not implemented yet", "tickets", "CC-303 to CC-307", "args", args)
		return nil
	}
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
	if strings.ContainsAny(*company, `/\.`) {
		return fmt.Errorf("world: %q is not a company id", *company)
	}
	p, err := seed.LoadProfile(filepath.Join(*dir, *company+".yaml"))
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
