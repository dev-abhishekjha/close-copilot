package seed

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
)

// gstChars is the GSTIN alphabet: a character's index is its value in the
// Luhn mod-36 checksum.
const gstChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

const upperLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// EntityCompany is the PAN's 4th letter for a company.
const EntityCompany byte = 'C'

// ValidStateCode reports whether code is a two-digit GST state code: 01-38,
// or 97 (other territory).
func ValidStateCode(code string) bool {
	if len(code) != 2 || !isDigit(code[0]) || !isDigit(code[1]) {
		return false
	}
	n := int(code[0]-'0')*10 + int(code[1]-'0')
	return (n >= 1 && n <= 38) || n == 97
}

// ValidPAN checks the PAN format: 5 upper-case letters, 4 digits, 1
// upper-case letter.
func ValidPAN(pan string) error {
	if len(pan) != 10 {
		return fmt.Errorf("PAN %q: want 10 characters, got %d", pan, len(pan))
	}
	for i := range len(pan) {
		c := pan[i]
		switch {
		case i < 5 || i == 9:
			if !isUpper(c) {
				return fmt.Errorf("PAN %q: character %d must be an upper-case letter", pan, i+1)
			}
		default:
			if !isDigit(c) {
				return fmt.Errorf("PAN %q: character %d must be a digit", pan, i+1)
			}
		}
	}
	return nil
}

// CheckDigit returns the 15th GSTIN character for a 14-character prefix: a
// Luhn mod-36 checksum over the alphabet 0-9A-Z, with factors 2, 1, 2, ...
// from the right.
func CheckDigit(prefix14 string) (byte, error) {
	if len(prefix14) != 14 {
		return 0, fmt.Errorf("GSTIN prefix %q: want 14 characters, got %d", prefix14, len(prefix14))
	}
	factor, sum := 2, 0
	for i := len(prefix14) - 1; i >= 0; i-- {
		v := strings.IndexByte(gstChars, prefix14[i])
		if v < 0 {
			return 0, fmt.Errorf("GSTIN prefix %q: character %d (%q) is not in 0-9A-Z", prefix14, i+1, prefix14[i])
		}
		d := factor * v
		factor = 3 - factor // alternate 2, 1, 2, ...
		sum += d/36 + d%36
	}
	return gstChars[(36-sum%36)%36], nil
}

// PAN returns a synthetic PAN that depends only on (seed, key): three
// random letters, entity as the 4th letter (C for a company), the first
// letter of key as the 5th (random if key doesn't start with a letter),
// four digits (0001-9999) and a random letter.
//
// Callers pass an upper-case letter as entity; Profile uses EntityCompany.
func PAN(seed int64, key string, entity byte) string {
	// Synthetic test data must be reproducible from the seed, so a seeded
	// PCG is the point here, not a weakness.
	r := rand.New(rand.NewPCG(hashSeed("pan/1", seed, key), hashSeed("pan/2", seed, key))) //nolint:gosec // G404: deterministic synthetic data, not security
	var b strings.Builder
	b.Grow(10)
	for range 3 {
		b.WriteByte(upperLetters[r.IntN(26)])
	}
	b.WriteByte(entity)
	fifth := upperLetters[r.IntN(26)]
	if key != "" {
		if c := toUpper(key[0]); isUpper(c) {
			fifth = c
		}
	}
	b.WriteByte(fifth)
	fmt.Fprintf(&b, "%04d", 1+r.IntN(9999))
	b.WriteByte(upperLetters[r.IntN(26)])
	return b.String()
}

// GSTIN returns state code + PAN + entity number 1 + Z + check digit.
func GSTIN(stateCode, pan string) (string, error) {
	if !ValidStateCode(stateCode) {
		return "", fmt.Errorf("GSTIN: %q is not a GST state code (01-38 or 97)", stateCode)
	}
	if err := ValidPAN(pan); err != nil {
		return "", fmt.Errorf("GSTIN: %w", err)
	}
	prefix := stateCode + pan + "1Z"
	c, err := CheckDigit(prefix)
	if err != nil {
		return "", fmt.Errorf("GSTIN: %w", err)
	}
	return prefix + string(c), nil
}

// ValidGSTIN checks a GSTIN's length, character classes, state code and
// check digit.
func ValidGSTIN(s string) error {
	if len(s) != 15 {
		return fmt.Errorf("GSTIN %q: want 15 characters, got %d", s, len(s))
	}
	if !ValidStateCode(s[:2]) {
		return fmt.Errorf("GSTIN %q: %q is not a GST state code (01-38 or 97)", s, s[:2])
	}
	if err := ValidPAN(s[2:12]); err != nil {
		return fmt.Errorf("GSTIN %q: %w", s, err)
	}
	if c := s[12]; c == '0' || strings.IndexByte(gstChars, c) < 0 {
		return fmt.Errorf("GSTIN %q: character 13 (%q) must be 1-9 or A-Z", s, c)
	}
	if s[13] != 'Z' {
		return fmt.Errorf("GSTIN %q: character 14 must be Z", s)
	}
	want, err := CheckDigit(s[:14])
	if err != nil {
		return fmt.Errorf("GSTIN %q: %w", s, err)
	}
	if s[14] != want {
		return fmt.Errorf("GSTIN %q: check digit is %q, want %q", s, s[14], want)
	}
	return nil
}

// hashSeed folds a domain, a seed and a key into one PCG seed word.
func hashSeed(domain string, seed int64, key string) uint64 {
	h := fnv.New64a()
	buf := make([]byte, 0, len(domain)+len(key)+24)
	buf = append(buf, domain...)
	buf = append(buf, 0)
	buf = strconv.AppendInt(buf, seed, 10)
	buf = append(buf, 0)
	buf = append(buf, key...)
	_, _ = h.Write(buf) // hash.Hash.Write never returns an error
	return h.Sum64()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

func toUpper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}
