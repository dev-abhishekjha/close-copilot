package checks

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
)

func TestDedupeKey(t *testing.T) {
	tests := []struct {
		name string
		f    Finding
		want string
	}{
		{
			name: "no keys",
			f: Finding{
				Type: TypeUnrecordedBankCharge,
			},
			want: TypeUnrecordedBankCharge,
		},
		{
			name: "single key",
			f: Finding{
				Type: TypeUnrecordedBankCharge,
				Keys: map[string]string{"bank_txn_id": "TXN-001"},
			},
			want: `unrecorded_bank_charge:"bank_txn_id"="TXN-001";`,
		},
		{
			name: "multiple keys sorted deterministically",
			f: Finding{
				Type: TypeMissingAccrual,
				Keys: map[string]string{
					"supplier": "SUP-Acme",
					"month":    "2026-09",
				},
			},
			want: `missing_accrual:"month"="2026-09";"supplier"="SUP-Acme";`,
		},
		{
			name: "separators and quotes in values are escaped",
			f: Finding{
				Type: TypeVariance,
				Keys: map[string]string{"account": `a=b;"c"`},
			},
			want: `variance:"account"="a=b;\"c\"";`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DedupeKey(tc.f)
			if got != tc.want {
				t.Errorf("DedupeKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDedupeKeyMapOrderInvariant(t *testing.T) {
	// Build findings with keys inserted in different orders
	f1 := Finding{
		Type: TypeGSTR2BAmountMismatch,
		Keys: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"},
	}
	f2 := Finding{
		Type: TypeGSTR2BAmountMismatch,
		Keys: map[string]string{"d": "4", "c": "3", "b": "2", "a": "1"},
	}

	if DedupeKey(f1) != DedupeKey(f2) {
		t.Errorf("DedupeKey() not order invariant: %q != %q", DedupeKey(f1), DedupeKey(f2))
	}
}

func TestDedupeKeyNoCollision(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
	}{
		{
			name: "separator inside a value",
			a:    map[string]string{"a": "1;b=2"},
			b:    map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "equals inside a key",
			a:    map[string]string{"a=1": "x"},
			b:    map[string]string{"a": "1=x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ka := DedupeKey(Finding{Type: TypeVariance, Keys: tc.a})
			kb := DedupeKey(Finding{Type: TypeVariance, Keys: tc.b})
			if ka == kb {
				t.Errorf("DedupeKey collision: %v and %v both give %q", tc.a, tc.b, ka)
			}
		})
	}
}

func TestEvidenceHelpers(t *testing.T) {
	args := map[string]string{"company": "sharma", "month": "2026-09"}

	bRef := BooksEvidence("get_trial_balance", args, "TB-01")
	if bRef.Server != "books" || bRef.Tool != "get_trial_balance" {
		t.Errorf("BooksEvidence server/tool mismatch: %+v", bRef)
	}
	if len(bRef.IDs) != 1 || bRef.IDs[0] != "TB-01" {
		t.Errorf("BooksEvidence IDs mismatch: %+v", bRef.IDs)
	}

	var parsed map[string]string
	if err := json.Unmarshal(bRef.Args, &parsed); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if parsed["company"] != "sharma" {
		t.Errorf("parsed company = %q, want sharma", parsed["company"])
	}

	eRef := EvidenceEvidence("list_bank_lines", nil, "TXN-1", "TXN-2")
	if eRef.Server != "evidence" || eRef.Tool != "list_bank_lines" {
		t.Errorf("EvidenceEvidence server/tool mismatch: %+v", eRef)
	}
	if len(eRef.IDs) != 2 || eRef.IDs[0] != "TXN-1" || eRef.IDs[1] != "TXN-2" {
		t.Errorf("EvidenceEvidence IDs mismatch: %+v", eRef.IDs)
	}
}

func TestNewFinding(t *testing.T) {
	runID := uuid.New()
	amt := money.Paise(500000)
	keys := map[string]string{"voucher": "JV-01"}
	evidence := []EvidenceRef{BooksEvidence("list_gl_entries", nil, "GLE-1")}

	f := NewFinding(runID, TypeUnmatchedLedgerEntry, SeverityHigh, "Unmatched entry", &amt, keys, evidence)
	if f.ID == uuid.Nil {
		t.Error("expected non-nil UUID")
	}
	if f.RunID != runID {
		t.Errorf("RunID = %s, want %s", f.RunID, runID)
	}
	if f.Status != StatusOpen {
		t.Errorf("Status = %q, want %q", f.Status, StatusOpen)
	}
	if f.AmountPaise == nil || *f.AmountPaise != amt {
		t.Errorf("AmountPaise = %v, want %d", f.AmountPaise, amt)
	}
}
