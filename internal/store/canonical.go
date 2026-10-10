package store

// Canonical JSON for content-addressed artifacts (CC-709). An artifact's
// address is the lowercase hex sha256 of its canonical bytes, so the same
// value always hashes the same, whether it comes from a Go value or back
// out of a jsonb column.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnstorableContent reports a value that can't be stored as an artifact:
// a string (or object key) holding NUL, which Postgres jsonb rejects, or
// U+FFFD, which is what invalid UTF-8 becomes when encoded, so the original
// bytes could not be told apart from a genuine U+FFFD. The error never
// carries the content.
var ErrUnstorableContent = errors.New("store: content has a NUL or invalid UTF-8 in a string")

// maxExponent bounds the decimal exponent a canonical number may carry, so
// a hostile "1e999999999" cannot expand into a gigabyte of zeros.
const maxExponent = 1000

// Canonical returns the canonical JSON encoding of v: v is marshalled,
// decoded with UseNumber (numbers never pass through float64) and
// marshalled again, which sorts object keys and leaves no whitespace.
// Every number is rewritten to its shortest exact plain decimal form
// ("1.50" and "15e-1" both become "1.5"), because Postgres jsonb keeps the
// value of a number but not its spelling. A string or key holding NUL or
// invalid UTF-8 (seen as U+FFFD) fails with ErrUnstorableContent: it fails
// closed rather than store something that differs from what was read.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: canonical: marshal: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("store: canonical: decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("store: canonical: trailing data after the JSON value")
	}
	tree, err = normalize(tree)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("store: canonical: re-marshal: %w", err)
	}
	return out, nil
}

// CanonicalHash returns the canonical bytes of v and their lowercase hex
// sha256.
func CanonicalHash(v any) ([]byte, string, error) {
	b, err := Canonical(v)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// normalize rewrites every json.Number in a decoded tree to its canonical
// decimal text.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		s, err := canonicalNumber(string(t))
		if err != nil {
			return nil, err
		}
		return json.Number(s), nil
	case string:
		if err := storableString(t); err != nil {
			return nil, err
		}
		return t, nil
	case map[string]any:
		for k, e := range t {
			if err := storableString(k); err != nil {
				return nil, err
			}
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			t[k] = n
		}
		return t, nil
	case []any:
		for i, e := range t {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			t[i] = n
		}
		return t, nil
	default:
		return v, nil
	}
}

// storableString rejects NUL and U+FFFD (the decoder's stand-in for
// invalid UTF-8).
func storableString(s string) error {
	if strings.IndexByte(s, 0) >= 0 || strings.ContainsRune(s, utf8.RuneError) {
		return ErrUnstorableContent
	}
	return nil
}

// canonicalNumber rewrites a JSON number as a plain decimal with no
// exponent, no leading zeros in the integer part and no trailing zeros in
// the fraction, using string arithmetic only. Zero is "0", never "-0".
func canonicalNumber(s string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("store: canonical: invalid number %.40q", s)
	}
	if s == "" {
		return bad()
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		e, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return bad()
		}
		if e > maxExponent || e < -maxExponent {
			return "", fmt.Errorf("store: canonical: number exponent %d out of range", e)
		}
		exp = e
	}
	intPart, fracPart := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, fracPart = mant[:i], mant[i+1:]
	}
	if intPart == "" || !allDigits(intPart) || !allDigits(fracPart) {
		return bad()
	}
	digits := intPart + fracPart
	point := len(intPart) + exp // position of the decimal point in digits
	for digits != "" && digits[0] == '0' {
		digits = digits[1:]
		point--
	}
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return "0", nil
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	switch {
	case point <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -point))
		b.WriteString(digits)
	case point >= len(digits):
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", point-len(digits)))
	default:
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	}
	return b.String(), nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
