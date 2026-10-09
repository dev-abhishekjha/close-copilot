package seed

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Planted error types. Phase 1 implements unrecorded_bank_charge; Phase 2
// widens to the remaining nine types.
const (
	ErrorUnrecordedBankCharge   = "unrecorded_bank_charge"
	ErrorDuplicateVendorPayment = "duplicate_vendor_payment"
	ErrorMissingAccrual         = "missing_accrual"
	ErrorGSTR2BMissingIn2B      = "gstr2b_missing_in_2b"
	ErrorGSTR2BAmountMismatch   = "gstr2b_amount_mismatch"
	ErrorGSTR2BMissingInBooks   = "gstr2b_missing_in_books"
	ErrorPrepaidNotSpread       = "prepaid_not_spread"
	ErrorMisclassifiedExpense   = "misclassified_expense"
	ErrorWrongPeriodPosting     = "wrong_period_posting"
	ErrorPromptInjection        = "prompt_injection"
)

// ScenarioConfig tunes the planter across companies and months.
type ScenarioConfig struct {
	Suite         string             `json:"suite" yaml:"suite"`
	HistoryMonths []string           `json:"history_months,omitempty" yaml:"history_months,omitempty"`
	Evaluated     []EvaluatedTarget  `json:"evaluated" yaml:"evaluated"`
	CleanControl  CleanControlTarget `json:"clean_control" yaml:"clean_control"`
	Errors        map[string]int     `json:"errors" yaml:"errors"`
	Small         bool               `json:"small,omitempty" yaml:"small,omitempty"`
	Seed          int64              `json:"seed,omitempty" yaml:"seed,omitempty"`
}

// EvaluatedTarget names an evaluated company and its evaluated months.
type EvaluatedTarget struct {
	Company string   `json:"company" yaml:"company"`
	Months  []string `json:"months" yaml:"months"`
}

// CleanControlTarget names the unmutated control company-month.
type CleanControlTarget struct {
	Company string `json:"company" yaml:"company"`
	Month   string `json:"month" yaml:"month"`
}

// DefaultSkeletonConfig returns the scenario configuration for suite-skeleton
// (Phase 1: sharma 2026-09 with 3 unrecorded bank charges, 2026-08 clean control).
func DefaultSkeletonConfig() ScenarioConfig {
	return ScenarioConfig{
		Suite: "suite-skeleton",
		Evaluated: []EvaluatedTarget{
			{Company: "sharma", Months: []string{"2026-09"}},
		},
		CleanControl: CleanControlTarget{
			Company: "sharma",
			Month:   "2026-08",
		},
		Errors: map[string]int{
			ErrorUnrecordedBankCharge: 3,
		},
		Small: true,
	}
}

// PlantedError records one planted error applied to a company-month.
type PlantedError struct {
	ID            string      `json:"id"`
	Type          string      `json:"type"`
	TouchedExtIDs []string    `json:"touched_ext_ids"`
	AmountPaise   money.Paise `json:"amount_paise"`
}

// PlantedWorld contains the mutated views of a world after planting errors.
// BooksWorld is posted to ERPNext; BankWorld is used for bank CSV generation.
type PlantedWorld struct {
	Scenario      string         `json:"scenario"`
	Company       string         `json:"company"`
	Month         string         `json:"month"`
	Clean         bool           `json:"clean"`
	BooksWorld    World          `json:"books_world"`
	BankWorld     World          `json:"bank_world"`
	PlantedErrors []PlantedError `json:"planted_errors"`
}

// Planter applies planted errors to views of a true world.
type Planter struct {
	cfg ScenarioConfig
}

// NewPlanter creates a Planter with the given scenario configuration.
func NewPlanter(cfg ScenarioConfig) *Planter {
	return &Planter{cfg: cfg}
}

// PlantErrors is a package-level helper that creates a Planter and plants errors
// in the given company-month world.
func PlantErrors(w World, cfg ScenarioConfig) (PlantedWorld, error) {
	p := NewPlanter(cfg)
	return p.Plant(w)
}

// Plant applies the scenario's configured errors to w and returns the mutated
// views for books and bank. A clean control month is left completely unmutated.
func (p *Planter) Plant(w World) (PlantedWorld, error) {
	if w.Company == "" || w.Month == "" {
		return PlantedWorld{}, errors.New("world must have company and month")
	}

	// 1. Clean control check: if this company-month matches clean control, leave unmutated.
	if p.isCleanControl(w.Company, w.Month) {
		return cleanWorld(p.cfg.Suite, w), nil
	}

	// 2. Evaluated target check: if evaluated targets are declared and this company-month
	// is not among them, treat it as unmutated.
	if !p.isEvaluated(w.Company, w.Month) {
		return cleanWorld(p.cfg.Suite, w), nil
	}

	if len(p.cfg.Errors) == 0 {
		return cleanWorld(p.cfg.Suite, w), nil
	}

	rng := planterRNG(uint64(max(p.cfg.Seed, 0)), p.cfg.Suite, w.Company, w.Month) //nolint:gosec // G115: non-negative seed value
	usedExtIDs := make(map[string]bool)

	// Copy original events for books and bank.
	booksEvents := make([]Event, 0, len(w.Events))
	bankEvents := make([]Event, len(w.Events))
	copy(bankEvents, w.Events)
	bankClosing := w.ClosingBank

	var planted []PlantedError
	errorSeq := 1

	// Mutation: unrecorded_bank_charge
	// Removes the bank charge journal from books view, leaving it in bank CSV.
	bankChargeCount := p.cfg.Errors[ErrorUnrecordedBankCharge]
	droppedBooksExtIDs := make(map[string]bool)

	if bankChargeCount > 0 {
		// First pass: find existing EventBankCharge events in w.Events.
		for _, ev := range w.Events {
			if bankChargeCount == 0 {
				break
			}
			if ev.Kind == EventBankCharge && !usedExtIDs[ev.ExtID] {
				usedExtIDs[ev.ExtID] = true
				droppedBooksExtIDs[ev.ExtID] = true
				planted = append(planted, PlantedError{
					ID:            fmt.Sprintf("E%02d", errorSeq),
					Type:          ErrorUnrecordedBankCharge,
					TouchedExtIDs: []string{ev.ExtID},
					AmountPaise:   ev.Gross,
				})
				errorSeq++
				bankChargeCount--
			}
		}

		// If more bank charges are needed than exist in w.Events, generate additional
		// bank charges, append them to bankEvents, and adjust bankClosing.
		if bankChargeCount > 0 {
			start, err := ParseMonth(w.Month)
			if err != nil {
				return PlantedWorld{}, fmt.Errorf("plant errors parse month %s: %w", w.Month, err)
			}
			daysInMonth := start.AddDate(0, 1, -1).Day()
			maxSeq := maxEventSequence(w.Events)

			for bankChargeCount > 0 {
				maxSeq++
				extID := fmt.Sprintf("EVT-%s-%s-%04d", w.Company, w.Month, maxSeq)
				day := 1 + rng.IntN(daysInMonth)
				chargeDate := fmt.Sprintf("%s-%02d", w.Month, day)

				taxable := money.Paise(100000) // ₹1,000 monthly account charge
				cgst := money.Paise(roundDiv(int64(taxable)*bankChargeGSTRate/2, 100))
				sgst := money.Paise(roundDiv(int64(taxable)*bankChargeGSTRate/2, 100))
				gross := taxable + cgst + sgst

				extraEv := Event{
					ExtID:     extID,
					Kind:      EventBankCharge,
					Date:      chargeDate,
					Account:   AccountBankCharges,
					Taxable:   taxable,
					CGST:      cgst,
					SGST:      sgst,
					Gross:     gross,
					Narration: "Bank charges for " + monthName(start),
					Meta: Meta{
						Description: fmt.Sprintf("Account charges for %s, including GST", monthName(start)),
						GSTRate:     bankChargeGSTRate,
					},
				}

				bankEvents = append(bankEvents, extraEv)
				bankClosing -= gross
				usedExtIDs[extID] = true

				planted = append(planted, PlantedError{
					ID:            fmt.Sprintf("E%02d", errorSeq),
					Type:          ErrorUnrecordedBankCharge,
					TouchedExtIDs: []string{extID},
					AmountPaise:   gross,
				})
				errorSeq++
				bankChargeCount--
			}
		}
	}

	// Filter dropped events from books view.
	for _, ev := range w.Events {
		if droppedBooksExtIDs[ev.ExtID] {
			continue
		}
		booksEvents = append(booksEvents, ev)
	}

	booksWorld := w
	booksWorld.Events = booksEvents

	bankWorld := w
	bankWorld.Events = bankEvents
	bankWorld.ClosingBank = bankClosing

	return PlantedWorld{
		Scenario:      p.cfg.Suite,
		Company:       w.Company,
		Month:         w.Month,
		Clean:         false,
		BooksWorld:    booksWorld,
		BankWorld:     bankWorld,
		PlantedErrors: planted,
	}, nil
}

func (p *Planter) isCleanControl(company, month string) bool {
	return p.cfg.CleanControl.Company == company && p.cfg.CleanControl.Month == month
}

func (p *Planter) isEvaluated(company, month string) bool {
	if len(p.cfg.Evaluated) == 0 {
		return true
	}
	for _, target := range p.cfg.Evaluated {
		if target.Company != company {
			continue
		}
		if slices.Contains(target.Months, month) {
			return true
		}
	}
	return false
}

func cleanWorld(suite string, w World) PlantedWorld {
	return PlantedWorld{
		Scenario:      suite,
		Company:       w.Company,
		Month:         w.Month,
		Clean:         true,
		BooksWorld:    w,
		BankWorld:     w,
		PlantedErrors: []PlantedError{},
	}
}

// maxEventSequence extracts the highest integer suffix from EVT-<company>-<month>-<seq>.
func maxEventSequence(events []Event) int {
	maxSeq := len(events)
	for _, ev := range events {
		parts := strings.Split(ev.ExtID, "-")
		if len(parts) > 0 {
			if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	return maxSeq
}

func planterRNG(seed uint64, suite, company, month string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(suite))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(company))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(month))
	if seed == 0 {
		seed = 42
	}
	return rand.New(rand.NewPCG(seed, h.Sum64())) //nolint:gosec // G404: deterministic synthetic data, not security
}
