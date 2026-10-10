package agent

// The verifier (CC-705). It rejects an explanation that states a number,
// citation or journal entry not grounded in the evidence. It is plain Go,
// not a model call: it never uses VerifyRequest.Model, and it reads only
// stored artifacts (GetArtifact verifies each hash), so cmd/audit can run
// it again offline and get the same verdict byte for byte.
//
// What it checks:
//
//   - Provenance and scope: exactly one explanation ref, of kind
//     explanation, whose run_id and finding_id are the request's; every
//     evidence ref a tool_result whose company, date range, period and
//     history window (where its args carry them) lie within the run's
//     company and month. A missing artifact, a hash mismatch or a wrong
//     kind is an error (the step fails), not a violation.
//   - The suggested action is one of SuggestedActions.
//   - Amounts: every amount in the explanation, the action note and the
//     proposal's remark, every cited_amounts_paise value, and every
//     non-zero debit or credit of a proposal line must be within ₹1
//     (inclusive) of an evidence amount, of the sum or absolute difference
//     of two evidence amounts that are not balance-type (isBalanceKey), or
//     of the finding's amount, on absolute values. Proposal lines and the
//     proposal's total use only non-balance amounts: an entry may never
//     post a balance. A proposal's total debit must be within ₹1 of a
//     single non-balance evidence amount or of the finding's amount.
//     Zero-valued leaves ground nothing. Evidence amounts are the
//     money-valued leaves of exactly the records the model saw in EVIDENCE
//     (loadEvidence).
//   - Citations: each (doc_id, section) must have been in the DOCUMENTS the
//     model was given (the explanation's document_refs) and must exist in
//     doc_chunks for the company or as a shared passage.
//   - Proposal: balanced, each line one-sided, at least two lines; every
//     account in the account snapshot the model saw (accounts_ref, checked
//     for company and month); posting_date in the run's month or the next.
//
// The verdict is stored as a verdict artifact with sorted violations and
// no timestamps, so the same artifacts always give the same address. The
// verify step ends done whether the verdict passes or fails; the workflow,
// not the verifier, decides whether to retry. Violations name a fixed code,
// our field path, at most the offending amount (or token, or doc_id and
// section) and fixed text: never evidence, document or model text, since
// they go back into a prompt and into run_steps.feedback.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// VerifierVersion is recorded in every verdict. Change it when a rule
// changes, so an old verdict is not compared with a new rule.
const VerifierVersion = "cc-705.3"

// Violation codes: a closed set.
const (
	ViolationAmountNotInEvidence         = "amount_not_in_evidence"
	ViolationAmountUnparseable           = "amount_unparseable"
	ViolationCitedAmountNotInEvidence    = "cited_amount_not_in_evidence"
	ViolationProposalAmountNotInEvidence = "proposal_amount_not_in_evidence"
	ViolationProposalUnbalanced          = "proposal_unbalanced"
	ViolationProposalUnknownAccount      = "proposal_unknown_account"
	ViolationProposalDateOutOfWindow     = "proposal_date_out_of_window"
	ViolationAccountsUnverifiable        = "accounts_unverifiable"
	ViolationActionNotAllowed            = "action_not_allowed"
	ViolationCitationNotSupplied         = "citation_not_supplied"
	ViolationCitationUnknown             = "citation_unknown"
	ViolationScopeMismatch               = "scope_mismatch"
	ViolationEvidenceTruncated           = "evidence_truncated"
)

// violationDetail is the fixed text of each code.
var violationDetail = map[string]string{
	ViolationAmountNotInEvidence:         "amount is not in EVIDENCE, nor the sum or difference of two EVIDENCE amounts, within ₹1",
	ViolationAmountUnparseable:           "amount is not exact rupees: use digits with at most two decimals, such as ₹1,234.56",
	ViolationCitedAmountNotInEvidence:    "cited amount is not in EVIDENCE, nor the sum or difference of two EVIDENCE amounts, within 100 paise",
	ViolationProposalAmountNotInEvidence: "proposal amount is not grounded in EVIDENCE within 100 paise",
	ViolationProposalUnbalanced:          "proposal needs at least two lines, each with exactly one of debit or credit above zero, and debits equal to credits",
	ViolationProposalUnknownAccount:      "proposal account is not in ACCOUNTS",
	ViolationProposalDateOutOfWindow:     "posting_date must be a YYYY-MM-DD date in the finding's month or the next month",
	ViolationAccountsUnverifiable:        "the proposal's accounts can't be checked: no account snapshot was recorded",
	ViolationActionNotAllowed:            "suggested_action is not one of the allowed values",
	ViolationCitationNotSupplied:         "citation is not a passage from DOCUMENTS",
	ViolationCitationUnknown:             "citation is not a known passage for this company",
	ViolationScopeMismatch:               "artifact belongs to another run, finding, company or month",
	ViolationEvidenceTruncated:           "too many EVIDENCE amounts to check sums and differences; only single amounts were checked",
}

// Extra details of an ungrounded proposal amount: a line, and the entry's
// total.
const (
	detailProposalLine  = "a line must be a non-balance EVIDENCE amount, the sum or difference of two of them, or the finding's amount"
	detailProposalTotal = "the entry's total debit must equal a single non-balance EVIDENCE amount or the finding's amount"
)

// Limits of verdicts and feedback.
const (
	// maxFeedbackViolations caps the violations handed back to the
	// explainer (the verdict keeps all of them).
	maxFeedbackViolations = 40
	// maxCitationRunes caps a doc_id or section quoted in a violation.
	maxCitationRunes = 64
	// maxHistoryMonths bounds a snapshot's history window.
	maxHistoryMonths = 24
)

// Violation is one reason an explanation failed verification.
type Violation struct {
	Code       string       `json:"code"`
	Field      string       `json:"field"`
	ValuePaise *money.Paise `json:"value_paise,omitempty"`
	Detail     string       `json:"detail"`
}

// VerdictChecked counts what a verdict checked.
type VerdictChecked struct {
	Amounts       int `json:"amounts"`
	Citations     int `json:"citations"`
	ProposalLines int `json:"proposal_lines"`
}

// VerdictArtifact is the content of a verdict artifact.
type VerdictArtifact struct {
	RunID           string         `json:"run_id"`
	FindingID       string         `json:"finding_id"`
	ExplanationRef  string         `json:"explanation_ref"`
	ExplainAttempt  int            `json:"explain_attempt"`
	Pass            bool           `json:"pass"`
	Violations      []Violation    `json:"violations"`
	Checked         VerdictChecked `json:"checked"`
	VerifierVersion string         `json:"verifier_version"`
}

// VerifierFeedback is the VERIFIER_FEEDBACK contract: what a failed
// verify step hands to the next explain attempt (StepResult.Violations,
// run_steps.feedback).
type VerifierFeedback struct {
	Violations []Violation `json:"violations"`
}

// ArtifactGetter reads a stored artifact and verifies its hash.
// *store.Store implements it.
type ArtifactGetter interface {
	GetArtifact(ctx context.Context, sha string) (store.Artifact, error)
}

// VerifyStore is what the verifier reads and writes. *store.Store
// implements it.
type VerifyStore interface {
	ArtifactPutter
	ArtifactGetter
}

var _ VerifyStore = (*store.Store)(nil)

// CitationLookup reports whether a cited passage exists for a company
// (its own or shared). *store.Store implements it.
type CitationLookup interface {
	CitationExists(ctx context.Context, company, docID, section string) (bool, error)
}

var _ CitationLookup = (*store.Store)(nil)

// CodeVerifier is the Verifier in plain Go.
type CodeVerifier struct {
	Store VerifyStore
	// Citations checks cited passages against doc_chunks. It may be nil
	// only while no explanation cites anything.
	Citations CitationLookup
}

var _ Verifier = (*CodeVerifier)(nil)

// Verify checks one explanation, stores the verdict and returns the step's
// result: done with the verdict's address, and for a failed verdict the
// violations (VerifierFeedback JSON) and a reason naming their codes. A
// passing verdict carries the explanation's own needs_review flag.
func (v *CodeVerifier) Verify(ctx context.Context, req VerifyRequest) (StepResult, error) {
	verdict, needsReview, err := v.evaluate(ctx, req)
	if err != nil {
		return StepResult{}, err
	}
	sha, err := v.Store.PutArtifact(ctx, store.ArtifactVerdict, req.RunID, req.StepID, verdict)
	if err != nil {
		return StepResult{}, err
	}
	res := StepResult{StepID: req.StepID, Status: store.StepDone, OutputRefs: []string{sha}}
	if verdict.Pass {
		res.NeedsReview = needsReview
		return res, nil
	}
	b, err := feedbackFor(verdict.Violations)
	if err != nil {
		return StepResult{}, err
	}
	res.Violations = b
	res.Reason = failedReason(verdict.Violations)
	return res, nil
}

// Evaluate computes the verdict on one explanation without storing it.
// cmd/audit uses it to re-compute a stored verdict.
func (v *CodeVerifier) Evaluate(ctx context.Context, req VerifyRequest) (VerdictArtifact, error) {
	verdict, _, err := v.evaluate(ctx, req)
	return verdict, err
}

// feedbackFor encodes the VERIFIER_FEEDBACK for a failed verdict: at most
// maxFeedbackViolations violations, and fewer when needed to keep the JSON
// within store.MaxFeedbackBytes (what ReopenStep accepts), dropping from
// the end. The verdict keeps the full list.
func feedbackFor(vs []Violation) ([]byte, error) {
	fb := vs
	if len(fb) > maxFeedbackViolations {
		fb = fb[:maxFeedbackViolations]
	}
	for {
		b, err := json.Marshal(VerifierFeedback{Violations: fb})
		if err != nil {
			return nil, fmt.Errorf("agent: encode verifier feedback: %w", err)
		}
		if len(b) <= store.MaxFeedbackBytes || len(fb) == 0 {
			return b, nil
		}
		fb = fb[:len(fb)-1]
	}
}

// failedReason is a failed verdict's step reason: its distinct codes.
func failedReason(vs []Violation) string {
	var codes []string
	for _, x := range vs {
		if !slices.Contains(codes, x.Code) {
			codes = append(codes, x.Code)
		}
	}
	slices.Sort(codes)
	return "verification failed: " + strings.Join(codes, ", ")
}

// verdictBuilder collects violations and counts.
type verdictBuilder struct {
	violations []Violation
	checked    VerdictChecked
}

func (b *verdictBuilder) add(code, field string, value *money.Paise, extra string) {
	detail := violationDetail[code]
	if extra != "" {
		detail += ": " + extra
	}
	b.violations = append(b.violations, Violation{Code: code, Field: field, ValuePaise: value, Detail: detail})
}

// sorted returns the violations sorted and without duplicates.
func (b *verdictBuilder) sorted() []Violation {
	vs := slices.Clone(b.violations)
	cmpValue := func(a, b *money.Paise) int {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return -1
		case b == nil:
			return 1
		}
		return cmp.Compare(*a, *b)
	}
	order := func(a, b Violation) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.Field, b.Field), cmpValue(a.ValuePaise, b.ValuePaise), cmp.Compare(a.Detail, b.Detail))
	}
	slices.SortFunc(vs, order)
	vs = slices.CompactFunc(vs, func(a, b Violation) bool { return order(a, b) == 0 })
	if vs == nil {
		vs = []Violation{}
	}
	return vs
}

// scope is the run's company and month.
type scope struct {
	company  string
	month    string
	from, to time.Time // the month's first and last day
	index    int       // year*12 + month-1
}

func newScope(companyID, month string) (scope, error) {
	from, to, err := monthRange(month)
	if err != nil {
		return scope{}, err
	}
	return scope{company: companyID, month: month, from: from, to: to, index: monthIndex(from)}, nil
}

func monthIndex(t time.Time) int { return t.Year()*12 + int(t.Month()) - 1 }

// evaluate computes the verdict and returns the explanation's needs_review
// flag.
func (v *CodeVerifier) evaluate(ctx context.Context, req VerifyRequest) (VerdictArtifact, bool, error) {
	if v.Store == nil {
		return VerdictArtifact{}, false, errors.New("agent: verifier has no store")
	}
	sc, err := newScope(req.Company, req.Month)
	if err != nil {
		return VerdictArtifact{}, false, err
	}
	if len(req.ExplanationRefs) != 1 {
		return VerdictArtifact{}, false, fmt.Errorf("agent: verify: want exactly one explanation ref, got %d", len(req.ExplanationRefs))
	}
	explRef := req.ExplanationRefs[0]
	art, err := loadExplanation(ctx, v.Store, explRef)
	if err != nil {
		return VerdictArtifact{}, false, err
	}

	var b verdictBuilder
	if art.RunID != req.RunID.String() {
		b.add(ViolationScopeMismatch, "explanation.run_id", nil, "")
	}
	if art.FindingID != req.Finding.ID.String() {
		b.add(ViolationScopeMismatch, "explanation.finding_id", nil, "")
	}

	// Evidence: the records the model saw, checked for scope.
	var amounts evidenceAmounts
	for i, sha := range req.EvidenceRefs {
		view, snap, err := loadEvidence(ctx, v.Store, sha, req.Finding.Evidence)
		if err != nil {
			return VerdictArtifact{}, false, fmt.Errorf("agent: verify: %w", err)
		}
		for _, arg := range sc.outOfScope(snap.Args) {
			b.add(ViolationScopeMismatch, fmt.Sprintf("evidence[%d].args.%s", i, arg), nil, "")
		}
		for _, r := range view.Records {
			moneyLeaves(r, &amounts)
		}
	}
	gs := newGroundSet(amounts, req.Finding.AmountPaise)
	if !gs.pairs {
		b.add(ViolationEvidenceTruncated, "evidence", nil, "")
	}

	if !slices.Contains(SuggestedActions, art.SuggestedAction) {
		b.add(ViolationActionNotAllowed, "suggested_action", nil, "")
	}

	// Amounts in text.
	texts := []struct{ field, text string }{
		{"explanation", art.Explanation},
		{"action_note", art.ActionNote},
	}
	if art.Proposal != nil {
		texts = append(texts, struct{ field, text string }{"proposal.remark", art.Proposal.Remark})
	}
	for _, t := range texts {
		found, bad := extractAmounts(t.text)
		for _, tok := range bad {
			b.checked.Amounts++
			b.add(ViolationAmountUnparseable, t.field, nil, tok)
		}
		for _, a := range found {
			b.checked.Amounts++
			if !gs.grounded(a) {
				b.add(ViolationAmountNotInEvidence, t.field, paisePtr(a), "")
			}
		}
	}

	// Cited amounts.
	for i, a := range art.CitedAmountsPaise {
		b.checked.Amounts++
		if !gs.grounded(a) {
			b.add(ViolationCitedAmountNotInEvidence, fmt.Sprintf("cited_amounts_paise[%d]", i), paisePtr(a), "")
		}
	}

	if err := v.checkCitations(ctx, req.Company, art, &b); err != nil {
		return VerdictArtifact{}, false, err
	}
	if art.Proposal != nil {
		if err := v.checkProposal(ctx, sc, art, gs, &b); err != nil {
			return VerdictArtifact{}, false, err
		}
	}

	vs := b.sorted()
	return VerdictArtifact{
		RunID: req.RunID.String(), FindingID: req.Finding.ID.String(),
		ExplanationRef: explRef, ExplainAttempt: req.ExplainAttempt,
		Pass: len(vs) == 0, Violations: vs, Checked: b.checked,
		VerifierVersion: VerifierVersion,
	}, art.NeedsReview, nil
}

func paisePtr(p money.Paise) *money.Paise { return &p }

// loadExplanation reads and decodes an explanation artifact.
func loadExplanation(ctx context.Context, st ArtifactGetter, sha string) (ExplanationArtifact, error) {
	a, err := st.GetArtifact(ctx, sha)
	if err != nil {
		return ExplanationArtifact{}, fmt.Errorf("agent: verify explanation: %w", err)
	}
	if a.Kind != store.ArtifactExplanation {
		return ExplanationArtifact{}, fmt.Errorf("agent: verify: %s is a %s artifact, not an explanation", shortSHA(sha), a.Kind)
	}
	var art ExplanationArtifact
	if err := json.Unmarshal(a.Content, &art); err != nil {
		return ExplanationArtifact{}, fmt.Errorf("agent: verify: decode explanation %s: %w", shortSHA(sha), err)
	}
	return art, nil
}

// checkCitations checks every citation against the DOCUMENTS the model
// was given and against doc_chunks.
func (v *CodeVerifier) checkCitations(ctx context.Context, companyID string, art ExplanationArtifact, b *verdictBuilder) error {
	if len(art.Citations) == 0 {
		return nil
	}
	supplied := map[store.Citation]bool{}
	for _, sha := range art.DocumentRefs {
		a, err := v.Store.GetArtifact(ctx, sha)
		if err != nil {
			return fmt.Errorf("agent: verify documents: %w", err)
		}
		if a.Kind != store.ArtifactRetrieval {
			return fmt.Errorf("agent: verify: document ref %s is a %s artifact, not a retrieval", shortSHA(sha), a.Kind)
		}
		var content any
		if err := json.Unmarshal(a.Content, &content); err != nil {
			return fmt.Errorf("agent: verify: decode retrieval %s: %w", shortSHA(sha), err)
		}
		collectPassages(content, supplied)
	}
	for i, c := range art.Citations {
		b.checked.Citations++
		field := fmt.Sprintf("citations[%d]", i)
		quoted := capRunes(c.DocID, maxCitationRunes) + " §" + capRunes(c.Section, maxCitationRunes)
		if !supplied[c] {
			b.add(ViolationCitationNotSupplied, field, nil, quoted)
			continue
		}
		if v.Citations == nil {
			return errors.New("agent: verifier has no citation lookup")
		}
		ok, err := v.Citations.CitationExists(ctx, companyID, c.DocID, c.Section)
		if err != nil {
			return fmt.Errorf("agent: verify citation: %w", err)
		}
		if !ok {
			b.add(ViolationCitationUnknown, field, nil, quoted)
		}
	}
	return nil
}

// collectPassages adds every object with string doc_id and section fields
// in a retrieval artifact's content.
func collectPassages(v any, out map[store.Citation]bool) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collectPassages(e, out)
		}
	case map[string]any:
		doc, ok1 := x["doc_id"].(string)
		sec, ok2 := x["section"].(string)
		if ok1 && ok2 && doc != "" && sec != "" {
			out[store.Citation{DocID: doc, Section: sec}] = true
		}
		for _, e := range x {
			collectPassages(e, out)
		}
	}
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// checkProposal checks a proposed journal entry.
func (v *CodeVerifier) checkProposal(ctx context.Context, sc scope, art ExplanationArtifact, gs groundSet, b *verdictBuilder) error {
	p := art.Proposal
	b.checked.ProposalLines = len(p.Lines)

	// Balance: at least two lines, each one-sided, debits equal credits.
	balanced := len(p.Lines) >= 2
	var debits, credits money.Paise
	for _, l := range p.Lines {
		d, c := l.DebitPaise, l.CreditPaise
		if d < 0 || c < 0 || (d > 0) == (c > 0) || debits > maxPaise-d || credits > maxPaise-c {
			balanced = false
			continue
		}
		debits += d
		credits += c
	}
	if !balanced || debits != credits {
		b.add(ViolationProposalUnbalanced, "proposal.lines", nil, "")
	}
	// The entry's total must be one non-balance evidence amount or the
	// finding's amount: lines may split it (500 + 90 = 590), but the whole entry
	// can't be a derived number.
	if debits > 0 {
		b.checked.Amounts++
		if !gs.groundedProposalSingle(debits) {
			b.add(ViolationProposalAmountNotInEvidence, "proposal.total_debit", paisePtr(debits), detailProposalTotal)
		}
	}

	// Posting date: the run's month or the next one.
	date, err := time.Parse(time.DateOnly, p.PostingDate)
	if err != nil || date.Before(sc.from) || date.After(sc.from.AddDate(0, 2, -1)) {
		b.add(ViolationProposalDateOutOfWindow, "proposal.posting_date", nil, "")
	}

	// Accounts: from the snapshot the model saw.
	accounts, err := v.snapshotAccounts(ctx, sc, art.AccountsRef, b)
	if err != nil {
		return err
	}
	for i, l := range p.Lines {
		if accounts != nil && !accounts[l.Account] {
			b.add(ViolationProposalUnknownAccount, fmt.Sprintf("proposal.lines[%d].account", i), nil, "")
		}
		for _, side := range []struct {
			name  string
			value money.Paise
		}{{"debit", l.DebitPaise}, {"credit", l.CreditPaise}} {
			if side.value == 0 {
				continue
			}
			b.checked.Amounts++
			if !gs.groundedProposal(side.value) {
				b.add(ViolationProposalAmountNotInEvidence, fmt.Sprintf("proposal.lines[%d].%s", i, side.name), paisePtr(side.value), detailProposalLine)
			}
		}
	}
	return nil
}

// snapshotAccounts rebuilds the account set from the explanation's
// accounts snapshot. With no snapshot it adds accounts_unverifiable and
// returns nil (every account is then unchecked, and the verdict fails). A
// snapshot of another company or month adds scope_mismatch and returns an
// empty set, so every account is also unknown.
func (v *CodeVerifier) snapshotAccounts(ctx context.Context, sc scope, ref string, b *verdictBuilder) (map[string]bool, error) {
	if ref == "" {
		b.add(ViolationAccountsUnverifiable, "accounts_ref", nil, "")
		return nil, nil
	}
	a, err := v.Store.GetArtifact(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("agent: verify accounts: %w", err)
	}
	if a.Kind != store.ArtifactToolResult {
		return nil, fmt.Errorf("agent: verify: accounts ref %s is a %s artifact, not a tool result", shortSHA(ref), a.Kind)
	}
	snap, err := decodeSnapshot(a.Content)
	if err != nil {
		return nil, fmt.Errorf("agent: verify: decode accounts %s: %w", shortSHA(ref), err)
	}
	if snap.Server != ServerBooks || snap.Tool != toolGetTrialBalance {
		return nil, fmt.Errorf("agent: verify: accounts ref %s is not a trial balance snapshot", shortSHA(ref))
	}
	out := map[string]bool{}
	bad := sc.outOfScope(snap.Args)
	if _, ok := snap.Args["company"]; !ok {
		bad = append(bad, "company")
	}
	if len(bad) > 0 {
		for _, arg := range bad {
			b.add(ViolationScopeMismatch, "accounts.args."+arg, nil, "")
		}
		return out, nil
	}
	return snapshotAccountSet(snap), nil
}

// snapshotAccountSet is the set of account names in a trial balance
// snapshot's rows.
func snapshotAccountSet(snap snapshotContent) map[string]bool {
	out := map[string]bool{}
	for _, r := range selectRecords(snap.Result, nil) {
		if m, ok := r.(map[string]any); ok {
			if name, ok := m["account"].(string); ok && name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// outOfScope returns the names of the snapshot args that lie outside the
// run's company and month, sorted. Args a snapshot lacks are not checked.
//
//   - company must equal the run's company.
//   - from_date and to_date (or from and to) must lie within the month.
//   - period and month must equal the month.
//   - through_month (with months) and before_month (with lookback_months)
//     open a history window: the month itself or one of the
//     maxHistoryMonths before it (before_month may be the next month), and
//     a window length of 1 to maxHistoryMonths.
func (sc scope) outOfScope(args map[string]any) []string {
	var bad []string
	for k, val := range args {
		s, isStr := val.(string)
		ok := true
		switch k {
		case "company":
			ok = isStr && s == sc.company
		case "from_date", "to_date", "from", "to":
			if isStr && s == "" {
				break // an open bound the reader didn't set
			}
			d, err := time.Parse(time.DateOnly, s)
			ok = isStr && err == nil && !d.Before(sc.from) && !d.After(sc.to)
		case "period", "month":
			ok = isStr && s == sc.month
		case "through_month":
			ok = isStr && sc.monthWithin(s, 0)
		case "before_month":
			ok = isStr && sc.monthWithin(s, 1)
		case "months", "lookback_months":
			n, isNum := val.(json.Number)
			i, err := n.Int64()
			ok = isNum && err == nil && i >= 1 && i <= maxHistoryMonths
		}
		if !ok {
			bad = append(bad, k)
		}
	}
	slices.Sort(bad)
	return bad
}

// monthWithin reports whether a YYYY-MM month lies from maxHistoryMonths
// before the run's month to ahead months after it.
func (sc scope) monthWithin(month string, ahead int) bool {
	t, err := company.ParseMonth(month)
	if err != nil {
		return false
	}
	i := monthIndex(t)
	return i >= sc.index-maxHistoryMonths && i <= sc.index+ahead
}

// EvidenceRefs are the distinct snapshot addresses of a finding's
// evidence, in order: what the workflow passes to the explainer and the
// verifier.
func EvidenceRefs(f store.Finding) []string { return evidenceRefs(f) }

// ProjectEvidence returns the EVIDENCE section the explainer built for a
// finding, as JSON, from the stored snapshots (cmd/audit).
func ProjectEvidence(ctx context.Context, st ArtifactGetter, f store.Finding) (json.RawMessage, error) {
	views := []evidenceView{}
	for _, sha := range evidenceRefs(f) {
		view, _, err := loadEvidence(ctx, st, sha, f.Evidence)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	b, err := json.Marshal(views)
	if err != nil {
		return nil, fmt.Errorf("agent: encode evidence: %w", err)
	}
	return b, nil
}
