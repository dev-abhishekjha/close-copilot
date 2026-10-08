package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Paise is an exact amount of Indian rupees in paise (1 rupee = 100 paise).
type Paise int64

// ErrSyntax reports text that is not a rupee amount or a JSON number.
var ErrSyntax = errors.New("invalid amount")

// ErrOverflow reports an amount outside the int64 paise range.
var ErrOverflow = errors.New("amount out of range")

// maxExponent bounds the exponent FromJSONNumber works with. Any non-zero
// mantissa scaled by 10^maxExponent overflows int64 paise, and any mantissa
// scaled by 10^-maxExponent rounds to zero, so larger exponents need no
// arithmetic.
const maxExponent = 1000

// ParseRupees parses a rupee amount such as "1180", "-0.05", "1,180.00" or
// "12,34,56,789.01" into exact paise.
//
// The text is an optional leading "-", the rupees, and optionally a "." and
// one or more decimals. The rupees may use digit-grouping commas, either
// Indian (1,23,45,678: the last group three digits, the others two) or
// western (12,345,678: every group three digits). Any number of decimals is
// accepted; the amount is rounded to the nearest paisa, with halves rounded
// away from zero ("0.005" is 1 paisa, "-0.005" is -1, "0.004" is 0). This is
// the same rule as the shared spec's math.Round(x*100), computed on the text
// so it is exact.
//
// Errors wrap ErrSyntax or ErrOverflow.
func ParseRupees(s string) (Paise, error) {
	p, err := parseRupees(s)
	if err != nil {
		return 0, fmt.Errorf("money: parse rupees %q: %w", s, err)
	}
	return p, nil
}

func parseRupees(s string) (Paise, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrSyntax)
	}
	neg := false
	rest := s
	if rest[0] == '-' {
		neg = true
		rest = rest[1:]
	}
	intPart, frac, hasDot := strings.Cut(rest, ".")
	if hasDot && frac == "" {
		return 0, fmt.Errorf("%w: no digits after the decimal point", ErrSyntax)
	}
	if !allDigits(frac) {
		return 0, fmt.Errorf("%w: decimals must be digits", ErrSyntax)
	}
	digits, err := ungroup(intPart)
	if err != nil {
		return 0, err
	}
	return scale(neg, digits+frac, 2-len(frac))
}

// ungroup checks the rupee digits and their grouping commas and returns the
// digits alone.
func ungroup(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: no digits before the decimal point", ErrSyntax)
	}
	if !strings.Contains(s, ",") {
		if !allDigits(s) {
			return "", fmt.Errorf("%w: rupees must be digits", ErrSyntax)
		}
		return s, nil
	}
	groups := strings.Split(s, ",")
	for _, g := range groups {
		if g == "" || !allDigits(g) {
			return "", fmt.Errorf("%w: misplaced comma or non-digit in %q", ErrSyntax, s)
		}
	}
	if !groupedAs(groups, 3) && !groupedAs(groups, 2) {
		return "", fmt.Errorf("%w: %q is neither Indian (1,23,456) nor western (123,456) grouping", ErrSyntax, s)
	}
	return strings.Join(groups, ""), nil
}

// groupedAs reports whether groups (at least two) are a valid grouping with
// the last group three digits, the middle groups exactly size digits and the
// first group 1 to size digits. size 3 is western grouping, size 2 Indian.
func groupedAs(groups []string, size int) bool {
	n := len(groups)
	if len(groups[n-1]) != 3 {
		return false
	}
	if l := len(groups[0]); l < 1 || l > size {
		return false
	}
	for _, g := range groups[1 : n-1] {
		if len(g) != size {
			return false
		}
	}
	return true
}

// FromJSONNumber converts a JSON number of rupees, as Frappe sends it, into
// exact paise. It reads the number's text, including the exponent forms
// Python floats can produce ("1e-05", "1.18E+3", "-2.5e2"), and rounds to
// the nearest paisa with halves away from zero, like ParseRupees. It never
// goes through a float. Commas are not accepted, and an empty number is an
// error.
//
// Errors wrap ErrSyntax or ErrOverflow.
func FromJSONNumber(n json.Number) (Paise, error) {
	p, err := fromJSONNumber(string(n))
	if err != nil {
		return 0, fmt.Errorf("money: json number %q: %w", string(n), err)
	}
	return p, nil
}

func fromJSONNumber(s string) (Paise, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrSyntax)
	}
	neg := false
	rest := s
	if rest[0] == '-' {
		neg = true
		rest = rest[1:]
	}
	mant, expText := rest, ""
	if i := strings.IndexAny(rest, "eE"); i >= 0 {
		mant, expText = rest[:i], rest[i+1:]
		if expText == "" {
			return 0, fmt.Errorf("%w: empty exponent", ErrSyntax)
		}
	}
	intPart, frac, hasDot := strings.Cut(mant, ".")
	if intPart == "" || !allDigits(intPart) {
		return 0, fmt.Errorf("%w: no digits before the decimal point", ErrSyntax)
	}
	if hasDot && (frac == "" || !allDigits(frac)) {
		return 0, fmt.Errorf("%w: decimals must be one or more digits", ErrSyntax)
	}

	exp := 0
	if expText != "" {
		expNeg := false
		switch expText[0] {
		case '-':
			expNeg = true
			expText = expText[1:]
		case '+':
			expText = expText[1:]
		}
		if expText == "" || !allDigits(expText) {
			return 0, fmt.Errorf("%w: exponent must be digits", ErrSyntax)
		}
		expText = strings.TrimLeft(expText, "0")
		if len(expText) > 4 {
			expText = "9999" // beyond ±maxExponent either way
		}
		if expText != "" {
			e, err := strconv.Atoi(expText)
			if err != nil {
				return 0, fmt.Errorf("%w: exponent: %w", ErrSyntax, err)
			}
			exp = min(e, maxExponent+1)
		}
		if expNeg {
			exp = -exp
		}
	}

	digits := intPart + frac
	if strings.Trim(digits, "0") == "" {
		return 0, nil
	}
	if exp > maxExponent {
		return 0, ErrOverflow
	}
	if exp < -maxExponent {
		return 0, nil // rounds to zero whatever the mantissa
	}
	return scale(neg, digits, exp+2-len(frac))
}

// scale returns the paise for the decimal digits times 10^shift, negated if
// neg, rounding half away from zero when shift drops digits. digits is
// non-empty and all ASCII digits.
//
// It accumulates the magnitude as a negative int64, as strconv does, so
// that math.MinInt64 paise is reachable without overflow.
func scale(neg bool, digits string, shift int) (Paise, error) {
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	keep, roundUp := digits, false
	if shift < 0 {
		drop := -shift
		if drop > len(digits) {
			return 0, nil
		}
		keep = digits[:len(digits)-drop]
		roundUp = digits[len(digits)-drop] >= '5'
		shift = 0
	}

	var acc int64 // the magnitude, negated
	push := func(d int64) error {
		if acc < (math.MinInt64+d)/10 {
			return ErrOverflow
		}
		acc = acc*10 - d
		return nil
	}
	for i := 0; i < len(keep); i++ {
		if err := push(int64(keep[i] - '0')); err != nil {
			return 0, err
		}
	}
	for range shift {
		if err := push(0); err != nil {
			return 0, err
		}
	}
	if roundUp {
		if acc == math.MinInt64 {
			return 0, ErrOverflow
		}
		acc--
	}
	if neg {
		return Paise(acc), nil
	}
	if acc == math.MinInt64 {
		return 0, ErrOverflow
	}
	return Paise(-acc), nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// split returns the sign and the magnitude of p as whole rupees and the
// remaining paise (0 to 99). It is safe for math.MinInt64.
func (p Paise) split() (neg bool, rupees int64, paise int64) {
	if p < 0 {
		// p/100 and p%100 truncate toward zero, so both negations fit even
		// for math.MinInt64.
		return true, -int64(p / 100), -int64(p % 100)
	}
	return false, int64(p / 100), int64(p % 100)
}

// Rupees returns p as plain rupees with two decimals and no grouping, such
// as "1180.00" or "-0.05". ParseRupees(p.Rupees()) == p for every p.
func (p Paise) Rupees() string {
	neg, r, f := p.split()
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(r, 10))
	fmt.Fprintf(&b, ".%02d", f)
	return b.String()
}

// Format returns p for display: "₹" and the rupees in Indian grouping (the
// last three digits, then pairs) with two decimals, such as "₹1,23,456.78".
// Negative amounts are "-₹1,23,456.78" and zero is "₹0.00".
func (p Paise) Format() string {
	neg, r, f := p.split()
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString("₹")
	b.WriteString(groupIndian(strconv.FormatInt(r, 10)))
	fmt.Fprintf(&b, ".%02d", f)
	return b.String()
}

// groupIndian inserts commas into a run of digits: the last three digits,
// then groups of two.
func groupIndian(d string) string {
	if len(d) <= 3 {
		return d
	}
	head, tail := d[:len(d)-3], d[len(d)-3:]
	var parts []string
	if len(head)%2 == 1 {
		parts = append(parts, head[:1])
		head = head[1:]
	}
	for len(head) > 0 {
		parts = append(parts, head[:2])
		head = head[2:]
	}
	parts = append(parts, tail)
	return strings.Join(parts, ",")
}
