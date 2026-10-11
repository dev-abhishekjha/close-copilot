package checks

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// rulesPrepaidAccount is the chart-of-accounts base name for prepaid costs.
const rulesPrepaidAccount = "Prepaid Expenses"

const (
	rulesIDCapitalisation = "rules.capitalisation"
	rulesIDPrepaid        = "rules.prepaid"
	rulesIDCutoff         = "rules.cutoff"
)

var rulesServicePeriodRe = regexp.MustCompile(`(?i)service\s+period\s*:\s*(\d{1,2}-\d{1,2}-\d{4})\s+to\s+(\d{1,2}-\d{1,2}-\d{4})`)

// PolicyRulesCheck applies the capitalisation, prepaid and cut-off rules from
// config/rules.yaml to purchase invoice lines.
type PolicyRulesCheck struct{}

var _ Check = (*PolicyRulesCheck)(nil)

// Name returns the identifier for this check.
func (p *PolicyRulesCheck) Name() string { return "rules" }

type rulesHit struct {
	typ     string
	rule    string
	account string
	start   string
	amount  money.Paise
	action  string
}

// Run executes the policy rules over the month's purchase invoices.
func (p *PolicyRulesCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	first, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: rules invalid month %q: %w", in.Month, err)
	}
	last := first.AddDate(0, 1, -1)
	nextFirst := first.AddDate(0, 1, 0)

	invoices, err := in.Books.PurchaseInvoices(ctx, in.Company, first, last)
	if err != nil {
		return nil, fmt.Errorf("checks: rules load purchase invoices: %w", err)
	}

	capMin := money.Paise(in.Rules.Capitalisation.ThresholdINR * 100)
	prepMin := money.Paise(in.Rules.Prepaid.MinAmountINR * 100)

	var findings []Finding
	for _, inv := range invoices {
		if inv.IsReturn {
			continue
		}
		hits := map[string]*rulesHit{}
		add := func(typ, rule, action string, it ledger.PurchaseInvoiceItem, start string) {
			h, ok := hits[typ]
			if !ok {
				h = &rulesHit{typ: typ, rule: rule, account: it.ExpenseAccount, start: start, action: action}
				hits[typ] = h
			}
			h.amount += it.Amount
		}
		for _, it := range inv.Items {
			isPrepaidAcct := rulesAccountMatches(it.ExpenseAccount, []string{rulesPrepaidAccount})
			if rulesAccountMatches(it.ExpenseAccount, in.Rules.Capitalisation.WatchedExpenseAccounts) &&
				it.Amount >= capMin && rulesKeywordMatches(it.Description, in.Rules.Capitalisation.AssetKeywords) {
				add(TypeMisclassifiedExpense, rulesIDCapitalisation, ActionReclassify, it, "")
			}
			if it.Amount >= prepMin && !isPrepaidAcct && rulesKeywordMatches(it.Description, in.Rules.Prepaid.Keywords) {
				add(TypePrepaidNotSpread, rulesIDPrepaid, ActionBookEntry, it, "")
			}
			if !isPrepaidAcct {
				if start, ok := rulesServiceStart(it.Description); ok && !start.Before(nextFirst) {
					add(TypeWrongPeriodPosting, rulesIDCutoff, ActionBookEntry, it, start.Format("2006-01-02"))
				}
			}
		}
		supplier := inv.SupplierName
		if supplier == "" {
			supplier = inv.Supplier
		}
		for _, typ := range []string{TypeMisclassifiedExpense, TypePrepaidNotSpread, TypeWrongPeriodPosting} {
			h, ok := hits[typ]
			if !ok {
				continue
			}
			args := map[string]string{
				"company": in.Company, "month": in.Month,
				"rule": h.rule, "account": h.account,
			}
			if h.start != "" {
				args["service_period_start"] = h.start
			}
			amt := h.amount
			act := h.action
			findings = append(findings, Finding{
				Type:        typ,
				Severity:    SeverityMedium,
				Title:       fmt.Sprintf("%s (%s) booked to %s on %s from %s: %s", rulesLabel(typ), amt.Format(), h.account, inv.Name, supplier, h.rule),
				AmountPaise: &amt,
				Keys:        map[string]string{"invoice": inv.Name},
				Evidence:    []EvidenceRef{BooksEvidence("list_purchase_invoices", args, inv.Name)},
				Action:      &act,
				Status:      StatusOpen,
			})
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return strings.Compare(a.Keys["invoice"], b.Keys["invoice"])
	})
	return findings, nil
}

func rulesLabel(typ string) string {
	switch typ {
	case TypeMisclassifiedExpense:
		return "Asset expensed"
	case TypePrepaidNotSpread:
		return "Prepaid cost expensed"
	default:
		return "Next-period cost posted early"
	}
}

func rulesAccountMatches(account string, bases []string) bool {
	a := strings.ToLower(strings.TrimSpace(account))
	if a == "" {
		return false
	}
	for _, b := range bases {
		b = strings.ToLower(strings.TrimSpace(b))
		if b == "" {
			continue
		}
		if a == b || strings.HasPrefix(a, b+" - ") {
			return true
		}
	}
	return false
}

func rulesKeywordMatches(desc string, keywords []string) bool {
	d := strings.ToLower(desc)
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(d, k) {
			return true
		}
	}
	return false
}

func rulesServiceStart(desc string) (time.Time, bool) {
	m := rulesServicePeriodRe.FindStringSubmatch(desc)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("02-01-2006", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
