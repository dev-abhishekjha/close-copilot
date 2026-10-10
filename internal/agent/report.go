package agent

// The close report (CC-703): a templated Synthesizer in plain Go, with no
// model call. It renders one run as Markdown: the run, its outcome, its
// steps, and each finding with its amount in rupees, its evidence IDs and
// snapshots, and its explanation and verification when present. The same
// Markdown is stored as a report artifact and written to
// <results dir>/runs/<run_id>.md.
//
// The report holds no timings, so two runs of the same month render the
// same text apart from run and finding IDs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// DefaultResultsDir is where reports go when no directory is given.
const DefaultResultsDir = "results"

// ReportData is what a report is rendered from.
type ReportData struct {
	RunID    uuid.UUID
	Company  string
	Month    string
	Outcome  string // done or partial
	Reason   string // why the run is partial
	Usage    store.RunUsage
	Findings []store.Finding // in report order
	Steps    []store.Step
}

// ReportArtifact is the content of a report artifact.
type ReportArtifact struct {
	RunID    string `json:"run_id"`
	Markdown string `json:"markdown"`
}

// shortSHA is the first 12 hex digits of an artifact address.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// oneLine flattens text to one line of Markdown: control characters
// become spaces and backticks are dropped, so a value can't break the
// layout.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cell is oneLine with table pipes escaped.
func cell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", `\|`)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// stepRow is one line of the steps table.
type stepRow struct {
	kind, subject, status, note string
}

// RenderReport renders the report Markdown.
func RenderReport(d ReportData) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# Close report: %s, %s\n\n", oneLine(d.Company), oneLine(d.Month))
	p("| | |\n|---|---|\n")
	p("| Run | `%s` |\n", d.RunID)
	p("| Company | %s |\n", cell(d.Company))
	p("| Month | %s |\n", cell(d.Month))
	p("| Outcome | %s |\n", cell(d.Outcome))
	if d.Reason != "" {
		p("| Note | %s |\n", cell(d.Reason))
	}
	p("| Findings | %d |\n", len(d.Findings))
	cost := d.Usage.CostUSD
	if cost == "" {
		cost = "0"
	}
	p("| Model usage | %d input tokens, %d output tokens, %d cache-read tokens, USD %s |\n",
		d.Usage.InputTokens, d.Usage.OutputTokens, d.Usage.CacheReadTokens, cell(cost))

	explain := map[string]store.Step{}
	verify := map[string]store.Step{}
	var rows []stepRow
	for _, kind := range []string{store.StepKindRouter, "check.", store.StepKindRetrieve} {
		rows = append(rows, plainSteps(d.Steps, kind)...)
	}
	for _, s := range d.Steps {
		switch s.Kind {
		case store.StepKindExplain:
			explain[s.Subject] = s
		case store.StepKindVerify:
			verify[s.Subject] = s
		}
	}
	rows = append(rows, perFinding(store.StepKindExplain, explain, d.Findings), perFinding(store.StepKindVerify, verify, d.Findings))
	rows = append(rows, plainSteps(d.Steps, store.StepKindInvestigate)...)

	p("\n## Steps\n\n| Step | Subject | Status | Note |\n|---|---|---|---|\n")
	for _, r := range rows {
		p("| %s | %s | %s | %s |\n", cell(r.kind), cell(r.subject), cell(r.status), cell(r.note))
	}

	p("\n## Findings\n")
	if len(d.Findings) == 0 {
		p("\nNo findings.\n")
	}
	for i, f := range d.Findings {
		p("\n### %d. %s\n\n", i+1, oneLine(f.Title))
		p("- Finding: `%s`\n", f.ID)
		p("- Type: %s\n", oneLine(f.Type))
		p("- Severity: %s\n", oneLine(f.Severity))
		if f.AmountPaise != nil {
			p("- Amount: %s\n", f.AmountPaise.Format())
		}
		p("- Status: %s\n", oneLine(f.Status))
		if len(f.Evidence) == 0 {
			p("- Evidence: none\n")
		} else {
			p("- Evidence:\n")
			for _, e := range f.Evidence {
				ids := make([]string, len(e.IDs))
				for j, id := range e.IDs {
					ids[j] = oneLine(id)
				}
				p("  - %s/%s: %s (snapshot `%s`)\n", oneLine(e.Server), oneLine(e.Tool), strings.Join(ids, ", "), shortSHA(e.Artifact))
			}
		}
		if f.Action != nil {
			p("- Action: %s\n", oneLine(*f.Action))
		}
		p("- Explanation: %s\n", explanationLine(f, explain[f.ID.String()]))
		p("- Verification: %s\n", verificationLine(f, verify[f.ID.String()]))
	}
	return b.String()
}

// plainSteps are the steps of one kind ("check." for every check), in
// subject order, one row each.
func plainSteps(steps []store.Step, kind string) []stepRow {
	var out []stepRow
	for _, s := range steps {
		match := s.Kind == kind || (strings.HasSuffix(kind, ".") && strings.HasPrefix(s.Kind, kind))
		if !match {
			continue
		}
		out = append(out, stepRow{kind: s.Kind, subject: s.Subject, status: s.Status, note: deref(s.Error)})
	}
	slices.SortStableFunc(out, func(a, b stepRow) int { return strings.Compare(a.kind+"\x00"+a.subject, b.kind+"\x00"+b.subject) })
	return out
}

// perFinding sums the explain or verify steps into one row: counts by
// status and the distinct notes, so the row doesn't depend on finding IDs.
func perFinding(kind string, steps map[string]store.Step, findings []store.Finding) stepRow {
	counts := map[string]int{}
	var notes []string
	for _, f := range findings {
		s, ok := steps[f.ID.String()]
		status := "not run"
		if ok {
			status = s.Status
			if n := deref(s.Error); n != "" && !slices.Contains(notes, n) {
				notes = append(notes, n)
			}
		}
		counts[status]++
	}
	var parts []string
	for _, st := range []string{store.StepDone, store.StepSkipped, store.StepFailed, store.StepRunning, store.StepPending, "not run"} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "none")
	}
	slices.Sort(notes)
	return stepRow{kind: kind, subject: fmt.Sprintf("%d findings", len(findings)), status: strings.Join(parts, ", "), note: strings.Join(notes, "; ")}
}

func explanationLine(f store.Finding, s store.Step) string {
	if f.Explanation != nil && *f.Explanation != "" {
		return oneLine(*f.Explanation)
	}
	switch s.Status {
	case store.StepDone:
		if len(s.OutputRefs) > 0 {
			return fmt.Sprintf("recorded (artifact `%s`)", shortSHA(s.OutputRefs[0]))
		}
		return "recorded"
	case store.StepSkipped, store.StepFailed:
		if n := deref(s.Error); n != "" {
			return fmt.Sprintf("missing (%s: %s)", s.Status, oneLine(n))
		}
		return "missing (" + s.Status + ")"
	}
	return "missing"
}

func verificationLine(f store.Finding, s store.Step) string {
	if f.Status == "needs_review" {
		return "needs review"
	}
	switch s.Status {
	case store.StepDone:
		if f.Verified {
			return "verified"
		}
		if len(s.OutputRefs) > 0 {
			return fmt.Sprintf("checked (verdict `%s`)", shortSHA(s.OutputRefs[0]))
		}
		return "checked"
	case store.StepSkipped, store.StepFailed:
		if n := deref(s.Error); n != "" {
			return fmt.Sprintf("not verified (%s: %s)", s.Status, oneLine(n))
		}
		return "not verified (" + s.Status + ")"
	}
	return "not verified"
}

// ReportPath is the report file of a run under dir ("" means
// DefaultResultsDir).
func ReportPath(dir string, runID uuid.UUID) string {
	if dir == "" {
		dir = DefaultResultsDir
	}
	return filepath.Join(dir, "runs", runID.String()+".md")
}

// writeReport writes the report atomically: a temporary file in the same
// directory, then a rename.
func writeReport(dir string, runID uuid.UUID, md string) (string, error) {
	path := ReportPath(dir, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("agent: report dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+runID.String()+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("agent: write report: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(md); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent: write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agent: write report: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("agent: write report: %w", err)
	}
	return path, nil
}

// artifactReader reads a stored artifact. *store.Store implements it.
type artifactReader interface {
	GetArtifact(ctx context.Context, sha string) (store.Artifact, error)
}

// restoreReport makes sure the report file of a run whose synthesize step
// is done exists, rewriting it from the step's report artifact if not.
func restoreReport(ctx context.Context, st artifactReader, dir string, runID uuid.UUID, refs []string) (string, error) {
	path := ReportPath(dir, runID)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("agent: report file: %w", err)
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("agent: run %s: synthesize step has no report artifact", runID)
	}
	a, err := st.GetArtifact(ctx, refs[0])
	if err != nil {
		return "", err
	}
	if a.Kind != store.ArtifactReport {
		return "", fmt.Errorf("agent: run %s: synthesize output is a %s artifact, not a report", runID, a.Kind)
	}
	var r ReportArtifact
	if err := json.Unmarshal(a.Content, &r); err != nil {
		return "", fmt.Errorf("agent: decode report artifact: %w", err)
	}
	if r.RunID != runID.String() {
		return "", fmt.Errorf("agent: report artifact belongs to run %.40q, not %s", r.RunID, runID)
	}
	return writeReport(dir, runID, r.Markdown)
}
