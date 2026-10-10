//go:build integration

package evidence_test

// The bank statement tests that need Postgres (CC-402), and setupTestStore,
// the Postgres helper every integration test in this package shares. Run
// with go test -tags=integration ./internal/evidence/...

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()

	var databaseURL string
	if envURL := os.Getenv("TEST_DATABASE_URL"); envURL != "" {
		databaseURL = envURL
	} else {
		pgContainer, err := postgres.Run(ctx,
			"pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot_evidence"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start testcontainers postgres: %v", err)
		}
		t.Cleanup(func() {
			_ = testcontainers.TerminateContainer(pgContainer)
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
		t.Fatalf("migration: %v", err)
	}

	// Seed companies
	companiesDir := filepath.Join("..", "..", "config", "companies")
	if err := st.SeedCompaniesFromDir(ctx, companiesDir); err != nil {
		t.Fatalf("seed companies: %v", err)
	}

	return st
}

func TestLoadBankStatement_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	st := setupTestStore(t)

	// 1. Initial Load
	res, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-09", strings.NewReader(sampleCSV), "bank.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement: %v", err)
	}

	if res.Total != 3 || res.Inserted != 3 || res.Updated != 0 {
		t.Errorf("first load result mismatch: %+v", res)
	}

	// 2. Idempotent Reload
	res2, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-09", strings.NewReader(sampleCSV), "bank.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement second run: %v", err)
	}
	if res2.Total != 3 || res2.Inserted != 0 || res2.Updated != 3 {
		t.Errorf("second load result mismatch (idempotency): %+v", res2)
	}

	lines, err := st.ListBankLines(ctx, store.BankLineFilter{CompanyID: "sharma"})
	if err != nil {
		t.Fatalf("ListBankLines: %v", err)
	}
	if len(lines) != 3 {
		t.Errorf("expected 3 bank lines in DB, got %d", len(lines))
	}
}

func TestLoadBankStatement_SumOfAmountsInvariant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Generate synthetic bank CSV via seed for sharma 2026-09
	p, err := seed.LoadProfile(filepath.Join("..", "..", "config", "companies", "sharma.yaml"))
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	w, err := seed.Generate(p, "2026-09", seed.Options{Small: true})
	if err != nil {
		t.Fatalf("Generate world: %v", err)
	}
	seedLines, err := seed.BankLines(w)
	if err != nil {
		t.Fatalf("BankLines: %v", err)
	}

	tmpFile := filepath.Join(t.TempDir(), "bank.csv")
	if err := seed.WriteBankCSV(tmpFile, seedLines); err != nil {
		t.Fatalf("WriteBankCSV: %v", err)
	}

	ctx := context.Background()
	st := setupTestStore(t)

	res, err := evidence.LoadBankFile(ctx, st, "sharma", "2026-09", tmpFile)
	if err != nil {
		t.Fatalf("LoadBankFile: %v", err)
	}
	if res.Total != len(seedLines) {
		t.Errorf("Total loaded lines %d != seeded lines %d", res.Total, len(seedLines))
	}

	// Verify invariant: sum(amounts) = closingBalance - openingBalance
	dbLines, err := st.ListBankLines(ctx, store.BankLineFilter{CompanyID: "sharma"})
	if err != nil {
		t.Fatalf("ListBankLines: %v", err)
	}

	var sum money.Paise
	for _, l := range dbLines {
		sum += l.AmountPaise
	}

	firstLine := dbLines[0]
	lastLine := dbLines[len(dbLines)-1]

	openingBalance := *firstLine.BalancePaise - firstLine.AmountPaise
	closingBalance := *lastLine.BalancePaise

	diff := closingBalance - openingBalance
	if sum != diff {
		t.Errorf("Sum of amounts invariant violated: sum=%s, closing-opening=%s", sum.Format(), diff.Format())
	}
}

func TestLoadBankStatement_ContinuityWarning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	st := setupTestStore(t)

	// Month 1: 2026-08 closing at 20,00,000 paise (₹20,000.00)
	m1CSV := `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
M1-01,2026-08-31,,Closing txn,,,5000.00,20000.00
`
	_, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-08", strings.NewReader(m1CSV), "bank-aug.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement month 1: %v", err)
	}

	// Month 2 matching opening balance: opening = balance (22000.00) - deposit (2000.00) = 20000.00
	m2Matching := `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
M2-01,2026-09-01,,Matching open,,,2000.00,22000.00
`
	resMatch, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-09", strings.NewReader(m2Matching), "bank-sep.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement matching: %v", err)
	}
	if resMatch.Warning != "" {
		t.Errorf("expected empty warning, got %q", resMatch.Warning)
	}

	// Month 2 mismatched opening balance: opening = 30000.00 - 2000.00 = 28000.00 (vs 20000.00)
	m2Mismatch := `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
M2-02,2026-09-02,,Mismatched open,,,2000.00,30000.00
`
	resMismatch, err := evidence.LoadBankStatement(ctx, st, "sharma", "2026-09", strings.NewReader(m2Mismatch), "bank-sep.csv")
	if err != nil {
		t.Fatalf("LoadBankStatement mismatch: %v", err)
	}
	if resMismatch.Warning == "" || !strings.Contains(resMismatch.Warning, "does not match previous month") {
		t.Errorf("expected continuity warning, got %q", resMismatch.Warning)
	}
}
