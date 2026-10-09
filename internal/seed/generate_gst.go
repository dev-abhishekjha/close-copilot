package seed

import "github.com/abhishekjha/close-copilot/internal/money"

// tax is one invoice's GST split.
type tax struct {
	IGST, CGST, SGST money.Paise
}

func (t tax) total() money.Paise { return t.IGST + t.CGST + t.SGST }

// computeGST returns the GST on a taxable amount at rate percent. In the
// company's own state it is CGST and SGST at half the rate each; in another
// state it is IGST at the full rate. Each tax is rounded to the paisa, half
// away from zero, and CGST and SGST are each computed from the half rate,
// so they are always equal. Rate 0 or an unregistered supplier
// (registered false) gives no tax.
func computeGST(taxable money.Paise, rate int, inState, registered bool) tax {
	if !registered || rate == 0 {
		return tax{}
	}
	if inState {
		// Half the rate in basis points: 18% -> 900 bp, 5% -> 250 bp.
		half := roundDiv(int64(taxable)*int64(rate)*50, 10000)
		return tax{CGST: money.Paise(half), SGST: money.Paise(half)}
	}
	return tax{IGST: money.Paise(roundDiv(int64(taxable)*int64(rate), 100))}
}

// roundDiv returns n/d rounded to the nearest integer, halves away from
// zero. d must be positive.
func roundDiv(n, d int64) int64 {
	q, r := n/d, n%d
	if r < 0 {
		r = -r
	}
	if 2*r >= d {
		if n < 0 {
			q--
		} else {
			q++
		}
	}
	return q
}
