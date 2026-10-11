package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/company"
)

// Commands.
const (
	cmdClose     = "close"
	cmdResume    = "resume"
	cmdMintScope = "mint-scope"
)

// maxScopeTTL caps a mint-scope header's life, so a scope minted for an
// Inspector session can't become a long-lived credential.
const maxScopeTTL = 24 * time.Hour

const usage = `usage:
  agent close --company <id> --month YYYY-MM [--results-dir results] [--timeout 10m] [--config-dir config] [--no-explain]
  agent resume <run_id> [--results-dir results] [--timeout 10m] [--config-dir config] [--no-explain]
  agent mint-scope --company <id> --month YYYY-MM [--ttl 1h]
    prints an X-Run-Scope header value (at most 24h) for a manual MCP Inspector session; it needs
    MCP_SCOPE_KEY, and the agent's other required variables must be set too, though it uses none of them`

// errUsage is wrapped by every argument error.
var errUsage = errors.New("agent: bad arguments")

// command is one parsed command line.
type command struct {
	name       string
	company    string
	month      string
	runID      uuid.UUID
	resultsDir string
	timeout    time.Duration
	configDir  string
	// noExplain skips the explain (and verify) steps: no model is called
	// and the run ends partial.
	noExplain bool
	// ttl is how long a mint-scope header stays valid.
	ttl time.Duration
}

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s\n%s", errUsage, fmt.Sprintf(format, a...), usage)
}

// parseArgs parses the arguments after the global flags: a command and
// its flags. --timeout may shorten the run's 10-minute deadline, never
// lengthen it.
func parseArgs(args []string) (command, error) {
	if len(args) == 0 {
		return command{}, usageErr("no command")
	}
	c := command{name: args[0]}
	if c.name == cmdMintScope {
		return parseMintScope(args[1:])
	}
	fs := flag.NewFlagSet("agent "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.resultsDir, "results-dir", agent.DefaultResultsDir, "directory for runs/<run_id>.md")
	fs.DurationVar(&c.timeout, "timeout", agent.DefaultRunTimeout, "run deadline, at most 10m")
	fs.StringVar(&c.configDir, "config-dir", "config", "directory holding companies/, rules.yaml and pricing.yaml")
	fs.BoolVar(&c.noExplain, "no-explain", false, "skip explaining: no model calls, the run ends partial")

	rest := args[1:]
	switch c.name {
	case cmdClose:
		fs.StringVar(&c.company, "company", "", "company ID, such as sharma")
		fs.StringVar(&c.month, "month", "", "month to close, YYYY-MM")
		if err := fs.Parse(rest); err != nil {
			return command{}, usageErr("%v", err)
		}
		if fs.NArg() > 0 {
			return command{}, usageErr("unexpected argument %.40q", fs.Arg(0))
		}
		if c.company == "" || c.month == "" {
			return command{}, usageErr("close needs --company and --month")
		}
		if _, err := company.ParseMonth(c.month); err != nil {
			return command{}, usageErr("--month: %v", err)
		}
	case cmdResume:
		var positional []string
		// The run ID may come before or after the flags.
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			positional, rest = append(positional, rest[0]), rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return command{}, usageErr("%v", err)
		}
		positional = append(positional, fs.Args()...)
		if len(positional) != 1 {
			return command{}, usageErr("resume needs exactly one run ID")
		}
		id, err := uuid.Parse(positional[0])
		if err != nil || id == uuid.Nil {
			return command{}, usageErr("run ID %.40q is not a UUID", positional[0])
		}
		c.runID = id
	default:
		return command{}, usageErr("unknown command %.40q", c.name)
	}
	if c.timeout <= 0 || c.timeout > agent.DefaultRunTimeout {
		return command{}, usageErr("--timeout must be above 0 and at most %s", agent.DefaultRunTimeout)
	}
	if c.resultsDir == "" || c.configDir == "" {
		return command{}, usageErr("--results-dir and --config-dir must not be empty")
	}
	return c, nil
}

// parseMintScope parses mint-scope's flags. The company is not checked
// against the profiles: a scope for an unknown company reads nothing.
func parseMintScope(args []string) (command, error) {
	c := command{name: cmdMintScope}
	fs := flag.NewFlagSet("agent "+cmdMintScope, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.company, "company", "", "company ID, such as sharma")
	fs.StringVar(&c.month, "month", "", "the run's month, YYYY-MM")
	fs.DurationVar(&c.ttl, "ttl", time.Hour, "how long the scope stays valid, at most 24h")
	if err := fs.Parse(args); err != nil {
		return command{}, usageErr("%v", err)
	}
	if fs.NArg() > 0 {
		return command{}, usageErr("unexpected argument %.40q", fs.Arg(0))
	}
	if c.company == "" || c.month == "" {
		return command{}, usageErr("mint-scope needs --company and --month")
	}
	if _, err := company.ParseMonth(c.month); err != nil {
		return command{}, usageErr("--month: %v", err)
	}
	if c.ttl <= 0 || c.ttl > maxScopeTTL {
		return command{}, usageErr("--ttl must be above 0 and at most %s", maxScopeTTL)
	}
	return c, nil
}
