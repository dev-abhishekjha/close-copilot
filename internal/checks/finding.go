package checks

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Finding aliases the store.Finding model from the shared specs (CC-401, CC-601).
type Finding = store.Finding

// EvidenceRef records the tool or reader call and record IDs that produced a finding.
type EvidenceRef = store.EvidenceRef

// Citation points to a specific document section.
type Citation = store.Citation

// JournalProposal represents an adjusting entry proposed by a check or agent.
type JournalProposal = store.JournalProposal

// JournalPayload represents the debit/credit lines of a proposed journal entry.
type JournalPayload = store.JournalPayload

// JournalLine represents a single debit or credit line in a journal entry.
type JournalLine = store.JournalLine

// Finding types from the shared specs (docs/implementation-tickets.md).
const (
	TypeUnrecordedBankCharge   = "unrecorded_bank_charge"
	TypeUnmatchedBankLine      = "unmatched_bank_line"
	TypeUnmatchedLedgerEntry   = "unmatched_ledger_entry"
	TypeDuplicateVendorPayment = "duplicate_vendor_payment"
	TypeGSTR2BMissingIn2B      = "gstr2b_missing_in_2b"
	TypeGSTR2BAmountMismatch   = "gstr2b_amount_mismatch"
	TypeGSTR2BMissingInBooks   = "gstr2b_missing_in_books"
	TypeGSTR2BWrongPeriod      = "gstr2b_wrong_period"
	TypeGSTR2BNotEligible      = "gstr2b_not_eligible"
	TypeMissingAccrual         = "missing_accrual"
	TypePrepaidNotSpread       = "prepaid_not_spread"
	TypeMisclassifiedExpense   = "misclassified_expense"
	TypeWrongPeriodPosting     = "wrong_period_posting"
	TypeVariance               = "variance"
	TypeSuspiciousInstruction  = "suspicious_instruction_text"
)

// Severity levels.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"
)

// Finding statuses.
const (
	StatusOpen        = "open"
	StatusNeedsReview = "needs_review"
	StatusAccepted    = "accepted"
	StatusDismissed   = "dismissed"
)

// Recommended action enums.
const (
	ActionBookEntry        = "book_entry"
	ActionAccrue           = "accrue"
	ActionReclassify       = "reclassify"
	ActionFollowUpSupplier = "follow_up_supplier"
	ActionInvestigate      = "investigate"
	ActionNoAction         = "no_action"
)

// DedupeKey returns a deterministic string key formed by finding type and
// sorted keys. Each key and value is quoted (strconv.Quote), so values that
// contain '=', ';' or quotes cannot make two different key sets collide.
func DedupeKey(f Finding) string {
	var b strings.Builder
	b.WriteString(f.Type)
	if len(f.Keys) == 0 {
		return b.String()
	}
	b.WriteByte(':')
	keys := make([]string, 0, len(f.Keys))
	for k := range f.Keys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		b.WriteString(strconv.Quote(k))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(f.Keys[k]))
		b.WriteByte(';')
	}
	return b.String()
}

// NewEvidenceRef creates a new EvidenceRef with JSON-encoded arguments.
func NewEvidenceRef(server, tool string, args any, ids ...string) (EvidenceRef, error) {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return EvidenceRef{}, fmt.Errorf("checks: marshal evidence args: %w", err)
		}
		raw = b
	}
	return EvidenceRef{
		Server: server,
		Tool:   tool,
		Args:   raw,
		IDs:    ids,
	}, nil
}

// BooksEvidence builds an EvidenceRef for books reader calls.
func BooksEvidence(tool string, args any, ids ...string) EvidenceRef {
	ref, err := NewEvidenceRef("books", tool, args, ids...)
	if err != nil {
		return EvidenceRef{Server: "books", Tool: tool, IDs: ids}
	}
	return ref
}

// EvidenceEvidence builds an EvidenceRef for evidence reader calls.
func EvidenceEvidence(tool string, args any, ids ...string) EvidenceRef {
	ref, err := NewEvidenceRef("evidence", tool, args, ids...)
	if err != nil {
		return EvidenceRef{Server: "evidence", Tool: tool, IDs: ids}
	}
	return ref
}

// NewFinding creates a new Finding with default status and a generated UUID.
func NewFinding(runID uuid.UUID, findingType, severity, title string, amount *money.Paise, keys map[string]string, evidence []EvidenceRef) Finding {
	return Finding{
		ID:          uuid.New(),
		RunID:       runID,
		Type:        findingType,
		Severity:    severity,
		Title:       title,
		AmountPaise: amount,
		Keys:        keys,
		Evidence:    evidence,
		Status:      StatusOpen,
	}
}
