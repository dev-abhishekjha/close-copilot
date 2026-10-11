package evidence

import "strings"

// NormInvoiceNo normalises an invoice number for matching: uppercase, drop
// spaces, tabs, '/', '-', '_' and '.', and strip leading zeros from each run
// of digits (an all-zero run becomes "0"). "abc/101", "ABC-0101" and
// "ABC 101" all give "ABC101". The seed keeps an equivalent copy
// (seed.NormInvoiceNo); a parity test guards drift.
func NormInvoiceNo(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	b.Grow(len(s))
	var digits strings.Builder

	flush := func() {
		if digits.Len() == 0 {
			return
		}
		t := strings.TrimLeft(digits.String(), "0")
		if t == "" {
			t = "0"
		}
		b.WriteString(t)
		digits.Reset()
	}

	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '\t' || r == '/' || r == '-' || r == '_' || r == '.':
			flush()
		default:
			flush()
			b.WriteRune(r)
		}
	}
	flush()
	return b.String()
}
