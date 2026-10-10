package checks

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

var (
	// bankChargeRegex matches narrations that typically indicate bank charges/fees.
	bankChargeRegex = regexp.MustCompile(`(?i)CHARGES|CHGS|SMS|FEE|COMMISSION`)

	// maxBankChargePaise is ₹10,000 in paise (1,000,000 paise).
	maxBankChargePaise = money.Paise(1000000)
)

// minRemarksRefLen is the shortest bank ref that may match a voucher by
// appearing inside its remarks; shorter refs match too easily by accident.
const minRemarksRefLen = 6

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
	if len(in.BankAccounts) == 0 {
		return nil, fmt.Errorf("checks: bankrec for company %q: no bank accounts in inputs", in.Company)
	}
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

	// Keep only GL entries on the company's bank accounts
	bankGLEntries := filterBankGLEntries(glEntries, in.BankAccounts)

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
	if err != nil {
		return nil, fmt.Errorf("checks: bankrec load payment entries: %w", err)
	}
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

	vouchers := make([]BankVoucher, 0, len(vouchersMap))
	for _, v := range vouchersMap {
		vouchers = append(vouchers, *v)
	}
	// Map iteration order is random; sort so matching and findings are
	// deterministic.
	sort.Slice(vouchers, func(i, j int) bool {
		if !vouchers[i].PostingDate.Equal(vouchers[j].PostingDate) {
			return vouchers[i].PostingDate.Before(vouchers[j].PostingDate)
		}
		return vouchers[i].VoucherNo < vouchers[j].VoucherNo
	})

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

	// Pass 2: exact amount within the date window. Each amount bucket is
	// paired by pairBucket, which keeps as many pairs as possible and
	// among those prefers the closest dates, then the most similar
	// narrations, independent of input order.
	linesByAmount := make(map[money.Paise][]int)
	var amountOrder []money.Paise
	for i, line := range bankLines {
		if matchedBank[line.TxnID] {
			continue
		}
		if _, seen := linesByAmount[line.AmountPaise]; !seen {
			amountOrder = append(amountOrder, line.AmountPaise)
		}
		linesByAmount[line.AmountPaise] = append(linesByAmount[line.AmountPaise], i)
	}
	for _, amt := range amountOrder {
		var open []*BankVoucher
		for _, v := range byAmount[amt] {
			if !matchedVouchers[v.VoucherNo] {
				open = append(open, v)
			}
		}
		if len(open) == 0 {
			continue
		}
		lines := make([]*store.BankLine, 0, len(linesByAmount[amt]))
		for _, li := range linesByAmount[amt] {
			lines = append(lines, &bankLines[li])
		}
		for _, p := range pairBucket(lines, open, windowDays) {
			matchedVouchers[p.voucher.VoucherNo] = true
			matchedBank[p.line.TxnID] = true
			matches = append(matches, BankMatch{
				BankLine: *p.line,
				Voucher:  *p.voucher,
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

type bucketPair struct {
	line    *store.BankLine
	voucher *BankVoucher
}

// bucketScore ranks a partial matching: more pairs first, then a smaller
// total date gap in days, then a higher total narration similarity.
type bucketScore struct {
	pairs int
	days  int
	sim   int
}

func (a bucketScore) better(b bucketScore) bool {
	if a.pairs != b.pairs {
		return a.pairs > b.pairs
	}
	if a.days != b.days {
		return a.days < b.days
	}
	return a.sim > b.sim
}

// pairBucket pairs bank lines with vouchers of the same amount whose dates
// are within windowDays. With every pair allowed the same window, an optimal
// matching never crosses in date order, so a dynamic program over both lists
// sorted by date finds the best one exactly. It runs in O(lines*vouchers) for
// the bucket; buckets are small because amounts rarely repeat.
func pairBucket(lines []*store.BankLine, vouchers []*BankVoucher, windowDays int) []bucketPair {
	ls := slices.Clone(lines)
	vs := slices.Clone(vouchers)
	sort.SliceStable(ls, func(i, j int) bool {
		if !ls[i].TxnDate.Equal(ls[j].TxnDate) {
			return ls[i].TxnDate.Before(ls[j].TxnDate)
		}
		return ls[i].TxnID < ls[j].TxnID
	})
	sort.SliceStable(vs, func(i, j int) bool {
		if !vs[i].PostingDate.Equal(vs[j].PostingDate) {
			return vs[i].PostingDate.Before(vs[j].PostingDate)
		}
		return vs[i].VoucherNo < vs[j].VoucherNo
	})

	const (
		takePair = iota
		skipLine
		skipVoucher
	)
	n, m := len(ls), len(vs)
	cols := m + 1
	choice := make([]byte, (n+1)*cols)
	prev := make([]bucketScore, cols)
	cur := make([]bucketScore, cols)
	for j := 1; j <= m; j++ {
		choice[j] = skipVoucher
	}
	for i := 1; i <= n; i++ {
		cur[0] = bucketScore{}
		choice[i*cols] = skipLine
		for j := 1; j <= m; j++ {
			best, how := prev[j], byte(skipLine)
			if s := cur[j-1]; s.better(best) {
				best, how = s, skipVoucher
			}
			l, v := ls[i-1], vs[j-1]
			if d := daysDiff(l.TxnDate, v.PostingDate); d <= windowDays {
				s := prev[j-1]
				s.pairs++
				s.days += d
				s.sim += jaroWinklerScore(l.Narration, v.Party+" "+v.Remarks)
				if !best.better(s) {
					best, how = s, takePair
				}
			}
			cur[j] = best
			choice[i*cols+j] = how
		}
		prev, cur = cur, prev
	}

	var out []bucketPair
	for i, j := n, m; i > 0 && j > 0; {
		switch choice[i*cols+j] {
		case takePair:
			out = append(out, bucketPair{line: ls[i-1], voucher: vs[j-1]})
			i--
			j--
		case skipLine:
			i--
		default:
			j--
		}
	}
	slices.Reverse(out)
	return out
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
	if len(bankRef) >= minRemarksRefLen && v.Remarks != "" &&
		strings.Contains(strings.ToLower(v.Remarks), strings.ToLower(bankRef)) {
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

// filterBankGLEntries returns the entries posted to one of the named bank
// accounts (exact ERPNext account names).
func filterBankGLEntries(entries []ledger.GLEntry, bankAccounts []string) []ledger.GLEntry {
	accounts := make(map[string]struct{}, len(bankAccounts))
	for _, a := range bankAccounts {
		accounts[a] = struct{}{}
	}
	var bankEntries []ledger.GLEntry
	for _, e := range entries {
		if _, ok := accounts[e.Account]; ok {
			bankEntries = append(bankEntries, e)
		}
	}
	return bankEntries
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
