package seed

import (
	"time"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// The company profile and rules types moved to internal/company (CC-601a)
// so the checks and the store can load them without importing the seeder.
// These aliases and forwarding functions keep the seeder's API unchanged.

// Supplier kinds.
const (
	KindRent         = company.KindRent
	KindUtility      = company.KindUtility
	KindSubscription = company.KindSubscription
	KindGoods        = company.KindGoods
	KindServices     = company.KindServices
)

// Supplier billing cycles. An empty billing means monthly.
const (
	BillingMonthly = company.BillingMonthly
	BillingAnnual  = company.BillingAnnual
)

// Supplier count bounds for a profile (CC-301).
const (
	MinSuppliers = company.MinSuppliers
	MaxSuppliers = company.MaxSuppliers
)

// ErrUnregistered is company.ErrUnregistered: a GSTIN was asked for a
// supplier that isn't registered for GST.
var ErrUnregistered = company.ErrUnregistered

// Profile and rules types; see internal/company for their documentation.
type (
	Profile             = company.Profile
	Sales               = company.Sales
	Supplier            = company.Supplier
	Payroll             = company.Payroll
	Bank                = company.Bank
	Rules               = company.Rules
	CapitalisationRules = company.CapitalisationRules
	PrepaidRules        = company.PrepaidRules
	VarianceRules       = company.VarianceRules
	BankMatchRules      = company.BankMatchRules
	GSTR2BRules         = company.GSTR2BRules
	AccrualRules        = company.AccrualRules
	Range               = company.Range
	Day                 = company.Day
	BasisPoints         = company.BasisPoints
	Share               = company.Share
	Percent             = company.Percent
)

// Fixed returns a range with Min == Max == v.
func Fixed(v int64) Range { return company.Fixed(v) }

// Between returns the range [lo, hi].
func Between(lo, hi int64) Range { return company.Between(lo, hi) }

// DayOfMonth returns the given day n (1-28) of the month
// (company.DayOfMonth).
func DayOfMonth(n int) Day { return company.DayOfMonth(n) }

// ParseMonth parses a YYYY-MM month to UTC midnight on its first day.
func ParseMonth(s string) (time.Time, error) { return company.ParseMonth(s) }

// LoadProfile reads and validates one profile (company.LoadProfile).
func LoadProfile(path string) (Profile, error) { return company.LoadProfile(path) }

// LoadProfiles loads and cross-checks every profile in dir
// (company.LoadProfiles).
func LoadProfiles(dir string) ([]Profile, error) { return company.LoadProfiles(dir) }

// LoadRules reads and validates config/rules.yaml (company.LoadRules).
func LoadRules(path string) (Rules, error) { return company.LoadRules(path) }

// deref returns *r, or the zero Range when r is nil.
func deref(r *Range) Range {
	if r == nil {
		return Range{}
	}
	return *r
}
