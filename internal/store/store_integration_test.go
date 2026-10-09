//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func setupTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()

	var databaseURL string
	if envURL := os.Getenv("TEST_DATABASE_URL"); envURL != "" {
		databaseURL = envURL
	} else {
		pgContainer, err := postgres.Run(ctx,
			"pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start testcontainers postgres: %v", err)
		}
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(pgContainer); err != nil {
				t.Logf("terminate testcontainers postgres: %v", err)
			}
		})

		connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("get postgres connection string: %v", err)
		}
		databaseURL = connStr
	}

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
	})

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := st.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// Verify idempotency: second migration should be a no-op
	if err := st.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("second migration (idempotency check): %v", err)
	}

	return st, databaseURL
}

func TestStoreIntegration_MigrationsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)

	// 1. Test Companies
	t.Run("companies", func(t *testing.T) {
		comp := store.Company{
			ID:         "testcorp",
			ERPCompany: "Test Corporation Pvt Ltd",
			GSTIN:      "27AABCT1234A1Z5",
		}
		if err := st.UpsertCompany(ctx, comp); err != nil {
			t.Fatalf("UpsertCompany: %v", err)
		}

		got, err := st.GetCompany(ctx, "testcorp")
		if err != nil {
			t.Fatalf("GetCompany: %v", err)
		}
		if got != comp {
			t.Errorf("GetCompany: got %+v, want %+v", got, comp)
		}

		// Seed from profiles in config/companies
		companiesDir := filepath.Join("..", "..", "config", "companies")
		profiles, err := seed.LoadProfiles(companiesDir)
		if err != nil {
			t.Fatalf("LoadProfiles: %v", err)
		}
		if err := st.SeedCompaniesFromProfiles(ctx, profiles); err != nil {
			t.Fatalf("SeedCompaniesFromProfiles: %v", err)
		}

		all, err := st.ListCompanies(ctx)
		if err != nil {
			t.Fatalf("ListCompanies: %v", err)
		}
		if len(all) < 2 {
			t.Errorf("ListCompanies: expected at least 2 profiles, got %d", len(all))
		}
	})

	// 2. Test Evidence: Bank Lines
	t.Run("bank_lines", func(t *testing.T) {
		bal := money.Paise(9882000)
		ref := "CHQ-123"
		valDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		line := store.BankLine{
			CompanyID:    "testcorp",
			TxnID:        "TXN-001",
			TxnDate:      time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
			ValueDate:    &valDate,
			Narration:    "ACH DEBIT VENDOR PAYMENT",
			Ref:          &ref,
			AmountPaise:  money.Paise(-118000), // withdrawal: negative
			BalancePaise: &bal,
			SourceFile:   "sharma-bank-2026-09.csv",
		}

		if err := st.UpsertBankLines(ctx, []store.BankLine{line}); err != nil {
			t.Fatalf("UpsertBankLines: %v", err)
		}

		got, err := st.GetBankLine(ctx, "testcorp", "TXN-001")
		if err != nil {
			t.Fatalf("GetBankLine: %v", err)
		}
		if got.AmountPaise != money.Paise(-118000) {
			t.Errorf("AmountPaise: got %v, want -118000", got.AmountPaise)
		}
		if got.BalancePaise == nil || *got.BalancePaise != bal {
			t.Errorf("BalancePaise: got %v, want %v", got.BalancePaise, bal)
		}

		// Filter
		minAmt := money.Paise(100000)
		filtered, err := st.ListBankLines(ctx, store.BankLineFilter{
			CompanyID: "testcorp",
			MinAmount: &minAmt,
		})
		if err != nil {
			t.Fatalf("ListBankLines: %v", err)
		}
		if len(filtered) != 1 {
			t.Errorf("ListBankLines: want 1 line, got %d", len(filtered))
		}
	})

	// 3. Test Evidence: GSTR-2B
	t.Run("gstr2b_entries", func(t *testing.T) {
		suppName := "Tech Solutions Ltd"
		entry := store.GSTR2BEntry{
			CompanyID:     "testcorp",
			Period:        "2026-09",
			SupplierGSTIN: "27AABCS1429B1ZB",
			SupplierName:  &suppName,
			InvoiceNo:     "INV/2026/001",
			InvoiceNoNorm: "INV2026001",
			InvoiceDate:   time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
			TaxablePaise:  money.Paise(1000000),
			IGSTPaise:     money.Paise(0),
			CGSTPaise:     money.Paise(90000),
			SGSTPaise:     money.Paise(90000),
			ITCAvailable:  true,
		}

		if err := st.UpsertGSTR2BEntries(ctx, []store.GSTR2BEntry{entry}); err != nil {
			t.Fatalf("UpsertGSTR2BEntries: %v", err)
		}

		got, err := st.GetGSTR2BEntry(ctx, "testcorp", "2026-09", "27AABCS1429B1ZB", "INV2026001")
		if err != nil {
			t.Fatalf("GetGSTR2BEntry: %v", err)
		}
		if got.TaxablePaise != money.Paise(1000000) || got.CGSTPaise != money.Paise(90000) {
			t.Errorf("GSTR2B amounts mismatch: got %+v", got)
		}

		list, err := st.ListGSTR2BEntries(ctx, "testcorp", "2026-09")
		if err != nil {
			t.Fatalf("ListGSTR2BEntries: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("ListGSTR2BEntries: want 1, got %d", len(list))
		}
	})

	// 4. Test Runs, Findings, Proposals, and Audit Log
	t.Run("runs_and_findings", func(t *testing.T) {
		runID := uuid.New()
		started := time.Now().Add(-10 * time.Minute)
		traceID := "trace-xyz-123"

		run := store.CloseRun{
			ID:          runID,
			CompanyID:   "testcorp",
			Month:       "2026-09",
			Status:      "running",
			StartedAt:   &started,
			InputTokens: 1200,
			CostUSD:     0.0240,
			TraceID:     &traceID,
		}
		if err := st.CreateCloseRun(ctx, run); err != nil {
			t.Fatalf("CreateCloseRun: %v", err)
		}

		gotRun, err := st.GetCloseRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetCloseRun: %v", err)
		}
		if gotRun.Status != "running" || gotRun.InputTokens != 1200 {
			t.Errorf("GetCloseRun mismatch: %+v", gotRun)
		}

		// Update run
		finished := time.Now()
		run.Status = "done"
		run.FinishedAt = &finished
		run.OutputTokens = 850
		if err := st.UpdateCloseRun(ctx, run); err != nil {
			t.Fatalf("UpdateCloseRun: %v", err)
		}

		// Findings & Proposal
		findingID := uuid.New()
		propID := uuid.New()
		amt := money.Paise(118000)
		expl := "Bank charge of ₹1,180.00 is missing from the general ledger."
		act := "book_entry"

		prop := store.JournalProposal{
			ID:        propID,
			FindingID: &findingID,
			CompanyID: "testcorp",
			Payload: store.JournalPayload{
				PostingDate: "2026-09-30",
				Lines: []store.JournalLine{
					{Account: "Bank Charges - TC", DebitPaise: 100000},
					{Account: "Input Tax Credit - CGST - TC", DebitPaise: 9000},
					{Account: "Input Tax Credit - SGST - TC", DebitPaise: 9000},
					{Account: "HDFC Bank - TC", CreditPaise: 118000},
				},
				Remark: "Adjust bank charge",
			},
			Status: "proposed",
			Maker:  "agent",
		}

		finding := store.Finding{
			ID:          findingID,
			RunID:       runID,
			Type:        "unrecorded_bank_charge",
			Severity:    "high",
			Title:       "Unrecorded bank charge",
			AmountPaise: &amt,
			Keys: map[string]string{
				"bank_txn_id": "TXN-001",
			},
			Evidence: []store.EvidenceRef{
				{
					Server: "evidence",
					Tool:   "list_bank_lines",
					Args:   json.RawMessage(`{"company":"testcorp"}`),
					IDs:    []string{"TXN-001"},
				},
			},
			Explanation: &expl,
			Action:      &act,
			Citations: []store.Citation{
				{DocID: "POL-001", Section: "§2.1"},
			},
			Proposal: &prop,
			Verified: true,
			Status:   "open",
		}

		if err := st.CreateFinding(ctx, finding); err != nil {
			t.Fatalf("CreateFinding: %v", err)
		}

		findings, err := st.ListFindingsByRun(ctx, runID)
		if err != nil {
			t.Fatalf("ListFindingsByRun: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("ListFindingsByRun: want 1, got %d", len(findings))
		}
		if findings[0].Proposal == nil || len(findings[0].Proposal.Payload.Lines) != 4 {
			t.Errorf("Finding proposal not round-tripped properly: %+v", findings[0].Proposal)
		}

		// Standalone proposal CRUD
		if err := st.CreateJournalProposal(ctx, prop); err != nil {
			t.Fatalf("CreateJournalProposal: %v", err)
		}
		gotProp, err := st.GetJournalProposal(ctx, propID)
		if err != nil {
			t.Fatalf("GetJournalProposal: %v", err)
		}
		if gotProp.Status != "proposed" {
			t.Errorf("GetJournalProposal status: %s", gotProp.Status)
		}

		// Audit Log
		auditID, err := st.InsertAuditLog(ctx, store.AuditLogEntry{
			Actor:  "bot",
			Server: stringPtr("evidence"),
			Tool:   stringPtr("list_bank_lines"),
			Args:   json.RawMessage(`{"company":"testcorp"}`),
		})
		if err != nil {
			t.Fatalf("InsertAuditLog: %v", err)
		}
		if auditID <= 0 {
			t.Errorf("InsertAuditLog returned invalid id: %d", auditID)
		}

		logs, err := st.ListAuditLogs(ctx, 10)
		if err != nil {
			t.Fatalf("ListAuditLogs: %v", err)
		}
		if len(logs) == 0 || logs[0].ID != auditID {
			t.Errorf("ListAuditLogs want latest id %d, got %+v", auditID, logs)
		}
	})

	// 5. Test Docs: Vector Embedding and Full-Text Search
	t.Run("doc_chunks", func(t *testing.T) {
		vec1 := make([]float32, 384)
		vec2 := make([]float32, 384)
		for i := range vec1 {
			vec1[i] = 0.1
			vec2[i] = -0.1
		}

		title1 := "Fixed Assets Accounting Policy"
		title2 := "Revenue Recognition Contract"
		sec := "Section 3"
		compID := "testcorp"

		chunk1 := store.DocChunk{
			DocID:       "POL-001",
			DocType:     "policy",
			CompanyID:   nil, // shared
			Section:     &sec,
			Title:       &title1,
			Content:     "Capital expenditure exceeding 50,000 rupees must be capitalised as fixed assets.",
			ContentHash: "hash-001",
			Embedding:   pgvector.NewVector(vec1),
		}

		chunk2 := store.DocChunk{
			DocID:       "CON-001",
			DocType:     "contract",
			CompanyID:   &compID,
			Section:     &sec,
			Title:       &title2,
			Content:     "Software subscriptions are billed annually in advance with 30-day terms.",
			ContentHash: "hash-002",
			Embedding:   pgvector.NewVector(vec2),
		}

		if err := st.InsertDocChunks(ctx, []store.DocChunk{chunk1, chunk2}); err != nil {
			t.Fatalf("InsertDocChunks: %v", err)
		}

		// Vector search matching closest to vec1
		targetVec := make([]float32, 384)
		for i := range targetVec {
			targetVec[i] = 0.09
		}
		vecResults, err := st.SearchDocChunksByVector(ctx, pgvector.NewVector(targetVec), nil, 2)
		if err != nil {
			t.Fatalf("SearchDocChunksByVector: %v", err)
		}
		if len(vecResults) != 2 {
			t.Fatalf("SearchDocChunksByVector want 2 results, got %d", len(vecResults))
		}
		if vecResults[0].DocID != "POL-001" {
			t.Errorf("Closest vector match should be POL-001, got %s", vecResults[0].DocID)
		}

		// Full-text search
		textResults, err := st.SearchDocChunksByText(ctx, "capitalised assets", nil, 5)
		if err != nil {
			t.Fatalf("SearchDocChunksByText: %v", err)
		}
		if len(textResults) == 0 || textResults[0].DocID != "POL-001" {
			t.Errorf("Text search for 'capitalised assets' failed, got %+v", textResults)
		}

		// Delete by DocID
		if err := st.DeleteDocChunksByDocID(ctx, "POL-001"); err != nil {
			t.Fatalf("DeleteDocChunksByDocID: %v", err)
		}
		postDelete, err := st.SearchDocChunksByText(ctx, "capitalised assets", nil, 5)
		if err != nil {
			t.Fatalf("SearchDocChunksByText after delete: %v", err)
		}
		if len(postDelete) != 0 {
			t.Errorf("Deleted chunk still returned in text search")
		}
	})

	// 6. Test WithTx Rollback
	t.Run("tx_rollback", func(t *testing.T) {
		errRollback := st.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO companies (id, erp_company, gstin) VALUES ($1, $2, $3)",
				"rollmeback", "Rollback Company", "27AABCR9999Z1ZX")
			if err != nil {
				return err
			}
			return pgx.ErrTxClosed // simulated error triggering rollback
		})
		if errRollback == nil {
			t.Fatal("expected tx error, got nil")
		}

		_, err := st.GetCompany(ctx, "rollmeback")
		if err == nil {
			t.Fatal("GetCompany after rollback should fail, but succeeded")
		}
	})
}

func stringPtr(s string) *string {
	return &s
}
