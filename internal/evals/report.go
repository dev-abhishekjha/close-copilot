package evals

import (
	"fmt"
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
