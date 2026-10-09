package checks

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

var (
	// bankChargeRegex matches narrations that typically indicate bank charges/fees.
	bankChargeRegex = regexp.MustCompile(`(?i)CHARGES|CHGS|SMS|FEE|COMMISSION`)

	// maxBankChargePaise is ₹10,000 in paise (1,000,000 paise).
	maxBankChargePaise = money.Paise(1000000)
)

// BankVoucher represents the net effect of a voucher on the company's bank account.
type BankVoucher struct {
	VoucherNo   string
	VoucherType string
	PostingDate time.Time
	Net         money.Paise // debit - credit (positive = deposit, negative = withdrawal)
	ReferenceNo string
	Remarks     string
	Party       string
	EntryIDs    []string
}

// BankMatch represents a successfully reconciled pair between a bank statement line and a book voucher.
type BankMatch struct {
	BankLine store.BankLine
	Voucher  BankVoucher
	Pass     int // 1 for exact reference, 2 for date-window
}

// BankRecResult holds the matched pairs and leftovers from reconciliation.
type BankRecResult struct {
	Matched           []BankMatch
	UnmatchedBank     []store.BankLine
	UnmatchedVouchers []BankVoucher
}

// BankRecCheck implements the deterministic Check interface for bank reconciliation.
type BankRecCheck struct{}

var _ Check = (*BankRecCheck)(nil)

// Name returns the identifier for this check.
func (b *BankRecCheck) Name() string {
	return "bankrec"
}

// Run executes bank reconciliation for the specified company and month.
func (b *BankRecCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	startMonth, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: bankrec invalid month %q: %w", in.Month, err)
	}
	endMonth := startMonth.AddDate(0, 1, -1)

	// Load bank statement lines
	bankLines, err := in.Evidence.BankLines(ctx, in.Company, startMonth, endMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: bankrec load bank lines: %w", err)
	}

	// Load GL entries
	glEntries, err := in.Books.GLEntries(ctx, in.Company, startMonth, endMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: bankrec load gl entries: %w", err)
	}

	// Filter to bank account GL entries
	bankGLEntries := filterBankGLEntries(glEntries)

	// Build vouchers map from GL entries
	vouchersMap := make(map[string]*BankVoucher)
	for _, e := range bankGLEntries {
		v := vouchersMap[e.VoucherNo]
		if v == nil {
			v = &BankVoucher{
				VoucherNo:   e.VoucherNo,
				VoucherType: e.VoucherType,
				PostingDate: e.PostingDate,
				Remarks:     e.Remarks,
				Party:       e.Party,
			}
			vouchersMap[e.VoucherNo] = v
		}
		v.Net += e.Debit - e.Credit
		v.EntryIDs = append(v.EntryIDs, e.Name)
	}

	// Enrich with Payment Entry references if available
	payments, err := in.Books.PaymentEntries(ctx, in.Company, startMonth, endMonth)
	if err == nil {
		for _, p := range payments {
			if v, ok := vouchersMap[p.Name]; ok {
				if p.ReferenceNo != "" {
					v.ReferenceNo = p.ReferenceNo
				}
				if v.Party == "" && p.Party != "" {
					v.Party = p.Party
				}
			}
		}
	}

	vouchers := make([]BankVoucher, 0, len(vouchersMap))
	for _, v := range vouchersMap {
		vouchers = append(vouchers, *v)
	}

	windowDays := in.Rules.BankMatch.DateWindowDays
	if windowDays <= 0 {
		windowDays = 3
	}

	recResult := MatchBankLines(bankLines, vouchers, windowDays)

	return classifyFindings(recResult, in.Company), nil
}

// MatchBankLines reconciles bank statement lines with book vouchers using two passes:
// Pass 1: exact amount and matching reference.
// Pass 2: exact amount within date window, tie-broken by closest date and narration similarity.
func MatchBankLines(bankLines []store.BankLine, vouchers []BankVoucher, windowDays int) BankRecResult {
	// Index vouchers by amount for linear lookups
	byAmount := make(map[money.Paise][]*BankVoucher, len(vouchers))
	for i := range vouchers {
		v := &vouchers[i]
		byAmount[v.Net] = append(byAmount[v.Net], v)
	}

	matchedVouchers := make(map[string]bool)
	matchedBank := make(map[string]bool)
	var matches []BankMatch

	// Pass 1: exact amount and matching reference
	for _, line := range bankLines {
		if line.Ref == nil || *line.Ref == "" {
			continue
		}
		bRef := strings.TrimSpace(*line.Ref)
		candidates := byAmount[line.AmountPaise]

		for _, v := range candidates {
			if matchedVouchers[v.VoucherNo] {
				continue
			}
			if matchesReference(bRef, v) {
				matchedVouchers[v.VoucherNo] = true
				matchedBank[line.TxnID] = true
				matches = append(matches, BankMatch{
					BankLine: line,
					Voucher:  *v,
					Pass:     1,
				})
				break
			}
		}
	}

	// Pass 2: exact amount within date window
	for _, line := range bankLines {
		if matchedBank[line.TxnID] {
			continue
		}

		candidates := byAmount[line.AmountPaise]
		var bestVoucher *BankVoucher
		var bestDiffDays int
		var bestSimScore int

		for _, v := range candidates {
			if matchedVouchers[v.VoucherNo] {
				continue
			}

			diffDays := daysDiff(line.TxnDate, v.PostingDate)
			if diffDays > windowDays {
				continue
			}

			sim := jaroWinklerScore(line.Narration, v.Party+" "+v.Remarks)

			if bestVoucher == nil {
				bestVoucher = v
				bestDiffDays = diffDays
				bestSimScore = sim
				continue
			}

			// Tie-breaker 1: closest date
			if diffDays < bestDiffDays {
				bestVoucher = v
				bestDiffDays = diffDays
				bestSimScore = sim
			} else if diffDays == bestDiffDays {
				// Tie-breaker 2: highest narration similarity
				if sim > bestSimScore {
					bestVoucher = v
					bestDiffDays = diffDays
					bestSimScore = sim
				}
			}
		}

		if bestVoucher != nil {
			matchedVouchers[bestVoucher.VoucherNo] = true
			matchedBank[line.TxnID] = true
			matches = append(matches, BankMatch{
				BankLine: line,
				Voucher:  *bestVoucher,
				Pass:     2,
			})
		}
	}

	// Collect leftovers
	var unmatchedBank []store.BankLine
	for _, line := range bankLines {
		if !matchedBank[line.TxnID] {
			unmatchedBank = append(unmatchedBank, line)
		}
	}

	var unmatchedVouchers []BankVoucher
	for _, v := range vouchers {
		if !matchedVouchers[v.VoucherNo] {
			unmatchedVouchers = append(unmatchedVouchers, v)
		}
	}

	return BankRecResult{
		Matched:           matches,
		UnmatchedBank:     unmatchedBank,
		UnmatchedVouchers: unmatchedVouchers,
	}
}

func matchesReference(bankRef string, v *BankVoucher) bool {
	if bankRef == "" {
		return false
	}
	if v.ReferenceNo != "" && strings.EqualFold(bankRef, v.ReferenceNo) {
		return true
	}
	if strings.EqualFold(bankRef, v.VoucherNo) {
		return true
	}
	if v.Remarks != "" && strings.Contains(strings.ToLower(v.Remarks), strings.ToLower(bankRef)) {
		return true
	}
	return false
}

func classifyFindings(res BankRecResult, company string) []Finding {
	var findings []Finding

	actBook := ActionBookEntry
	actInvestigate := ActionInvestigate

	// Classify unmatched bank lines
	for _, line := range res.UnmatchedBank {
		absAmount := line.AmountPaise
		if absAmount < 0 {
			absAmount = -absAmount
		}

		// Check for unrecorded bank charge: withdrawal under ₹10,000 with charge narration
		if line.AmountPaise < 0 && absAmount <= maxBankChargePaise && bankChargeRegex.MatchString(line.Narration) {
			evidenceArgs := map[string]string{
				"company": company,
				"txn_id":  line.TxnID,
			}
			f := Finding{
				Type:        TypeUnrecordedBankCharge,
				Severity:    SeverityMedium,
				Title:       fmt.Sprintf("Unrecorded bank charge: %s (%s)", line.Narration, absAmount.Format()),
				AmountPaise: &absAmount,
				Keys: map[string]string{
					"bank_txn_id": line.TxnID,
				},
				Evidence: []EvidenceRef{
					EvidenceEvidence("list_bank_lines", evidenceArgs, line.TxnID),
				},
				Action: &actBook,
				Status: StatusOpen,
			}
			findings = append(findings, f)
			continue
		}

		// Other unmatched lines become unmatched_bank_line
		keys := map[string]string{
			"bank_txn_id": line.TxnID,
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line.Narration)), "PG SETTL") {
			keys["hint"] = "gateway"
		}

		evidenceArgs := map[string]string{
			"company": company,
			"txn_id":  line.TxnID,
		}
		f := Finding{
			Type:        TypeUnmatchedBankLine,
			Severity:    SeverityMedium,
			Title:       fmt.Sprintf("Unmatched bank line: %s (%s)", line.Narration, absAmount.Format()),
			AmountPaise: &absAmount,
			Keys:        keys,
			Evidence: []EvidenceRef{
				EvidenceEvidence("list_bank_lines", evidenceArgs, line.TxnID),
			},
			Action: &actInvestigate,
			Status: StatusOpen,
		}
		findings = append(findings, f)
	}

	// Classify unmatched book entries
	for _, v := range res.UnmatchedVouchers {
		absAmount := v.Net
		if absAmount < 0 {
			absAmount = -absAmount
		}

		evidenceArgs := map[string]string{
			"company":    company,
			"voucher_no": v.VoucherNo,
		}
		f := Finding{
			Type:        TypeUnmatchedLedgerEntry,
			Severity:    SeverityMedium,
			Title:       fmt.Sprintf("Unmatched ledger entry: %s (%s)", v.VoucherNo, absAmount.Format()),
			AmountPaise: &absAmount,
			Keys: map[string]string{
				"gl_entry": v.VoucherNo,
			},
			Evidence: []EvidenceRef{
				BooksEvidence("list_gl_entries", evidenceArgs, v.EntryIDs...),
			},
			Action: &actInvestigate,
			Status: StatusOpen,
		}
		findings = append(findings, f)
	}

	return findings
}

func filterBankGLEntries(entries []frappe.GLEntry) []frappe.GLEntry {
	var bankEntries []frappe.GLEntry
	for _, e := range entries {
		if isBankAccount(e.Account) {
			bankEntries = append(bankEntries, e)
		}
	}
	if len(bankEntries) > 0 {
		return bankEntries
	}
	return entries
}

func isBankAccount(account string) bool {
	lower := strings.ToLower(account)
	return strings.Contains(lower, "bank") || strings.Contains(lower, "current") ||
		strings.Contains(lower, "hdfc") || strings.Contains(lower, "icici") ||
		strings.Contains(lower, "sbi") || strings.Contains(lower, "axis")
}

func daysDiff(t1, t2 time.Time) int {
	diff := t1.Sub(t2)
	if diff < 0 {
		diff = -diff
	}
	return int(diff / (24 * time.Hour))
}

// jaroWinklerScore calculates Jaro-Winkler string similarity scaled to [0, 10000].
// It uses only integer arithmetic to comply with the nofloat merge gate.
func jaroWinklerScore(s1, s2 string) int {
	s1 = strings.ToUpper(strings.TrimSpace(s1))
	s2 = strings.ToUpper(strings.TrimSpace(s2))
	if s1 == s2 {
		return 10000
	}
	len1 := len(s1)
	len2 := len(s2)
	if len1 == 0 || len2 == 0 {
		return 0
	}

	matchDist := max(len1, len2)/2 - 1
	if matchDist < 0 {
		matchDist = 0
	}

	s1Matches := make([]bool, len1)
	s2Matches := make([]bool, len2)

	matches := 0
	for i := 0; i < len1; i++ {
		start := max(0, i-matchDist)
		end := min(i+matchDist+1, len2)
		for j := start; j < end; j++ {
			if s2Matches[j] || s1[i] != s2[j] {
				continue
			}
			s1Matches[i] = true
			s2Matches[j] = true
			matches++
			break
		}
	}

	if matches == 0 {
		return 0
	}

	transpositions := 0
	k := 0
	for i := 0; i < len1; i++ {
		if !s1Matches[i] {
			continue
		}
		for !s2Matches[k] {
			k++
		}
		if s1[i] != s2[k] {
			transpositions++
		}
		k++
	}

	term1 := (matches * 10000) / len1
	term2 := (matches * 10000) / len2
	term3 := ((matches - transpositions/2) * 10000) / matches
	jaro := (term1 + term2 + term3) / 3

	prefix := 0
	maxPrefix := min(4, min(len1, len2))
	for i := 0; i < maxPrefix; i++ {
		if s1[i] == s2[i] {
			prefix++
		} else {
			break
		}
	}

	jw := jaro + (prefix*(10000-jaro))/10
	return jw
}
