package agent

// The close report (CC-703): a templated Synthesizer in plain Go, with no
// model call. It renders one run as Markdown: the run, its outcome, its
// steps, and each finding with its amount in rupees, its evidence IDs and
// snapshots, its suggested action, explanation, citations, proposed entry
// and verification when present, the average model cost per explained
// finding (CC-704), and the verification pass rate with the retries and
// the findings left for review (CC-705), plus a line when COPILOT_FAULT is
// on. The same
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
	"strconv"
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
	// Fault is the COPILOT_FAULT mode the run's stored explanations show
	// ("" for none): the workflow reads it from the explanation artifacts'
	// fault_injected mark, never from the process environment.
	Fault string
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
	explained := 0
	for _, f := range d.Findings {
		if explain[f.ID.String()].Status == store.StepDone {
			explained++
		}
	}
	if explained == 0 {
		p("| Average cost per explained finding | none explained |\n")
	} else {
		p("| Average cost per explained finding | USD %s (run cost over %d explained) |\n", cell(averageUSD(cost, explained)), explained)
	}
	p("| Verification | %s |\n", cell(verificationSummary(d.Findings, explain, verify)))
	if d.Fault != "" {
		p("| Fault injection | COPILOT_FAULT=%s: one explanation was corrupted on purpose on its first attempt |\n", cell(d.Fault))
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
		if len(f.Citations) > 0 {
			cs := make([]string, len(f.Citations))
			for j, c := range f.Citations {
				cs[j] = fmt.Sprintf("[%s §%s]", oneLine(c.DocID), oneLine(c.Section))
			}
			p("- Citations: %s\n", strings.Join(cs, " "))
		}
		if f.Proposal != nil {
			p("- Proposed entry: %s\n", proposalLine(f.Proposal.Payload))
		}
		p("- Verification: %s\n", verificationLine(f, verify[f.ID.String()]))
	}
	return b.String()
}

// verificationSummary is "verified N of M explanations (pass rate P%), R
// retried, K needs_review": M findings have an explanation, N of them a
// verify step done without a reason (passed), R an explain step that got
// verifier feedback, and K are marked needs_review.
func verificationSummary(findings []store.Finding, explain, verify map[string]store.Step) string {
	explained, passed, retried, review := 0, 0, 0, 0
	for _, f := range findings {
		id := f.ID.String()
		e := explain[id]
		if len(e.Feedback) > 0 {
			retried++
		}
		if f.Status == "needs_review" {
			review++
		}
		if e.Status != store.StepDone {
			continue
		}
		explained++
		if v, ok := verify[id]; ok && v.Status == store.StepDone && v.Error == nil {
			passed++
		}
	}
	rate := "n/a"
	if explained > 0 {
		rate = strconv.Itoa(passed*100/explained) + "%"
	}
	return fmt.Sprintf("verified %d of %d explanations (pass rate %s), %d retried, %d needs_review", passed, explained, rate, retried, review)
}

// proposalLine renders a proposed journal entry on one line.
func proposalLine(pl store.JournalPayload) string {
	parts := []string{oneLine(pl.PostingDate)}
	for _, l := range pl.Lines {
		switch {
		case l.DebitPaise > 0:
			parts = append(parts, fmt.Sprintf("Dr %s %s", oneLine(l.Account), l.DebitPaise.Format()))
		case l.CreditPaise > 0:
			parts = append(parts, fmt.Sprintf("Cr %s %s", oneLine(l.Account), l.CreditPaise.Format()))
		}
	}
	if r := oneLine(pl.Remark); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "; ")
}

// averageUSD divides a decimal USD amount (such as "0.0042") by n, exactly
// to the micro-dollar with halves rounded up, and returns decimal text.
// Digits beyond the sixth decimal are dropped. Text that is not a plain
// non-negative decimal gives "unknown". No floating point is involved.
func averageUSD(total string, n int) string {
	whole, frac, _ := strings.Cut(total, ".")
	if whole == "" || n <= 0 || !allDigits(whole) || !allDigits(frac) || len(whole) > 12 {
		return "unknown"
	}
	if len(frac) > 6 {
		frac = frac[:6]
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil {
		return "unknown"
	}
	micro := w*1_000_000 + f
	avg := (2*micro + int64(n)) / (2 * int64(n))
	out := strconv.FormatInt(avg/1_000_000, 10)
	if rest := strings.TrimRight(fmt.Sprintf("%06d", avg%1_000_000), "0"); rest != "" {
		out += "." + rest
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
