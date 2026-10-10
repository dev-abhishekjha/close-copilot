package seed

import "github.com/abhishekjha/close-copilot/internal/company"

// The synthetic PAN and GSTIN helpers moved to internal/company (CC-601a)
// with the profiles that derive their identifiers from them. These
// forwarding functions keep the seeder's API unchanged.

// EntityCompany is the PAN's 4th letter for a company.
const EntityCompany = company.EntityCompany

// ValidStateCode reports whether code is a two-digit GST state code: 01-38,
// or 97 (other territory).
func ValidStateCode(code string) bool { return company.ValidStateCode(code) }

// ValidPAN checks the PAN format: 5 upper-case letters, 4 digits, 1
// upper-case letter.
func ValidPAN(pan string) error { return company.ValidPAN(pan) }

// CheckDigit returns the 15th GSTIN character for a 14-character prefix
// (company.CheckDigit).
func CheckDigit(prefix14 string) (byte, error) { return company.CheckDigit(prefix14) }

// PAN returns a synthetic PAN that depends only on (seed, key)
// (company.PAN).
func PAN(seed int64, key string, entity byte) string { return company.PAN(seed, key, entity) }

// GSTIN returns state code + PAN + entity number 1 + Z + check digit.
func GSTIN(stateCode, pan string) (string, error) { return company.GSTIN(stateCode, pan) }

// ValidGSTIN checks a GSTIN's length, character classes, state code and
// check digit.
func ValidGSTIN(s string) error { return company.ValidGSTIN(s) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
