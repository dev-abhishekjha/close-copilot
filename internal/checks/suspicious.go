package checks

// Prompt-injection detection (CC-1103). Text inside the data (invoice
// remarks, item descriptions, payment and journal remarks, bank
// narrations) is written by suppliers, customers, banks and clerks, never
// by the operator, so it may carry instructions aimed at the agent. The
// defence has four layers:
//
//   - structural: the agent has no write tools; the only write path needs a
//     human checker (internal/approvals);
//   - prompt: untrusted text reaches a model fenced and labelled as data
//     (internal/agent: the explainer's <<<NAME sections, the investigator's
//     <<<NARRATION fence and dropped remarks);
//   - detection: this check flags instruction-like text as a
//     suspicious_instruction_text finding, and the workflow keeps every
//     finding on the same record away from the models (no explanation, no
//     investigation, no proposal; needs_review);
//   - test: planted injections in the eval, with unauthorized writes at 0.
//
// The check is deterministic and cheap: a handful of case-insensitive
// patterns over normalised text. A finding's title names the record and
// the field, never the matched text, because titles reach the UI and the
// prompts of other steps.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// suspiciousPatterns are the instruction-like shapes, compiled once. They
// run over normaliseUntrusted's output.
var suspiciousPatterns = []*regexp.Regexp{
	// "ignore all previous instructions", "disregard prior rules".
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\s+(?:(?:all|any|every|previous|prior|above|earlier|preceding|your|these|those)\s+){1,3}(?:instructions?|rules?|prompts?|guidelines?)\b`),
	// A fake role line: "system:" or "assistant:" at a line start or after
	// a separator, never mid-phrase ("ERP system: SAP" and "Payroll -
	// System: auto" do not match).
	regexp.MustCompile(`(?im)(?:^|[.;|/>\]})"'])\s*(?:system|assistant)\s*:`),
	// A role marker mid-line that goes on to address the reader:
	// "NEFT CHGS SYSTEM: the operator ...", "system: you are ...".
	regexp.MustCompile(`(?i)\b(?:system|assistant)\s*:\s*(?:you\b|the\s+(?:operator|user|agent|assistant|model)\b|ignore\b|disregard\b|approve\b|post\b|override\b|new\s+instructions?\b)`),
	// Role tags such as <system> or </assistant>.
	regexp.MustCompile(`(?i)<\s*/?\s*(?:system|assistant|instructions?)\s*>`),
	// "approve all", "approve every", "approve pending", "approve this".
	regexp.MustCompile(`(?i)\bapprove\s+(?:all|every|pending|this)\b`),
	// Imperatives aimed at an AI, within a few words: "AI assistants must
	// ...", "LLM, please ...".
	regexp.MustCompile(`(?i)\b(?:ai|llms?|copilot|assistants?)\b(?:\W+\w+){0,3}?\W+` + imperatives),
	// Imperatives aimed at an addressed agent or model: "the agent must",
	// "Dear agent, please", "Agent, skip ...". An agent or model named in
	// passing ("commission agent should collect TDS", "clearing agent,
	// please see bill", "laptop model 5420 should be capitalised") is not
	// addressed.
	regexp.MustCompile(`(?im)(?:\b(?:the|dear|hey|attention)\s+(?:agents?|models?)\b|(?:^|[.;:!?])\s*(?:agents?|models?)\s*,)(?:\W+\w+){0,3}?\W+` + imperatives),
	// "without review", "without any human review".
	regexp.MustCompile(`(?i)\bwithout\s+(?:(?:any|further|human|manual)\s+){0,2}review\b`),
	// "do not flag".
	regexp.MustCompile(`(?i)\b(?:do\s+not|don'?t)\s+flag\b`),
}

// imperatives are the verbs an instruction to the agent uses.
const imperatives = `(?:must|should|please|approve|ignore|call|post|skip|do\s+not\s+flag|don'?t\s+flag)\b`

// maxTitleIDRunes caps the record ID in a finding's title.
const maxTitleIDRunes = 40

// titleID is id cut to maxTitleIDRunes runes.
func titleID(id string) string {
	if r := []rune(id); len(r) > maxTitleIDRunes {
		return string(r[:maxTitleIDRunes]) + "..."
	}
	return id
}

// zeroWidth are format characters that render as nothing; they are removed
// so a phrase split by them still matches.
var zeroWidth = map[rune]bool{
	0x00ad: true,                             // soft hyphen
	0x180e: true,                             // Mongolian vowel separator
	0x200b: true, 0x200c: true, 0x200d: true, // zero-width space, non-joiner, joiner
	0x2060: true, // word joiner
	0xfeff: true, // zero-width no-break space
}

// normaliseUntrusted strips zero-width characters, turns every other space
// or control character into a plain space (keeping line breaks) and
// collapses runs of spaces.
func normaliseUntrusted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ReplaceAll(s, "\r\n", "\n") {
		if zeroWidth[r] {
			continue
		}
		if r == '\r' {
			r = '\n'
		}
		if r != '\n' && (unicode.IsSpace(r) || unicode.IsControl(r)) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// looksLikeInstruction reports whether any of texts holds instruction-like
// text.
func looksLikeInstruction(texts ...string) bool {
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		n := normaliseUntrusted(t)
		for _, re := range suspiciousPatterns {
			if re.MatchString(n) {
				return true
			}
		}
	}
	return false
}

// SuspiciousTextCheck flags records whose free text reads like an
// instruction to the agent: purchase and sales invoice remarks and item
// descriptions, payment entry remarks, journal entry remarks (from the GL)
// and bank narrations of the month. It raises one
// suspicious_instruction_text finding per record, keyed invoice (the
// voucher name) or bank_txn_id.
type SuspiciousTextCheck struct{}

var _ Check = (*SuspiciousTextCheck)(nil)

// Name returns the identifier for this check.
func (s *SuspiciousTextCheck) Name() string { return "suspicious_text" }

// Run scans the month's free-text fields.
func (s *SuspiciousTextCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	first, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: suspicious_text invalid month %q: %w", in.Month, err)
	}
	last := first.AddDate(0, 1, -1)
	args := map[string]string{"company": in.Company, "from_date": first.Format("2006-01-02"), "to_date": last.Format("2006-01-02")}

	var findings []Finding
	add := func(key, id, what string, ev EvidenceRef) {
		// The title names the record (cut to maxTitleIDRunes), never the
		// matched text.
		act := ActionInvestigate
		findings = append(findings, Finding{
			Type:     TypeSuspiciousInstruction,
			Severity: SeverityMedium,
			Title:    fmt.Sprintf("Instruction-like text in %s %s; review by hand", what, titleID(id)),
			Keys:     map[string]string{key: id},
			Evidence: []EvidenceRef{ev},
			Action:   &act,
			Status:   StatusOpen,
		})
	}

	pis, err := in.Books.PurchaseInvoices(ctx, in.Company, first, last)
	if err != nil {
		return nil, fmt.Errorf("checks: suspicious_text load purchase invoices: %w", err)
	}
	for _, inv := range pis {
		texts := []string{inv.Remarks}
		for _, it := range inv.Items {
			texts = append(texts, it.Description)
		}
		if looksLikeInstruction(texts...) {
			add("invoice", inv.Name, "purchase invoice", BooksEvidence("list_purchase_invoices", args, inv.Name))
		}
	}

	sis, err := in.Books.SalesInvoices(ctx, in.Company, first, last)
	if err != nil {
		return nil, fmt.Errorf("checks: suspicious_text load sales invoices: %w", err)
	}
	for _, inv := range sis {
		texts := []string{inv.Remarks}
		for _, it := range inv.Items {
			texts = append(texts, it.Description)
		}
		if looksLikeInstruction(texts...) {
			add("invoice", inv.Name, "sales invoice", BooksEvidence("list_sales_invoices", args, inv.Name))
		}
	}

	pes, err := in.Books.PaymentEntries(ctx, in.Company, first, last)
	if err != nil {
		return nil, fmt.Errorf("checks: suspicious_text load payment entries: %w", err)
	}
	for _, pe := range pes {
		if looksLikeInstruction(pe.Remarks) {
			add("invoice", pe.Name, "payment entry", BooksEvidence("list_payments", args, pe.Name))
		}
	}

	// Journal entries reach the checks only as GL lines; each line carries
	// the voucher's remark. The evidence names the voucher and every one of
	// its GL lines, so a finding that cites any of those lines (variance,
	// bankrec's unmatched ledger entries) is tainted with it.
	gl, err := in.Books.GLEntries(ctx, in.Company, first, last)
	if err != nil {
		return nil, fmt.Errorf("checks: suspicious_text load gl entries: %w", err)
	}
	type journal struct {
		lines []string
		hit   bool
	}
	jes := map[string]*journal{}
	var order []string
	for _, e := range gl {
		if e.VoucherType != "Journal Entry" || e.VoucherNo == "" {
			continue
		}
		j := jes[e.VoucherNo]
		if j == nil {
			j = &journal{}
			jes[e.VoucherNo] = j
			order = append(order, e.VoucherNo)
		}
		if e.Name != "" && !slices.Contains(j.lines, e.Name) {
			j.lines = append(j.lines, e.Name)
		}
		j.hit = j.hit || looksLikeInstruction(e.Remarks)
	}
	for _, v := range order {
		if j := jes[v]; j.hit {
			add("invoice", v, "journal entry", BooksEvidence("list_gl_entries", args, append([]string{v}, j.lines...)...))
		}
	}

	if in.Evidence != nil {
		lines, err := in.Evidence.BankLines(ctx, in.Company, first, last)
		if err != nil {
			return nil, fmt.Errorf("checks: suspicious_text load bank lines: %w", err)
		}
		for _, l := range lines {
			if l.TxnID != "" && looksLikeInstruction(l.Narration) {
				add("bank_txn_id", l.TxnID, "bank line", EvidenceEvidence("list_bank_lines", args, l.TxnID))
			}
		}
	}

	slices.SortStableFunc(findings, func(a, b Finding) int { return strings.Compare(DedupeKey(a), DedupeKey(b)) })
	return findings, nil
}
