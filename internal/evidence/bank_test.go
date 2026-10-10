package evidence_test

// The bank CSV parsing tests. The tests that load into Postgres are in
// bank_integration_test.go behind the integration build tag.

import (
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const sampleCSV = `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
BNK-20260901-001,2026-09-01,2026-09-01,NEFT-RENT-SEP-OMKAR ESTATES,N2440011,177000.00,,2323000.00
BNK-20260914-007,2026-09-14,2026-09-14,SMS/ACCT CHARGES INCL GST,,1180.00,,2321820.00
BNK-20260915-003,2026-09-15,2026-09-15,PG SETTL 0915 BATCH 7781,PGS7781,,97640.00,2419460.00
`

func TestParseBankCSV_Valid(t *testing.T) {
	lines, err := evidence.ParseBankCSV(strings.NewReader(sampleCSV), "sharma", "bank.csv")
	if err != nil {
		t.Fatalf("ParseBankCSV: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}

	// Line 1: Withdrawal 177000.00 -> -17700000 paise
	l1 := lines[0]
	if l1.TxnID != "BNK-20260901-001" {
		t.Errorf("TxnID: got %q, want BNK-20260901-001", l1.TxnID)
	}
	if l1.AmountPaise != money.Paise(-17700000) {
		t.Errorf("AmountPaise (withdrawal): got %v, want -17700000", l1.AmountPaise)
	}
	if l1.BalancePaise == nil || *l1.BalancePaise != money.Paise(232300000) {
		t.Errorf("BalancePaise: got %v, want 232300000", l1.BalancePaise)
	}

	// Line 3: Deposit 97640.00 -> +9764000 paise
	l3 := lines[2]
	if l3.TxnID != "BNK-20260915-003" {
		t.Errorf("TxnID: got %q, want BNK-20260915-003", l3.TxnID)
	}
	if l3.AmountPaise != money.Paise(9764000) {
		t.Errorf("AmountPaise (deposit): got %v, want 9764000", l3.AmountPaise)
	}
	if l3.Ref == nil || *l3.Ref != "PGS7781" {
		t.Errorf("Ref: got %v, want PGS7781", l3.Ref)
	}
}

func TestParseBankCSV_DeriveTxnID(t *testing.T) {
	csvWithoutIDs := `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
,2026-09-01,,TRANSFER TO VENDOR,,5000.00,,10000.00
,2026-09-01,,ANOTHER TRANSFER,,2000.00,,8000.00
,2026-09-02,,CUSTOMER DEPOSIT,,,3000.00,11000.00
`
	lines, err := evidence.ParseBankCSV(strings.NewReader(csvWithoutIDs), "sharma", "bank.csv")
	if err != nil {
		t.Fatalf("ParseBankCSV: %v", err)
	}

	if lines[0].TxnID != "TXN-20260901-001" {
		t.Errorf("derived ID 1: got %q, want TXN-20260901-001", lines[0].TxnID)
	}
	if lines[1].TxnID != "TXN-20260901-002" {
		t.Errorf("derived ID 2: got %q, want TXN-20260901-002", lines[1].TxnID)
	}
	if lines[2].TxnID != "TXN-20260902-001" {
		t.Errorf("derived ID 3: got %q, want TXN-20260902-001", lines[2].TxnID)
	}
}

func TestParseBankCSV_MalformedHeader(t *testing.T) {
	badHeader := `id,txn_date,narration,amount,balance
1,2026-09-01,Test,100,200
`
	_, err := evidence.ParseBankCSV(strings.NewReader(badHeader), "sharma", "bank.csv")
	if err == nil {
		t.Fatal("expected header error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid header") {
		t.Errorf("error %q should mention invalid header", err.Error())
	}
}

func TestParseBankCSV_MalformedRows(t *testing.T) {
	tests := []struct {
		name    string
		csv     string
		wantErr string
	}{
		{
			name: "both withdrawal and deposit set",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,Vendor payment,,100.00,200.00,1000.00
`,
			wantErr: "line 2: exactly one of withdrawal and deposit must be set",
		},
		{
			name: "neither withdrawal nor deposit set",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,Vendor payment,,,,1000.00
`,
			wantErr: "line 2: exactly one of withdrawal and deposit must be set",
		},
		{
			name: "invalid date",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,01-09-2026,,Vendor payment,,100.00,,1000.00
`,
			wantErr: "line 2: invalid date \"01-09-2026\"",
		},
		{
			name: "empty narration",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,,,100.00,,1000.00
`,
			wantErr: "line 2: narration is empty",
		},
		{
			name: "negative withdrawal",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,Payment,,-100.00,,1000.00
`,
			wantErr: "line 2: withdrawal amount -100.00 must be positive",
		},
		{
			name: "non-numeric withdrawal",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,Payment,,abc,,1000.00
`,
			wantErr: "line 2: parse withdrawal \"abc\"",
		},
		{
			name: "invalid balance",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,,Payment,,100.00,,invalid
`,
			wantErr: "line 2: parse balance \"invalid\"",
		},
		{
			name: "too few columns",
			csv: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
T1,2026-09-01,Test
`,
			wantErr: "line 2: expected 8 columns, got 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := evidence.ParseBankCSV(strings.NewReader(tt.csv), "sharma", "bank.csv")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
