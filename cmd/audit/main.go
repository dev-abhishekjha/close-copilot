// Command audit rebuilds a finding from its stored artifacts (CC-705,
// CC-709) and re-runs the verifier offline. Usage:
//
//	audit rebuild <finding-id>
//
// rebuild loads the finding and its run, takes the run's explain/<id> and
// verify/<id> steps from run_steps and their artifacts from output_refs
// only (never from artifacts.run_id or produced_by, which keep the first
// writer of a content-addressed artifact), and prints:
//
//   - the evidence records the model saw (the EVIDENCE projection),
//   - every model call of the explain step, with its prompt and response,
//   - the explanation,
//   - the stored verdict and a verdict re-computed from the same artifacts.
//
// It also checks the findings row and the steps against the stored
// records: the verified flag must equal the verdict's pass; the
// explanation, action, citations and proposal must equal the explanation
// artifact's; a passing verdict's verify step is done with no error; a
// failing one is final only on the last explain attempt, with the finding
// in needs_review and the verify step done with an error.
//
// A finding whose last explain retry ended without an explanation (needs
// review, no final explanation) is rebuilt from the last failed verdict
// the verify step keeps in input_refs and the rejected explanation it
// names (run_steps keeps earlier attempts' outputs in input_refs).
//
// Exit codes: 0 when the re-computed verdict equals the stored one byte for
// byte and the findings row and steps match; 1 when anything differs, a
// stored artifact no longer matches its sha256, an explanation or verdict
// that run_steps names is missing, or a done explain step has no verdict;
// 2 on a usage or store error, or when the run is not finished
// (not done, partial or failed).
//
// It needs only DATABASE_URL: no ERPNext, no model and no MCP server.
//
// The output is what the model saw and said, so it can hold ledger, bank
// and document text. All data here is synthetic today; once CC-710
// pseudonymises identifiers, this output must still stay on the local
// machine: never paste it into a ticket or a log.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Exit codes.
const (
	exitMatch    = 0
	exitMismatch = 1
	exitError    = 2
)

// errMismatch reports a stored record that does not match its rebuild.
var errMismatch = errors.New("audit: stored record does not match the rebuild")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, config.FromEnv, openStore)
	stop()
	os.Exit(code)
}

// auditStore is what rebuild reads. *store.Store implements it.
type auditStore interface {
	agent.ArtifactGetter
	agent.CitationLookup
	GetFinding(ctx context.Context, id uuid.UUID) (store.Finding, error)
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
	ListSteps(ctx context.Context, runID uuid.UUID) ([]store.Step, error)
	ListLLMCalls(ctx context.Context, stepID uuid.UUID) ([]store.LLMCall, error)
}

var _ auditStore = (*store.Store)(nil)

// openStore opens the store at the DATABASE_URL.
func openStore(ctx context.Context, cfg config.Config) (auditStore, func(), error) {
	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return nil, nil, err
	}
	return st, st.Close, nil
}

// run is main without os.Exit, for tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer,
	load func(required ...string) (config.Config, error),
	open func(context.Context, config.Config) (auditStore, func(), error)) int {
	cmd, err := parseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	cfg, err := load(config.EnvDatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "audit: %v\n", err)
		return exitError
	}
	st, closeStore, err := open(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "audit: %v\n", err)
		return exitError
	}
	defer closeStore()

	err = rebuild(ctx, st, cmd.findingID, stdout)
	switch {
	case err == nil:
		return exitMatch
	case errors.Is(err, errMismatch), errors.Is(err, store.ErrArtifactMismatch):
		_, _ = fmt.Fprintf(stderr, "audit: MISMATCH: %v\n", err)
		return exitMismatch
	}
	_, _ = fmt.Fprintf(stderr, "audit: %v\n", err)
	return exitError
}

// rebuild prints a finding's record and re-computes its verdict. It
// returns nil when the re-computed verdict equals the stored one, an error
// wrapping errMismatch (or store.ErrArtifactMismatch) when it differs, and
// any other error for a store or usage problem (an unfinished run
// included).
//
// It rebuilds two kinds of finding:
//
//   - one whose explain step is done: from that explanation and the verify
//     step's verdict (output_refs);
//   - one whose last explain retry ended without an explanation (the
//     explain step failed or was skipped after a failed verdict, and the
//     finding needs review): from the last verdict the verify step keeps in
//     input_refs and the rejected explanation that verdict names, which
//     must be the last one the explain step keeps in input_refs.
func rebuild(ctx context.Context, st auditStore, findingID uuid.UUID, w io.Writer) error {
	f, err := st.GetFinding(ctx, findingID)
	if err != nil {
		return err
	}
	run, err := st.GetCloseRun(ctx, f.RunID)
	if err != nil {
		return err
	}
	if !runFinished(run.Status) {
		return fmt.Errorf("audit: run %s not finished (status %.20q); rebuild it once it ends", run.ID, run.Status)
	}
	steps, err := st.ListSteps(ctx, run.ID)
	if err != nil {
		return err
	}
	var explain, verify *store.Step
	for i := range steps {
		if steps[i].Subject != findingID.String() {
			continue
		}
		switch steps[i].Kind {
		case store.StepKindExplain:
			explain = &steps[i]
		case store.StepKindVerify:
			verify = &steps[i]
		}
	}

	var t target
	switch {
	case explain != nil && explain.Status == store.StepDone:
		if len(explain.OutputRefs) != 1 {
			return fmt.Errorf("audit: finding %s has no done explain step with one explanation", findingID)
		}
		if verify == nil || len(verify.OutputRefs) != 1 {
			// A done explanation without its verdict is an integrity failure.
			return fmt.Errorf("%w: finding %s has a done explain step but no verify step with one verdict", errMismatch, findingID)
		}
		t = target{explanation: explain.OutputRefs[0], verdict: verify.OutputRefs[0], explainAttempt: explain.Attempt}
	case explain != nil && rejectedRetry(*explain, verify):
		t, err = lastRejected(ctx, st, findingID, *explain, *verify)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("audit: finding %s has no done explain step with one explanation", findingID)
	}

	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("== finding %s\nrun %s, company %s, month %s, type %s, status %s\n", f.ID, run.ID, run.CompanyID, run.Month, f.Type, f.Status)
	p("explain step: attempt %d, %s; verify step: attempt %d, %s\n", explain.Attempt, explain.Status, verify.Attempt, verify.Status)
	if t.rejected {
		p("the last explain retry ended without an explanation: rebuilding the last rejected explanation (attempt %d)\n", t.explainAttempt)
	}

	evidence, err := agent.ProjectEvidence(ctx, st, f)
	if err != nil {
		return err
	}
	p("\n== evidence (as sent to the model)\n%s\n", indent(evidence))

	calls, err := st.ListLLMCalls(ctx, explain.ID)
	if err != nil {
		return err
	}
	for i, c := range calls {
		p("\n== model call %d of %d: %s, %d input tokens, %d output tokens, %d cache-read tokens, USD %s\n",
			i+1, len(calls), c.Model, c.InputTokens, c.OutputTokens, c.CacheReadTokens, orUnknown(c.CostUSD))
		for _, part := range []struct{ name, sha string }{{"prompt", c.PromptSHA256}, {"response", c.ResponseSHA256}} {
			a, err := st.GetArtifact(ctx, part.sha)
			if err != nil {
				return err
			}
			p("-- %s %s\n%s\n", part.name, part.sha, indent(a.Content))
		}
	}

	expl, err := getNamed(ctx, st, "explanation", t.explanation)
	if err != nil {
		return err
	}
	p("\n== explanation %s\n%s\n", t.explanation, indent(expl.Content))

	stored, err := getNamed(ctx, st, "verdict", t.verdict)
	if err != nil {
		return err
	}
	if stored.Kind != store.ArtifactVerdict {
		return fmt.Errorf("%w: verify output %s is a %s artifact, not a verdict", errMismatch, t.verdict, stored.Kind)
	}
	p("\n== stored verdict %s\n%s\n", t.verdict, indent(stored.Content))

	v := &agent.CodeVerifier{Store: readOnly{st}, Citations: st}
	verdict, err := v.Evaluate(ctx, agent.VerifyRequest{
		RunID: run.ID, Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: agent.EvidenceRefs(f), ExplanationRefs: []string{t.explanation},
		ExplainAttempt: t.explainAttempt,
	})
	if err != nil {
		return err
	}
	recomputed, sha, err := store.CanonicalHash(verdict)
	if err != nil {
		return err
	}
	p("\n== re-computed verdict %s\n%s\n", sha, indent(recomputed))
	if sha != t.verdict || !bytes.Equal(recomputed, stored.Content) {
		p("\n== result: MISMATCH\n")
		return fmt.Errorf("%w: stored verdict %s, re-computed %s", errMismatch, t.verdict, sha)
	}
	var diffs []string
	if t.rejected {
		diffs = rejectedDiffs(f, *verify, verdict.Pass)
	} else {
		diffs, err = rowDiffs(f, expl.Content, verdict.Pass)
		if err != nil {
			return err
		}
		diffs = append(diffs, stateDiffs(f, *explain, *verify, verdict.Pass)...)
	}
	if len(diffs) > 0 {
		p("\n== findings row or steps differ from the stored records: %s\n== result: MISMATCH\n", strings.Join(diffs, ", "))
		return fmt.Errorf("%w: %s", errMismatch, strings.Join(diffs, ", "))
	}
	p("\n== result: match\n")
	return nil
}

// getNamed loads an explanation or verdict artifact that run_steps names.
// A missing one is an integrity failure (errMismatch, exit 1), not a store
// error: the step says it was written.
func getNamed(ctx context.Context, st auditStore, what, sha string) (store.Artifact, error) {
	a, err := st.GetArtifact(ctx, sha)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Artifact{}, fmt.Errorf("%w: stored %s %s is missing: %w", errMismatch, what, sha, err)
	case err != nil:
		return store.Artifact{}, fmt.Errorf("stored %s %s: %w", what, sha, err)
	}
	return a, nil
}

// target is the explanation and verdict a rebuild re-checks.
type target struct {
	explanation, verdict string
	explainAttempt       int
	// rejected is true for a finding whose last explain retry ended
	// without an explanation.
	rejected bool
}

// runFinished reports whether a run status is terminal: done, partial or
// failed.
func runFinished(status string) bool {
	switch status {
	case store.RunDone, store.RunPartial, store.RunFailed:
		return true
	}
	return false
}

// rejectedRetry reports whether a finding's explain step ended without an
// explanation after a failed verdict: the explain step failed or was
// skipped, carries verifier feedback, and the verify step keeps at least
// one earlier verdict.
func rejectedRetry(explain store.Step, verify *store.Step) bool {
	return (explain.Status == store.StepFailed || explain.Status == store.StepSkipped) &&
		len(bytes.TrimSpace(explain.Feedback)) > 0 && len(explain.InputRefs) > 0 &&
		verify != nil && len(verify.InputRefs) > 0
}

// lastRejected finds the last stored verdict of a finding whose last
// explain retry ended without an explanation, and the rejected explanation
// it names. The verdict is the last verdict artifact in the verify step's
// input_refs; the explanation it names must be the last one in the explain
// step's input_refs (provenance comes from run_steps only), and its
// attempt must be earlier than the explain step's.
func lastRejected(ctx context.Context, st auditStore, findingID uuid.UUID, explain, verify store.Step) (target, error) {
	var t target
	for i := len(verify.InputRefs) - 1; i >= 0 && t.verdict == ""; i-- {
		a, err := getNamed(ctx, st, "verdict", verify.InputRefs[i])
		if err != nil {
			return target{}, err
		}
		if a.Kind != store.ArtifactVerdict {
			continue
		}
		var v agent.VerdictArtifact
		if err := json.Unmarshal(a.Content, &v); err != nil {
			return target{}, fmt.Errorf("%w: verdict %s does not decode", errMismatch, verify.InputRefs[i])
		}
		t = target{explanation: v.ExplanationRef, verdict: verify.InputRefs[i], explainAttempt: v.ExplainAttempt, rejected: true}
	}
	if t.verdict == "" {
		return target{}, fmt.Errorf("%w: finding %s failed verification but its verify step keeps no verdict", errMismatch, findingID)
	}
	var lastExpl string
	for i := len(explain.InputRefs) - 1; i >= 0 && lastExpl == ""; i-- {
		a, err := getNamed(ctx, st, "explanation", explain.InputRefs[i])
		if err != nil {
			return target{}, err
		}
		if a.Kind == store.ArtifactExplanation {
			lastExpl = explain.InputRefs[i]
		}
	}
	if lastExpl == "" || t.explanation != lastExpl {
		return target{}, fmt.Errorf("%w: finding %s: the last verdict %s names an explanation the explain step's last attempt did not produce", errMismatch, findingID, t.verdict)
	}
	if t.explainAttempt < 1 || t.explainAttempt >= explain.Attempt {
		return target{}, fmt.Errorf("%w: finding %s: the last verdict is for explain attempt %d, but the explain step is on attempt %d", errMismatch, findingID, t.explainAttempt, explain.Attempt)
	}
	return t, nil
}

// stateDiffs names the differences between a done explanation's verdict
// and the finding's status and steps. A failing verdict is final only on
// the last explain attempt: the finding needs review and the verify step
// is done with an error naming the violations. A passing verdict leaves
// the verify step done with no error.
func stateDiffs(f store.Finding, explain, verify store.Step, pass bool) []string {
	var diffs []string
	if verify.Status != store.StepDone {
		diffs = append(diffs, fmt.Sprintf("verify step is %s, not done", verify.Status))
	}
	if pass {
		if verify.Error != nil {
			diffs = append(diffs, "verify step has an error but the verdict passed")
		}
		return diffs
	}
	if f.Status != checks.StatusNeedsReview {
		diffs = append(diffs, fmt.Sprintf("status is %.20q but the final verdict failed", f.Status))
	}
	if verify.Error == nil {
		diffs = append(diffs, "verify step has no error but the verdict failed")
	}
	if explain.Attempt < agent.MaxExplainAttempts {
		diffs = append(diffs, fmt.Sprintf("the verdict failed on explain attempt %d of %d, yet it is final", explain.Attempt, agent.MaxExplainAttempts))
	}
	return diffs
}

// rejectedDiffs names the differences between a finding whose last
// explain retry ended without an explanation and what the workflow leaves:
// a failed verdict, the finding in needs_review with its rejected
// explanation cleared, and the verify step skipped.
func rejectedDiffs(f store.Finding, verify store.Step, pass bool) []string {
	var diffs []string
	if pass {
		diffs = append(diffs, "the last verdict passed, yet the explanation was retried")
	}
	if f.Status != checks.StatusNeedsReview {
		diffs = append(diffs, fmt.Sprintf("status is %.20q but the retry ended without an explanation", f.Status))
	}
	if f.Verified {
		diffs = append(diffs, "verified is true without an explanation")
	}
	if f.Explanation != nil || f.Action != nil || f.Proposal != nil || len(f.Citations) > 0 {
		diffs = append(diffs, "the finding shows an explanation the verifier rejected")
	}
	if verify.Status != store.StepSkipped {
		diffs = append(diffs, fmt.Sprintf("verify step is %s, not skipped", verify.Status))
	}
	return diffs
}

// rowDiffs names the findings-row columns that differ from the stored
// records: verified against the verdict's pass, explanation, action,
// citations and proposal against the explanation artifact, and, for a
// passing explanation that asked for review (needs_review), the status.
// Nil and empty citations are equal.
func rowDiffs(f store.Finding, explanation []byte, pass bool) ([]string, error) {
	var art agent.ExplanationArtifact
	if err := json.Unmarshal(explanation, &art); err != nil {
		return nil, fmt.Errorf("audit: decode explanation: %w", err)
	}
	var diffs []string
	if f.Verified != pass {
		diffs = append(diffs, fmt.Sprintf("verified is %v but the verdict's pass is %v", f.Verified, pass))
	}
	if pass && art.NeedsReview && f.Status != checks.StatusNeedsReview {
		diffs = append(diffs, fmt.Sprintf("status is %.20q but the passing explanation asked for review", f.Status))
	}
	if f.Explanation == nil || *f.Explanation != art.Explanation {
		diffs = append(diffs, "explanation")
	}
	if f.Action == nil || *f.Action != art.SuggestedAction {
		diffs = append(diffs, "action")
	}
	if len(f.Citations) != len(art.Citations) || (len(f.Citations) > 0 && !slices.Equal(f.Citations, art.Citations)) {
		diffs = append(diffs, "citations")
	}
	switch {
	case (f.Proposal == nil) != (art.Proposal == nil):
		diffs = append(diffs, "proposal")
	case f.Proposal != nil:
		got, err1 := json.Marshal(f.Proposal.Payload)
		want, err2 := json.Marshal(art.Proposal)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("audit: encode proposal: %w", err)
		}
		if !bytes.Equal(got, want) {
			diffs = append(diffs, "proposal")
		}
	}
	return diffs, nil
}

// readOnly lets the verifier read artifacts and refuses any write.
type readOnly struct {
	agent.ArtifactGetter
}

func (readOnly) PutArtifact(context.Context, string, uuid.UUID, uuid.UUID, any) (string, error) {
	return "", errors.New("audit: read-only store")
}

// indent pretty-prints JSON; anything else is returned as is.
func indent(b []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return string(b)
	}
	return out.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
