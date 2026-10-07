package gates

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
)

// Exit codes shared by every gate command.
const (
	ExitPass  = 0 // the gate passed (or the command succeeded)
	ExitFail  = 1 // the gate failed; the report says why
	ExitUsage = 2 // bad usage or an I/O error; no verdict
)

// Env is what a command reads from its process. Tests replace every field.
type Env struct {
	Dir      string // repository root; commands run from it
	Stdout   io.Writer
	Stderr   io.Writer
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

// DefaultEnv is the process environment, run from the current directory.
func DefaultEnv() Env {
	return Env{Dir: ".", Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, LookPath: exec.LookPath}
}

// command holds what every gate command shares: its name and arguments (for
// the repro line), the logger and the common flags.
type command struct {
	env        Env
	name       string
	args       []string
	log        *slog.Logger
	fs         *flag.FlagSet
	base       *string
	reportPath *string
	attempt    *int
}

func newCommand(env Env, name string, args []string) *command {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return &command{
		env:        env,
		name:       name,
		args:       args,
		log:        slog.New(slog.NewJSONHandler(env.Stderr, nil)).With("cmd", name),
		fs:         fs,
		base:       fs.String("base", "main", "git ref the change is compared against"),
		reportPath: fs.String("report", "", "also write the report JSON to this path"),
		attempt:    fs.Int("attempt", 1, "build attempt number, recorded in the report"),
	}
}

// parse parses flags that may come before, between or after positional
// arguments, and returns the positional ones.
func (c *command) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := c.fs.Parse(args); err != nil {
			return nil, err
		}
		args = c.fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (c *command) usage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return ExitPass
	}
	c.log.Error("bad usage", "err", err)
	return ExitUsage
}

func (c *command) fail(msg string, err error) int {
	c.log.Error(msg, "err", err)
	return ExitUsage
}

func (c *command) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.env.Dir, p)
}

func (c *command) repo() Repo { return Repo{Dir: c.env.Dir, Log: c.log} }

// repro is the command line that reproduces this run.
func (c *command) repro() string { return CommandLine(c.name, c.args) }

// finish records the problems, prints the one-line summary and, on failure,
// the report JSON; it writes the report to --report whenever that is given,
// so a pass replaces an earlier failure.
func (c *command) finish(r *Report, problems []Problem, passDetail string) int {
	for _, p := range problems {
		if err := r.Add(Blocking{Check: p.Check, Message: p.Message, Repro: c.repro(), Evidence: p.Evidence}); err != nil {
			return c.fail("gate bug: invalid blocking entry", err)
		}
	}
	out, err := r.JSON()
	if err != nil {
		return c.fail("gate bug", err)
	}
	detail := passDetail
	if r.Failed() {
		detail = fmt.Sprintf("%d blocking", len(r.Blocking))
	}
	task := r.Task
	if task == "" {
		task = "-"
	}
	prefix := r.Gate + " " + c.name
	if r.Gate == c.name {
		prefix = c.name
	}
	_, _ = fmt.Fprintf(c.env.Stdout, "%s %s: %s (%s)\n", prefix, task, r.Verdict, detail)
	if r.Failed() {
		_, _ = c.env.Stdout.Write(out)
	}
	if *c.reportPath != "" {
		if err := r.Save(c.path(*c.reportPath)); err != nil {
			return c.fail("write report", err)
		}
	}
	if r.Failed() {
		return ExitFail
	}
	return ExitPass
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=,@%+-]+$`)

// CommandLine renders `go run ./gates/cmd/<name> <args>` with shell quoting.
func CommandLine(name string, args []string) string {
	parts := []string{"go", "run", "./gates/cmd/" + name}
	for _, a := range args {
		if !plainArg.MatchString(a) {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// RunReady is G0: `ready <spec> [--base main] [--report path]`.
func RunReady(ctx context.Context, env Env, args []string) int {
	c := newCommand(env, "ready", args)
	pos, err := c.parse(args)
	if err != nil {
		return c.usage(err)
	}
	if len(pos) != 1 {
		return c.usage(errors.New("want exactly one spec path: ready specs/CC-xxx.md [--base main]"))
	}
	specPath := pos[0]
	data, err := os.ReadFile(c.path(specPath)) //nolint:gosec // the path is the operator's command-line argument
	if err != nil {
		return c.fail("read spec", err)
	}
	repo := c.repo()
	r := NewReport("G0", SpecID(specPath))
	r.Commit = repo.ShortCommit(ctx)
	r.Attempt = *c.attempt

	s, err := ParseSpec(data)
	if err != nil {
		return c.finish(r, []Problem{{Check: "front_matter", Message: err.Error(), Evidence: specPath}}, "")
	}
	if s.ID != "" {
		r.Task = s.ID
	}
	r.MaxAttempts = s.MaxAttempts()

	done, err := repo.Done(ctx, *c.base)
	if err != nil {
		return c.fail("done tickets", err)
	}
	inFlight, err := repo.InFlight(ctx, *c.base)
	if err != nil {
		return c.fail("in-flight tickets", err)
	}
	problems := CheckReady(specPath, s, ReadyContext{
		Base: *c.base, Done: done, InFlight: inFlight, Root: env.Dir, LookPath: env.LookPath,
	})
	return c.finish(r, problems, "spec is ready for a worker")
}

// RunDeclared is G1's declared-files check:
// `declared --spec <spec> [--base main] [--report path]`.
func RunDeclared(ctx context.Context, env Env, args []string) int {
	c := newCommand(env, "declared", args)
	specPath := c.fs.String("spec", "", "the ticket's spec, specs/CC-xxx.md (required)")
	pos, err := c.parse(args)
	if err != nil {
		return c.usage(err)
	}
	if len(pos) != 0 || *specPath == "" {
		return c.usage(errors.New("usage: declared --spec specs/CC-xxx.md [--base main]"))
	}
	s, err := LoadSpec(c.path(*specPath))
	if err != nil {
		return c.fail("load spec", err)
	}
	repo := c.repo()
	r := NewReport("G1", s.ID)
	r.Commit = repo.ShortCommit(ctx)
	r.Attempt = *c.attempt
	r.MaxAttempts = s.MaxAttempts()

	changed, err := repo.ChangedFiles(ctx, *c.base)
	if err != nil {
		return c.fail("changed files", err)
	}
	problems, err := DeclaredProblems(s, changed)
	if err != nil {
		return c.fail("declared files", err)
	}
	return c.finish(r, problems, fmt.Sprintf("%d changed files, all declared", len(changed)))
}

// RunProtected is G1's protected-path check:
// `protected [--base main] [--labels a,b] [--task CC-xxx] [--report path]`.
// Labels come from --labels and, in CI, from $GITHUB_EVENT_PATH.
func RunProtected(ctx context.Context, env Env, args []string) int {
	c := newCommand(env, "protected", args)
	labelsFlag := c.fs.String("labels", "", "comma-separated pull request labels")
	task := c.fs.String("task", "", "ticket ID, recorded in the report")
	pos, err := c.parse(args)
	if err != nil {
		return c.usage(err)
	}
	if len(pos) != 0 {
		return c.usage(errors.New("usage: protected [--base main] [--labels approved]"))
	}
	labels := SplitLabels(*labelsFlag)
	if p := env.Getenv("GITHUB_EVENT_PATH"); p != "" {
		evLabels, err := LabelsFromEvent(p)
		if err != nil {
			return c.fail("labels from event", err)
		}
		labels = append(labels, evLabels...)
	}

	repo := c.repo()
	r := NewReport("G1", *task)
	r.Commit = repo.ShortCommit(ctx)
	r.Attempt = *c.attempt

	changed, err := repo.ChangedFiles(ctx, *c.base)
	if err != nil {
		return c.fail("changed files", err)
	}
	hits, err := ProtectedChanges(changed)
	if err != nil {
		return c.fail("protected paths", err)
	}
	problems, err := ProtectedProblems(changed, labels)
	if err != nil {
		return c.fail("protected paths", err)
	}
	detail := "no protected path changed"
	if len(hits) > 0 {
		detail = fmt.Sprintf("%d protected files changed, %q label present", len(hits), ApprovedLabel)
	}
	return c.finish(r, problems, detail)
}

// RunGraph is `graph ready [--base main] [--json] [--graph tasks/graph.yaml]`
// and `graph check <path>`.
func RunGraph(ctx context.Context, env Env, args []string) int {
	c := newCommand(env, "graph", args)
	asJSON := c.fs.Bool("json", false, "graph ready: print JSON")
	graphPath := c.fs.String("graph", "tasks/graph.yaml", "graph ready: task graph path")
	pos, err := c.parse(args)
	if err != nil {
		return c.usage(err)
	}
	switch {
	case len(pos) == 1 && pos[0] == "ready":
		return c.graphReady(ctx, *graphPath, *asJSON)
	case len(pos) == 2 && pos[0] == "check":
		return c.graphCheck(pos[1])
	default:
		return c.usage(errors.New("usage: graph ready [--base main] [--json] | graph check <path>"))
	}
}

func (c *command) graphCheck(path string) int {
	r := NewReport("graph", "")
	data, err := os.ReadFile(c.path(path)) //nolint:gosec // the path is the operator's command-line argument
	if err != nil {
		return c.fail("read task graph", err)
	}
	g, err := ParseGraph(data)
	if err != nil {
		return c.finish(r, []Problem{{Check: "parse", Message: err.Error(), Evidence: path}}, "")
	}
	return c.finish(r, CheckGraph(g, path), fmt.Sprintf("%d tasks", len(g.Tasks)))
}

// readyRow is one line of `graph ready`.
type readyRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Phase        int    `json:"phase"`
	Owner        string `json:"owner"`
	Risk         string `json:"risk"`
	NeedsERPNext bool   `json:"needs_erpnext"`
}

func (c *command) graphReady(ctx context.Context, graphPath string, asJSON bool) int {
	g, err := LoadGraph(c.path(graphPath))
	if err != nil {
		return c.fail("load task graph", err)
	}
	repo := c.repo()
	done, err := repo.Done(ctx, *c.base)
	if err != nil {
		return c.fail("done tickets", err)
	}
	tickets, err := repo.InFlight(ctx, *c.base)
	if err != nil {
		return c.fail("in-flight tickets", err)
	}
	inFlight := make(map[string]bool, len(tickets))
	for _, t := range tickets {
		inFlight[t.ID] = true
	}
	rows := []readyRow{}
	for _, t := range ReadyTasks(g, done, inFlight) {
		rows = append(rows, readyRow{ID: t.ID, Title: t.Title, Phase: t.Phase, Owner: t.Owner, Risk: t.Risk, NeedsERPNext: t.NeedsERPNext})
	}
	summary := fmt.Sprintf("%d tickets ready on %s (%d done, %d in flight)", len(rows), *c.base, len(done), len(tickets))
	if asJSON {
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return c.fail("marshal", err)
		}
		_, _ = fmt.Fprintf(c.env.Stdout, "%s\n", out)
		c.log.Info(summary)
		return ExitPass
	}
	tw := tabwriter.NewWriter(c.env.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tPHASE\tOWNER\tRISK\tERPNEXT\tTITLE")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%t\t%s\n", r.ID, r.Phase, r.Owner, r.Risk, r.NeedsERPNext, r.Title)
	}
	if err := tw.Flush(); err != nil {
		return c.fail("write", err)
	}
	_, _ = fmt.Fprintln(c.env.Stdout, summary)
	return ExitPass
}
