package agent

// Amounts for the verifier (CC-705): reading rupee amounts out of model
// text, collecting the money-valued leaves of the evidence the model saw,
// and deciding whether a number is grounded in them. Every conversion goes
// through internal/money and every comparison is exact int64 arithmetic.
//
// Known limitations:
//
//   - Amounts written in words ("one lakh") or abbreviated ("1.18 lakh",
//     "2 cr") are not extracted. The prompt tells the model to write
//     amounts in digits, such as ₹1,234.56.
//   - A bare comma-separated list of numbers that happens to look like a
//     grouped amount is read as one: "12,345,678" written as a list of IDs
//     is checked as ₹1,23,45,678, and "1,234,5678" as an ID list is
//     amount_unparseable. Such a false reject costs a retry with feedback
//     and, at worst, needs_review.
//   - The other way round, some bare tokens are skipped rather than
//     checked: numbers with neither a grouping comma nor a currency prefix
//     (plain integers such as "590" and bare decimals such as "1180.50"),
//     lists whose groups are all exactly four digits (years such as
//     "2025,2026", account codes such as "1110,1120"), numbers glued on
//     the left to a letter, digit, '.', ',', '/' or '_', or to a '-' that
//     itself follows one (a code such as "BT-0001,0002"), unless that
//     '-' or '/' chains the number to an amount on its left (a range
//     such as "5,000-6,000"). An amount written only in such a shape is
//     not checked; an amount with a currency prefix is, unless the
//     prefix is followed by a whole date token: YYYY-MM-DD, or D-M-YYYY
//     or D/M/YYYY with one or two digits for the day and month, the
//     month at most 12 and the day at most 31 ("INR 2026-08-31"). Any
//     other digits after a prefix ("₹5000-10-2000", "₹1-2-3") are
//     amounts.
//   - After a prefixed amount, a range reads both ends as amounts: the
//     ends joined by '-', a hyphen or dash (U+2010 to U+2014), a minus
//     sign (U+2212) or the word "to", with or without horizontal space
//     around it ("₹500-600", "₹500 – 600", "₹500 to 600"); so does a
//     number right after '/' ("₹500/600"). "/-" ("Rs. 5,000/-") ends an
//     amount. The other end is not read when it starts a whole date
//     token after a spaced separator or "to" ("₹590 to 2026-09-30").
//     Any other number after "to" is read, so "₹500 to 3 vendors" checks
//     ₹3 too: a false reject that costs a retry.
//   - After a bare grouped amount, only a number right after '-' (any
//     number: "5,000-6000", "5,000-6,000") or right after '/' (an amount
//     under the bare rules: "5,000/6,000") is read as the other end. A
//     bare range with spaces or another dash ("5,000 – 6000") reads its
//     second end only when that end is itself a bare amount.

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Grounding limits.
const (
	// amountTolerance is how far (inclusive) a stated amount may be from
	// the evidence: ₹1.
	amountTolerance money.Paise = 100
	// maxEvidenceAmounts bounds the pair search. Above it only single
	// amounts are checked, and the verdict carries evidence_truncated.
	maxEvidenceAmounts = 2000
	// maxGroundable is the largest absolute amount the pair search adds
	// or subtracts without overflow: 2^60 paise, about 11,500 lakh crore
	// rupees.
	maxGroundable money.Paise = 1 << 60
	// maxTokenRunes caps an unparseable token quoted in a violation.
	maxTokenRunes = 32
)

// moneyKeys are the JSON names of the money.Paise fields of the records a
// snapshot can hold: internal/ledger (tagged and untagged), internal/
// evidence, internal/store and internal/checks, plus a few ERPNext wire
// names. Any key that
// ends in "_paise" is money too. TestMoneyKeysCoverPaiseFields fails when a
// money.Paise field in those packages is missing here.
var moneyKeys = map[string]bool{
	// internal/ledger, JSON-tagged (reports.go).
	"opening": true, "debit": true, "credit": true, "closing": true, "net": true, "median_amount": true,
	// internal/ledger, untagged domain forms (models.go): Go field names.
	"Debit": true, "Credit": true, "NetTotal": true, "GrandTotal": true, "OutstandingAmount": true,
	"Amount": true, "TaxAmount": true, "PaidAmount": true, "ReceivedAmount": true,
	"UnallocatedAmount": true, "AllocatedAmount": true, "TotalAmount": true,
	"TotalDebit": true, "TotalCredit": true, "DebitInAccountCurrency": true, "CreditInAccountCurrency": true,
	// Untagged filters and check internals (internal/store, internal/checks).
	"MinAmount": true, "Net": true,
	// internal/evidence (tools.go).
	"amount": true, "balance": true, "taxable": true, "igst": true, "cgst": true, "sgst": true, "min_amount": true,
	// ERPNext wire names.
	"grand_total": true, "net_total": true, "outstanding_amount": true, "paid_amount": true,
	"received_amount": true, "tax_amount": true, "total_debit": true, "total_credit": true,
}

// isMoneyKey reports whether a JSON key names a money amount in paise.
func isMoneyKey(k string) bool {
	return moneyKeys[k] || strings.HasSuffix(k, "_paise")
}

// isBalanceKey reports whether a money key names a balance-type amount: a
// balance, an opening or closing figure, an outstanding amount, a total
// debit or total credit (case-insensitive, with or without an underscore),
// or any running or cumulative total. Such an amount grounds a number
// stated in text on its own, but never as one side of a sum or difference
// (the difference of two running balances is the sum of every movement
// between them, so pairing balances would ground almost any number), and
// never a proposal line or a proposal's total: a journal entry may not post
// a balance.
func isBalanceKey(k string) bool {
	l := strings.ReplaceAll(strings.ToLower(k), "_", "")
	for _, part := range []string{"balance", "opening", "closing", "running", "cumulative", "outstanding", "totaldebit", "totalcredit"} {
		if strings.Contains(l, part) {
			return true
		}
	}
	return false
}

// evidenceAmounts are the money-valued leaves of the evidence: every one
// (singles) and the ones that may take part in a sum or difference
// (pairable: all but balance-type keys).
type evidenceAmounts struct {
	singles  []money.Paise
	pairable []money.Paise
}

// moneyLeaves adds the money-valued leaves of a decoded JSON value
// (json.Number integers under a money key, at any depth). Other integers
// are never treated as money.
func moneyLeaves(v any, out *evidenceAmounts) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			moneyLeaves(e, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			e := x[k]
			if isMoneyKey(k) {
				ns := moneyNumbers(e, nil)
				out.singles = append(out.singles, ns...)
				if !isBalanceKey(k) {
					out.pairable = append(out.pairable, ns...)
				}
			}
			if _, ok := e.(json.Number); !ok {
				moneyLeaves(e, out)
			}
		}
	}
}

// moneyNumbers appends v when it is an integer json.Number, or the integer
// elements of v when it is a list (such as amounts_paise: [...]).
func moneyNumbers(v any, out []money.Paise) []money.Paise {
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			out = append(out, money.Paise(i))
		}
	case []any:
		for _, e := range x {
			if n, ok := e.(json.Number); ok {
				out = moneyNumbers(n, out)
			}
		}
	}
	return out
}

// absPaise is |p|, or false when it can't be grounded safely.
func absPaise(p money.Paise) (money.Paise, bool) {
	if p < 0 {
		if p < -maxGroundable {
			return 0, false
		}
		p = -p
	}
	if p > maxGroundable {
		return 0, false
	}
	return p, true
}

// absSorted returns the groundable absolute values of ps, sorted, with
// duplicates kept. Zero is left out: a zero debit or credit would otherwise
// ground every amount up to ₹1.
func absSorted(ps []money.Paise) []money.Paise {
	out := make([]money.Paise, 0, len(ps))
	for _, e := range ps {
		if a, ok := absPaise(e); ok && a != 0 {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// groundSet is the evidence a candidate amount is checked against.
type groundSet struct {
	singles  []money.Paise // absolute values of every non-zero evidence amount, sorted
	pairable []money.Paise // absolute values of the non-zero, non-balance amounts, sorted
	finding  *money.Paise  // |finding.amount_paise|, nil when absent or zero
	// pairs is false above maxEvidenceAmounts pairable amounts: only
	// single amounts count.
	pairs bool
}

// newGroundSet builds the set from evidence amounts and the finding's
// amount.
func newGroundSet(ev evidenceAmounts, finding *money.Paise) groundSet {
	g := groundSet{
		singles:  absSorted(ev.singles),
		pairable: absSorted(ev.pairable),
	}
	g.pairs = len(g.pairable) <= maxEvidenceAmounts
	if finding != nil {
		if a, ok := absPaise(*finding); ok && a != 0 {
			g.finding = &a
		}
	}
	return g
}

// within reports whether |a - b| <= amountTolerance (both non-negative and
// at most maxGroundable, so the difference can't overflow).
func within(a, b money.Paise) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= amountTolerance
}

// rangeIdx returns the index range [lo, hi) of the sorted amounts in
// [target - tolerance, target + tolerance].
func rangeIdx(amounts []money.Paise, target money.Paise) (int, int) {
	lo := sort.Search(len(amounts), func(i int) bool { return amounts[i] >= target-amountTolerance })
	hi := sort.Search(len(amounts), func(i int) bool { return amounts[i] > target+amountTolerance })
	return lo, hi
}

// hasOther reports whether [lo, hi) holds an index other than skip.
func hasOther(lo, hi, skip int) bool {
	return hi-lo > 1 || (hi-lo == 1 && lo != skip)
}

// singleIn reports whether candidate is within ₹1 (inclusive) of one of
// the sorted amounts or of the finding's amount, on absolute values.
func (g groundSet) singleIn(amounts []money.Paise, candidate money.Paise) bool {
	c, ok := absPaise(candidate)
	if !ok {
		return false
	}
	if g.finding != nil && within(c, *g.finding) {
		return true
	}
	lo, hi := rangeIdx(amounts, c)
	return hi > lo
}

// groundedSingle reports whether candidate is within ₹1 (inclusive) of a
// single evidence amount (balance-type ones included) or of the finding's
// amount, on absolute values.
func (g groundSet) groundedSingle(candidate money.Paise) bool {
	return g.singleIn(g.singles, candidate)
}

// groundedProposalSingle reports whether candidate is within ₹1 of a
// single non-balance evidence amount or of the finding's amount. A
// proposal's total must pass this: an entry may not post a balance.
func (g groundSet) groundedProposalSingle(candidate money.Paise) bool {
	return g.singleIn(g.pairable, candidate)
}

// grounded reports whether candidate is groundedSingle, or within ₹1 of
// the sum or absolute difference of two pairable evidence amounts (two
// different leaves; never a balance-type amount). All comparisons are on
// absolute values. Amounts stated in text and cited_amounts_paise use it.
//
// The pair rule is what lets a derived number through (a GST split such as
// 500 + 90 = 590, or a gap between two records), and it is also where a
// coincidental match can come from. For n pairable amounts it creates at
// most n(n-1)/2 sums and n(n-1)/2 differences, so n(n-1) targets, each a
// window of ±100 paise. It is bounded three ways: the evidence is only the
// records the finding names (selectRecords), only non-zero money keys
// count and balance-type keys are left out, and above maxEvidenceAmounts
// (2,000, so under 4 million targets) the pair rule is switched off and
// the verdict carries evidence_truncated. A search costs O(n log n) per
// candidate.
func (g groundSet) grounded(candidate money.Paise) bool {
	return g.groundedSingle(candidate) || g.groundedPair(candidate)
}

// groundedProposal is grounded for a proposal line: a single non-balance
// evidence amount, the finding's amount, or a pair of non-balance amounts.
// A balance-type amount never grounds a proposal line.
func (g groundSet) groundedProposal(candidate money.Paise) bool {
	return g.groundedProposalSingle(candidate) || g.groundedPair(candidate)
}

// groundedPair reports whether candidate is within ₹1 of the sum or
// absolute difference of two different pairable evidence amounts.
func (g groundSet) groundedPair(candidate money.Paise) bool {
	c, ok := absPaise(candidate)
	if !ok || !g.pairs {
		return false
	}
	for i, a := range g.pairable {
		// a + b = c  =>  b = c - a
		if c-a >= -amountTolerance {
			if lo, hi := rangeIdx(g.pairable, c-a); hasOther(lo, hi, i) {
				return true
			}
		}
		// |a - b| = c  =>  b = a - c or b = a + c
		if lo, hi := rangeIdx(g.pairable, a-c); hasOther(lo, hi, i) {
			return true
		}
		if lo, hi := rangeIdx(g.pairable, a+c); hasOther(lo, hi, i) {
			return true
		}
	}
	return false
}

// Amount patterns. A prefixed amount is ₹, Rs, Rs. or INR (any case,
// followed by any run of horizontal space, Unicode spaces such as U+00A0
// and U+202F included, but never a line break) before digits, which may be
// grouped and may have decimals. A bare amount needs at least one grouping
// comma.
var (
	prefixedAmountRe = regexp.MustCompile(`(?i)(?:₹|\brs\.?|\binr)[\t \p{Zs}]*-?(\d+(?:,\d+)*(?:\.\d+)?)`)
	bareAmountRe     = regexp.MustCompile(`\d+(?:,\d+)+(?:\.\d+)?`)
	// dateRe is a whole date token: 2026-08-31, 31-08-2026 or 31/08/2026
	// (the same separator twice; Go's regexp has no backreferences). The
	// submatches are year, month, day for the first form and day, month,
	// year for the others; dateAt checks the month and day.
	dateRe = regexp.MustCompile(`^(?:(\d{4})-(\d{2})-(\d{2})|(\d{1,2})-(\d{1,2})-(\d{4})|(\d{1,2})/(\d{1,2})/(\d{4}))`)
	// tailNumberRe is the number chained to an amount on its left.
	tailNumberRe = regexp.MustCompile(`^\d+(?:,\d+)*(?:\.\d+)?`)
)

// dateAt returns the end of the date token that starts at s[start:], or -1
// when there is none. The whole token must be date-shaped (see dateRe),
// with a month from 1 to 12 and a day from 1 to 31, and no digit, nor a
// '-' or '/' and a digit, follows it. "₹5,000-6,000", "₹500-600",
// "₹5000-10-2000" and "₹9,87,654/2" are not dates.
func dateAt(s string, start int) int {
	m := dateRe.FindStringSubmatchIndex(s[start:])
	if m == nil {
		return -1
	}
	sub := func(i int) int {
		n, _ := strconv.Atoi(s[start+m[2*i] : start+m[2*i+1]])
		return n
	}
	var month, day int
	switch {
	case m[2] >= 0:
		month, day = sub(2), sub(3)
	case m[8] >= 0:
		day, month = sub(4), sub(5)
	default:
		day, month = sub(7), sub(8)
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return -1
	}
	end := start + m[1]
	if (end < len(s) && isDigit(s[end])) || chainFollows(s, end) {
		return -1
	}
	return end
}

// chainFollows reports whether s[end:] starts with '-' or '/' and a digit:
// a range (5,000-6,000) or a number after a slash (9,87,654/2). "/-"
// (Rs. 5,000/-) is not a chain; it ends the amount.
func chainFollows(s string, end int) bool {
	return end+1 < len(s) && (s[end] == '-' || s[end] == '/') && isDigit(s[end+1])
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// rangeDashes are the separators, besides the word "to", that join the
// two ends of a range after a prefixed amount: hyphen-minus, hyphen,
// non-breaking hyphen, figure dash, en dash, em dash and minus sign.
const rangeDashes = "-‐‑‒–—−"

// skipHSpace returns the index after any run of horizontal space (tab,
// space or a Unicode space separator, never a line break) at s[i:].
func skipHSpace(s string, i int) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != '\t' && !unicode.Is(unicode.Zs, r) {
			break
		}
		i += n
	}
	return i
}

// prefixedRangeAt returns where the other end of a range starts after a
// prefixed amount that ends at s[end]: optional horizontal space, one of
// rangeDashes or the word "to" (any case), optional horizontal space, then
// a digit. It returns -1 when there is none.
func prefixedRangeAt(s string, end int) int {
	i := skipHSpace(s, end)
	if i >= len(s) {
		return -1
	}
	if i+2 <= len(s) && strings.EqualFold(s[i:i+2], "to") {
		i += 2
	} else {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !strings.ContainsRune(rangeDashes, r) {
			return -1
		}
		i += n
	}
	i = skipHSpace(s, i)
	if i < len(s) && isDigit(s[i]) {
		return i
	}
	return -1
}

// readChain reads the numbers chained to an amount that ends at s[end],
// appends the amounts to found, masks them, and returns where the chain
// ends.
//
// After a prefixed amount, the other end of a range (prefixedRangeAt:
// ₹500-600, ₹500 – 600, ₹500 to 600) and a number right after '/'
// (₹500/600) are read as prefixed amounts. After a bare amount, a number
// right after '-' is read when the number on its left was grouped
// (5,000-6000); otherwise, and after '/', it is read only when it is an
// amount under the bare rules (5,000/6,000). The chain stops at the first
// number that isn't read; the bare pass then skips it as a code.
func readChain(s string, end int, prefixed bool, masked []byte, found *[]textAmount) int {
	grouped := true // a bare amount always has a grouping comma
	for {
		start := -1
		switch {
		case chainFollows(s, end):
			start = end + 1
		case prefixed:
			// A whole date after a spaced range or "to" ("₹590 to
			// 2026-09-30") is a date, not the other end.
			if start = prefixedRangeAt(s, end); start >= 0 && dateAt(s, start) >= 0 {
				return end
			}
		}
		if start < 0 {
			return end
		}
		stop := start + tailNumberRe.FindStringIndex(s[start:])[1]
		num := s[start:stop]
		var a textAmount
		switch {
		case prefixed:
			a = readAmount(start, num, true)
		case s[end] == '-' && grouped && !strings.Contains(num, ",") && rightBoundary(s, stop),
			bareAmountRe.FindString(num) == num && !allFourDigitGroups(num) && rightBoundary(s, stop):
			a = readAmount(start, num, false)
		default:
			return end
		}
		if a.bad == "" && a.paise < 0 {
			return end // not money-shaped
		}
		grouped = strings.Contains(num, ",")
		*found = append(*found, a)
		for i := start; i < stop; i++ {
			masked[i] = ' '
		}
		end = stop
	}
}

// textAmount is one amount read from text.
type textAmount struct {
	pos   int
	paise money.Paise
	// bad is the token when it looks like an amount but isn't a valid one
	// (more than two decimals, or bad grouping after a currency prefix).
	bad string
}

// extractAmounts reads the rupee amounts in text, in order: amounts after
// ₹, Rs, Rs. or INR, and bare numbers grouped with commas (Indian
// 1,18,000 or western 118,000.50). It returns them as absolute paise, and
// separately the tokens that look like amounts but can't be read exactly
// (such as ₹1,180.505), which the verifier reports as amount_unparseable.
// Dates, months, GSTIN-like tokens and plain integers without a comma or
// a currency prefix are not amounts.
func extractAmounts(text string) ([]money.Paise, []string) {
	var found []textAmount
	masked := []byte(text)
	for _, m := range prefixedAmountRe.FindAllStringSubmatchIndex(text, -1) {
		if end := dateAt(text, m[2]); end >= 0 {
			// A date after a currency word ("INR 2026-08-31"): mask the
			// whole date so no part of it is read as a bare amount.
			for i := m[0]; i < end; i++ {
				masked[i] = ' '
			}
			continue
		}
		num := text[m[2]:m[3]]
		found = append(found, readAmount(m[0], num, true))
		for i := m[0]; i < m[1]; i++ {
			masked[i] = ' '
		}
		readChain(text, m[3], true, masked, &found)
	}
	rest := string(masked)
	chained := 0 // the end of the last chain the bare pass read
	for _, m := range bareAmountRe.FindAllStringIndex(rest, -1) {
		if m[0] < chained || !bareBoundary(rest, m[0], m[1]) || allFourDigitGroups(rest[m[0]:m[1]]) {
			continue
		}
		a := readAmount(m[0], rest[m[0]:m[1]], false)
		if a.bad == "" && a.paise < 0 {
			continue // not money-shaped: skipped
		}
		found = append(found, a)
		chained = readChain(rest, m[1], false, masked, &found)
	}
	slices.SortStableFunc(found, func(a, b textAmount) int { return a.pos - b.pos })
	var amounts []money.Paise
	var bad []string
	for _, a := range found {
		if a.bad != "" {
			bad = append(bad, a.bad)
			continue
		}
		amounts = append(amounts, a.paise)
	}
	return amounts, bad
}

// readAmount converts one matched number. A bare number that money.Parse-
// Rupees rejects is reported as a bad token when it is money-shaped (a
// decimal part, or a group of three or more digits after a comma, such as
// 9,87,6543), and is not an amount at all (paise -1, no bad token) when
// every group after the first has one or two digits and there are no
// decimals, such as a list "1,2,3" or "12,34".
func readAmount(pos int, num string, prefixed bool) textAmount {
	whole, frac, hasDot := strings.Cut(num, ".")
	if hasDot && len(frac) > 2 {
		return textAmount{pos: pos, bad: capToken(num)}
	}
	p, err := money.ParseRupees(num)
	if err != nil {
		if prefixed || hasDot || hasLongGroup(whole) {
			return textAmount{pos: pos, bad: capToken(num)}
		}
		return textAmount{pos: pos, paise: -1}
	}
	if a, ok := absPaise(p); ok {
		return textAmount{pos: pos, paise: a}
	}
	return textAmount{pos: pos, bad: capToken(num)}
}

// hasLongGroup reports whether a comma-grouped number has a group of three
// or more digits after its first comma.
func hasLongGroup(whole string) bool {
	groups := strings.Split(whole, ",")
	for _, g := range groups[1:] {
		if len(g) >= 3 {
			return true
		}
	}
	return false
}

// allFourDigitGroups reports whether every comma-separated group of a bare
// number has exactly four digits and there are no decimals: a list of
// years (2025,2026) or account codes (1110,1120), never an Indian or
// western grouped amount.
func allFourDigitGroups(num string) bool {
	if strings.Contains(num, ".") {
		return false
	}
	for _, g := range strings.Split(num, ",") {
		if len(g) != 4 {
			return false
		}
	}
	return true
}

// bareBoundary reports whether a bare number at s[start:end] stands alone:
// not part of a word, a code or a longer number.
//
// A '-' on the left is part of a code (BT-0001,0002 or B-1,234) when a
// letter, digit or joining mark sits before it, and the number is skipped;
// after a space or at the start it is a minus sign ("-1,180.00"), and the
// number is still read, so a stated negative amount is still checked. The
// far end of a range ("5,000-6,000") is read by readChain, not here.
func bareBoundary(s string, start, end int) bool {
	if start > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:start])
		if joinsLeft(r) {
			return false
		}
		if r == '-' && start-n > 0 {
			if p, _ := utf8.DecodeLastRuneInString(s[:start-n]); joinsLeft(p) || p == '-' {
				return false
			}
		}
	}
	return rightBoundary(s, end)
}

// rightBoundary reports whether nothing after s[:end] makes the number part
// of a word or a code: no letter, digit or '_', and a '/' only as "/-"
// (Rs. 5,000/-) or before a digit (a chain readChain handles).
func rightBoundary(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[end:])
	if r == '/' {
		return end+1 < len(s) && (s[end+1] == '-' || isDigit(s[end+1]))
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
}

// joinsLeft reports whether r, just before a number, makes the number part
// of a word, a code or a longer number.
func joinsLeft(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(".,/_", r)
}

// capToken caps a quoted token at maxTokenRunes runes.
func capToken(s string) string {
	if utf8.RuneCountInString(s) <= maxTokenRunes {
		return s
	}
	return string([]rune(s)[:maxTokenRunes])
}
