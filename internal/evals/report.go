package evals

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// RenderReport renders score.md: one Markdown table per metric, then the
// missed items, false alarms, unscored findings and failed runs. The
// output depends only on the score, so it is byte-stable.
func RenderReport(s Score) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Eval score: %s\n\n", mdCell(s.Suite))
	w("Commit `%s`, models fast `%s` / strong `%s`, agent %t. %d months scored, %d failed runs.\n\n",
		mdCode(s.Commit), mdCode(s.Models.Fast), mdCode(s.Models.Strong), s.Agent, s.Months, len(s.FailedRuns))

	w("## Overall\n\n")
	w("| Metric | Value |\n| --- | --- |\n")
	w("| Recall | %s |\n", s.Overall.Recall)
	w("| Precision | %s |\n", s.Overall.Precision)
	w("| Planted | %d |\n| Caught | %d |\n| Missed | %d |\n", s.Overall.Planted, s.Overall.Caught, s.Overall.Missed)
	w("| Scored findings | %d |\n| False alarms | %d |\n| Unscored findings | %d |\n",
		s.Overall.Findings, s.Overall.FalseAlarms, s.Overall.Unscored)
	w("| Clean-month false alarms | %d (%d clean months, %d failed) |\n", s.Clean.FalseAlarms, s.Clean.Months, s.Clean.FailedMonths)
	w("| Verified rate | %s |\n", s.VerifiedRate)
	w("| Verifier rejects | %d |\n| Explain retries | %d |\n", s.VerifierRejects, s.Retries)
	w("| Investigation accuracy | %s |\n", unmeasured(s.InvestigationAccuracy.Measured, s.InvestigationAccuracy.Reason, ""))
	writes := ""
	if s.UnauthorizedWrites.Count != nil {
		writes = fmt.Sprint(*s.UnauthorizedWrites.Count)
	}
	w("| Unauthorized writes | %s |\n\n", unmeasured(s.UnauthorizedWrites.Checked, s.UnauthorizedWrites.Reason, writes))

	w("## By finding type\n\n")
	w("| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |\n")
	w("| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |\n")
	names := make([]string, 0, len(s.Types))
	for k := range s.Types {
		names = append(names, k)
	}
	slices.Sort(names)
	if len(names) == 0 {
		w("| (none) | 0 | 0 | 0 | n/a | 0 | 0 | 0 | n/a |\n")
	}
	for _, k := range names {
		t := s.Types[k]
		w("| `%s` | %d | %d | %d | %s | %d | %d | %d | %s |\n", mdCode(k), t.Planted, t.Caught, t.Missed, t.Recall,
			t.Findings, t.Correct, t.FalseAlarms, t.Precision)
	}
	w("\n")

	w("## Cost and time per close run\n\n")
	w("| Metric | Total | Mean | p95 |\n| --- | ---: | ---: | ---: |\n")
	w("| Duration (ms) | %d | %d | %d |\n", s.Runs.DurationMS.Total, s.Runs.DurationMS.Mean, s.Runs.DurationMS.P95)
	w("| Cost (USD) | %s | %s | %s |\n\n", s.Runs.CostUSD.Total, s.Runs.CostUSD.Mean, s.Runs.CostUSD.P95)
	w("%d runs; tokens: %d input, %d output, %d cache read.\n\n", s.Runs.Runs, s.Runs.Tokens.Input, s.Runs.Tokens.Output, s.Runs.Tokens.CacheRead)

	w("## Planted items\n\n")
	if len(s.Items) == 0 {
		w("None.\n\n")
	} else {
		w("| Item | Type | Keys | Outcome | Finding | Evidence |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, it := range s.Items {
			w("| %s | `%s` | %s | %s | %s | %s |\n", mdCell(it.Key), mdCode(it.Type), mdCell(canonicalKeys(it.Keys)),
				outcome(it.Outcome, it.Reason), mdCell(orDash(it.FindingID)), mdCell(orDash(it.Evidence)))
		}
		w("\n")
	}

	w("## Missed\n\n")
	missed := 0
	for _, it := range s.Items {
		if it.Outcome == OutcomeCaught {
			continue
		}
		missed++
		w("- %s `%s` %s: %s; evidence `%s`; repro `%s`\n", mdCell(it.Key), mdCode(it.Type), mdCell(canonicalKeys(it.Keys)),
			outcome(it.Outcome, it.Reason), mdCode(it.Evidence), mdCode(it.Repro))
	}
	if missed == 0 {
		w("None.\n")
	}
	w("\n")

	w("## False alarms\n\n")
	if len(s.FalseAlarms) == 0 {
		w("None.\n")
	}
	for _, fa := range s.FalseAlarms {
		w("- %s-%s `%s` %s (%s), finding %s; evidence `%s`; repro `%s`\n", mdCell(fa.Company), mdCell(fa.Month),
			mdCode(fa.Type), mdCell(canonicalKeys(fa.Keys)), fa.Reason, mdCell(fa.FindingID), mdCode(fa.Evidence), mdCode(fa.Repro))
	}
	w("\n")

	if len(s.Investigations) > 0 {
		w("## Investigation cases\n\n")
		for _, x := range s.Investigations {
			w("- %s %s: %s\n", mdCell(x.Key), mdCell(canonicalKeys(x.Keys)), x.Outcome)
		}
		w("\n")
	}

	w("## Unscored findings\n\n")
	if len(s.Unscored) == 0 {
		w("None.\n")
	}
	for _, u := range s.Unscored {
		w("- %s-%s `%s` %s, finding %s\n", mdCell(u.Company), mdCell(u.Month), mdCode(u.Type), mdCell(canonicalKeys(u.Keys)), mdCell(u.FindingID))
	}
	w("\n")

	w("## Failed runs\n\n")
	if len(s.FailedRuns) == 0 {
		w("None.\n")
	}
	for _, r := range s.FailedRuns {
		w("- %s-%s: status %s, %s; evidence `%s`; repro `%s`\n", mdCell(r.Company), mdCell(r.Month), mdCell(orDash(r.Status)),
			mdCell(orDash(r.Reason)), mdCode(r.Evidence), mdCode(r.Repro))
	}
	return b.String()
}

// SummaryFile is the gate summary eval score writes next to score.md, for
// $GITHUB_STEP_SUMMARY.
const SummaryFile = "summary.md"

// Summary is what eval score checked beyond the score itself.
type Summary struct {
	// Requirements are the failed --require checks.
	Requirements []string
	// Compare is the comparison; nil when no --compare was given.
	Compare *Comparison
	// Notes say which rules were skipped and why.
	Notes []string
}

// Failed reports whether any requirement or comparison failed.
func (sm Summary) Failed() bool {
	return len(sm.Requirements) > 0 || (sm.Compare != nil && len(sm.Compare.Failures) > 0)
}

// RenderSummary renders summary.md: the gate's verdict, every comparison
// failure with its item, outcome, repro and evidence, the failed
// requirements, the skipped rules, then score.md.
func RenderSummary(s Score, sm Summary) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	verdict := "PASS"
	if sm.Failed() {
		verdict = "FAIL"
	}
	w("# Eval gate (G4): %s\n\n", verdict)
	w("Suite %s, agent %t (baseline tier %s).\n\n", mdCell(s.Suite), s.Agent, TierFor(s.Agent))

	w("## Comparison with the baseline\n\n")
	switch {
	case sm.Compare == nil:
		w("Not compared (no --compare).\n\n")
	case len(sm.Compare.Failures) == 0:
		w("No regressions.\n\n")
	default:
		w("| Item | Failure | Was | Now | Detail | Repro | Evidence |\n| --- | --- | --- | --- | --- | --- | --- |\n")
		for _, e := range sm.Compare.Failures {
			w("| %s | %s | %s | %s | %s | `%s` | `%s` |\n", mdCell(e.Key), mdCell(e.Kind), mdCell(orDash(e.Was)), mdCell(orDash(e.Now)),
				mdCell(strings.TrimSpace(e.Detail)), mdCode(e.Repro), mdCode(e.Evidence))
		}
		w("\n")
	}
	if sm.Compare != nil && len(sm.Compare.New) > 0 {
		w("New since the baseline (not failures):\n\n")
		for _, e := range sm.Compare.New {
			w("- %s: %s\n", mdCell(e.Key), mdCell(e.Now))
		}
		w("\n")
	}

	w("## Requirements\n\n")
	if len(sm.Requirements) == 0 {
		w("None failed.\n\n")
	}
	for _, r := range sm.Requirements {
		w("- %s\n", mdCell(r))
	}
	if len(sm.Requirements) > 0 {
		w("\n")
	}
	if len(sm.Notes) > 0 {
		w("## Skipped rules\n\n")
		for _, n := range sm.Notes {
			w("- %s\n", mdCell(n))
		}
		w("\n")
	}
	w("---\n\n")
	b.WriteString(RenderReport(s))
	return b.String()
}

// WriteSummary writes summary.md into dir, atomically. It refuses a dir
// CheckOutputPath refuses.
func WriteSummary(dir string, s Score, sm Summary) error {
	if err := CheckOutputPath("score --out", dir); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, SummaryFile), []byte(RenderSummary(s, sm)))
}

func unmeasured(ok bool, reason, value string) string {
	if !ok {
		return "not measured: " + mdCell(reason)
	}
	return value
}

func outcome(o, reason string) string {
	if reason == "" {
		return o
	}
	return o + " (" + mdCell(reason) + ")"
}

// mdEscaper backslash-escapes every character that could start Markdown
// or HTML markup (a link, an image, a tag, emphasis, a heading) and the
// backslash itself, and flattens line breaks, so finding keys, failure
// reasons and paths render as literal text. A backtick becomes a quote so
// it can't open a code span.
var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "|", `\|`, "<", `\<`, ">", `\>`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`,
	"!", `\!`, "*", `\*`, "_", `\_`, "#", `\#`, "\n", " ", "\r", " ", "`", "'",
)

// mdCell makes text safe inside a Markdown table cell or list item.
func mdCell(s string) string {
	return mdEscaper.Replace(s)
}

// mdCode makes text safe inside a code span.
func mdCode(s string) string {
	return strings.NewReplacer("`", "'", "\n", " ", "\r", " ", "|", `\|`).Replace(s)
}
