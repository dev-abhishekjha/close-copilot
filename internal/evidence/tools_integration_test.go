//go:build integration

package evidence_test

// The evidence tool tests over Postgres (CC-503). Run with
// go test -tags=integration ./internal/evidence/...

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// With the integration tag, TestContractEvidence gets a migrated Postgres;
// without it, it skips.
func init() { contractStore = setupTestStore }

func TestTools_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	st := setupTestStore(t)

	// 1. Seed bank statement lines
	const bankCSV = `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
BNK-20260901-001,2026-09-01,2026-09-01,NEFT-RENT-SEP-OMKAR ESTATES,N2440011,177000.00,,2323000.00
BNK-20260914-007,2026-09-14,2026-09-14,SMS/ACCT CHARGES INCL GST,,1180.00,,2321820.00
BNK-20260915-003,2026-09-15,2026-09-15,PG SETTL 0915 BATCH 7781,PGS7781,,97640.00,2419460.00
`
	_, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-09", strings.NewReader(bankCSV), "bank.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement: %v", err)
	}

	// 2. Seed GSTR-2B entries
	suppName := "Tata Communications"
	entries := []store.GSTR2BEntry{
		{
			CompanyID:     "sharma",
			Period:        "2026-09",
			SupplierGSTIN: "27AAACT2727Q1ZB",
			SupplierName:  &suppName,
			InvoiceNo:     "INV-001",
			InvoiceNoNorm: "INV001",
			InvoiceDate:   time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC),
			TaxablePaise:  money.Paise(1000000), // 10,000.00
			IGSTPaise:     money.Paise(180000),  // 1,800.00
			CGSTPaise:     0,
			SGSTPaise:     0,
			ITCAvailable:  true,
		},
		{
			CompanyID:     "sharma",
			Period:        "2026-09",
			SupplierGSTIN: "29AABCB1234F1Z5",
			SupplierName:  nil,
			InvoiceNo:     "INV-002",
			InvoiceNoNorm: "INV002",
			InvoiceDate:   time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
			TaxablePaise:  money.Paise(2000000), // 20,000.00
			IGSTPaise:     0,
			CGSTPaise:     money.Paise(180000),
			SGSTPaise:     money.Paise(180000),
			ITCAvailable:  true,
		},
	}
	if err := st.UpsertGSTR2BEntries(ctx, entries); err != nil {
		t.Fatalf("UpsertGSTR2BEntries: %v", err)
	}

	// Connect an in-memory MCP client session to test tools end-to-end
	server := mcp.NewServer(&mcp.Implementation{Name: "test-evidence", Version: "1.0.0"}, nil)
	evidence.RegisterTools(server, st)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer sess.Close()

	// Verify tool listing and annotations
	toolsList, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("sess.ListTools: %v", err)
	}
	if len(toolsList.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(toolsList.Tools))
	}
	toolsMap := make(map[string]*mcp.Tool)
	for _, tool := range toolsList.Tools {
		toolsMap[tool.Name] = tool
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s missing ReadOnlyHint: true", tool.Name)
		}
	}
	if _, ok := toolsMap["list_bank_lines"]; !ok {
		t.Errorf("missing tool list_bank_lines")
	}
	if _, ok := toolsMap["list_gstr2b_entries"]; !ok {
		t.Errorf("missing tool list_gstr2b_entries")
	}

	// 3. Test list_bank_lines call: all lines
	t.Run("list_bank_lines_all", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_bank_lines",
			Arguments: evidence.ListBankLinesInput{
				Company:  "sharma",
				FromDate: "2026-09-01",
				ToDate:   "2026-09-30",
			},
		})
		if err != nil {
			t.Fatalf("CallTool list_bank_lines: %v", err)
		}
		if res.IsError {
			t.Fatalf("CallTool returned error result: %+v", res)
		}

		lines, ok := res.StructuredContent.([]any)
		if !ok {
			t.Fatalf("expected StructuredContent to be []any, got %T", res.StructuredContent)
		}
		if len(lines) != 3 {
			t.Fatalf("expected 3 bank lines, got %d", len(lines))
		}

		first := lines[0].(map[string]any)
		if first["txn_id"] != "BNK-20260901-001" {
			t.Errorf("expected first txn_id BNK-20260901-001, got %v", first["txn_id"])
		}
		if first["date"] != "2026-09-01" {
			t.Errorf("expected date 2026-09-01, got %v", first["date"])
		}
		// Amount is withdrawal: -17700000 paise
		if amt, ok := first["amount"].(float64); !ok || int64(amt) != -17700000 {
			t.Errorf("expected amount -17700000, got %v", first["amount"])
		}
	})

	// 4. Test list_bank_lines with MinAmount filter
	t.Run("list_bank_lines_min_amount", func(t *testing.T) {
		minPaise := money.Paise(10000000) // 1,00,000 rupees
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_bank_lines",
			Arguments: evidence.ListBankLinesInput{
				Company:   "sharma",
				FromDate:  "2026-09-01",
				ToDate:    "2026-09-30",
				MinAmount: &minPaise,
			},
		})
		if err != nil {
			t.Fatalf("CallTool list_bank_lines: %v", err)
		}
		lines := res.StructuredContent.([]any)
		if len(lines) != 1 {
			t.Fatalf("expected 1 bank line matching min_amount, got %d", len(lines))
		}
		first := lines[0].(map[string]any)
		if first["txn_id"] != "BNK-20260901-001" {
			t.Errorf("expected BNK-20260901-001, got %v", first["txn_id"])
		}
	})

	// 5. Test list_bank_lines with date subrange
	t.Run("list_bank_lines_date_range", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_bank_lines",
			Arguments: evidence.ListBankLinesInput{
				Company:  "sharma",
				FromDate: "2026-09-01",
				ToDate:   "2026-09-05",
			},
		})
		if err != nil {
			t.Fatalf("CallTool list_bank_lines: %v", err)
		}
		lines := res.StructuredContent.([]any)
		if len(lines) != 1 {
			t.Fatalf("expected 1 bank line in [09-01, 09-05], got %d", len(lines))
		}
	})

	// 6. Test list_bank_lines validation errors
	t.Run("list_bank_lines_validation", func(t *testing.T) {
		// Missing company
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_bank_lines",
			Arguments: evidence.ListBankLinesInput{
				FromDate: "2026-09-01",
				ToDate:   "2026-09-30",
			},
		})
		if err == nil && !res.IsError {
			t.Error("expected error for missing company")
		}

		// from_date after to_date
		res, err = sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_bank_lines",
			Arguments: evidence.ListBankLinesInput{
				Company:  "sharma",
				FromDate: "2026-09-30",
				ToDate:   "2026-09-01",
			},
		})
		if err == nil && !res.IsError {
			t.Error("expected error for from_date after to_date")
		}
	})

	// 7. Test list_gstr2b_entries: all entries for period
	t.Run("list_gstr2b_entries_all", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_gstr2b_entries",
			Arguments: evidence.ListGSTR2BEntriesInput{
				Company: "sharma",
				Period:  "2026-09",
			},
		})
		if err != nil {
			t.Fatalf("CallTool list_gstr2b_entries: %v", err)
		}
		if res.IsError {
			t.Fatalf("CallTool returned error result: %+v", res)
		}

		entries := res.StructuredContent.([]any)
		if len(entries) != 2 {
			t.Fatalf("expected 2 GSTR-2B entries, got %d", len(entries))
		}

		e1 := entries[0].(map[string]any)
		if e1["supplier_gstin"] != "27AAACT2727Q1ZB" {
			t.Errorf("expected supplier_gstin 27AAACT2727Q1ZB, got %v", e1["supplier_gstin"])
		}
		if e1["invoice_no"] != "INV-001" {
			t.Errorf("expected invoice_no INV-001, got %v", e1["invoice_no"])
		}
		if e1["invoice_date"] != "2026-09-05" {
			t.Errorf("expected invoice_date 2026-09-05, got %v", e1["invoice_date"])
		}
	})

	// 8. Test list_gstr2b_entries with supplier GSTIN filter
	t.Run("list_gstr2b_entries_filter_gstin", func(t *testing.T) {
		gstin := "29AABCB1234F1Z5"
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_gstr2b_entries",
			Arguments: evidence.ListGSTR2BEntriesInput{
				Company:       "sharma",
				Period:        "2026-09",
				SupplierGSTIN: &gstin,
			},
		})
		if err != nil {
			t.Fatalf("CallTool list_gstr2b_entries: %v", err)
		}
		entries := res.StructuredContent.([]any)
		if len(entries) != 1 {
			t.Fatalf("expected 1 GSTR-2B entry matching GSTIN, got %d", len(entries))
		}
		e1 := entries[0].(map[string]any)
		if e1["supplier_gstin"] != "29AABCB1234F1Z5" {
			t.Errorf("expected 29AABCB1234F1Z5, got %v", e1["supplier_gstin"])
		}
	})

	// 9. Test list_gstr2b_entries validation errors
	t.Run("list_gstr2b_entries_validation", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_gstr2b_entries",
			Arguments: evidence.ListGSTR2BEntriesInput{
				Company: "sharma",
				Period:  "invalid-period",
			},
		})
		if err == nil && !res.IsError {
			t.Error("expected error for invalid period")
		}
	})
}
