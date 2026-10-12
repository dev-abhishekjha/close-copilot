package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const testCompany = "testco"

const validCSV = `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance
BNK-001,2026-09-01,,RENT PAYMENT,,50000.00,,950000.00
BNK-002,2026-09-02,,CUSTOMER RECEIPT,,,30000.00,980000.00
`

func testConfig(databaseURL string) config.Config {
	return config.Config{
		DatabaseURL: config.NewSecret(databaseURL),
	}
}

func setupTestStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	// Always a throwaway testcontainers Postgres: the loader deletes rows for
	// the period it loads, so these tests must never touch a shared database.
	pgContainer, err := postgres.Run(ctx,
		"pgvector/pgvector:pg17",
		postgres.WithDatabase("test_copilot_load"),
		postgres.WithUsername("copilot"),
		postgres.WithPassword("copilot"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start testcontainers postgres (Docker required): %v", err)
	}
	t.Cleanup(func() {
		_ = testcontainers.TerminateContainer(pgContainer)
	})

	databaseURL, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get postgres connection string: %v", err)
	}

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := st.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("migration: %v", err)
	}

	// Profiles keep the sibling gstr2b test working; it only ever sees the
	// throwaway container.
	companiesDir := filepath.Join("..", "..", "config", "companies")
	if err := st.SeedCompaniesFromDir(ctx, companiesDir); err != nil {
		t.Fatalf("seed companies: %v", err)
	}
	// A company no suite seeds, so a stray run can never hit suite data.
	if err := st.UpsertCompany(ctx, store.Company{ID: testCompany, ERPCompany: "Test Co Pvt Ltd", GSTIN: "27AABCT1234A1Z5"}); err != nil {
		t.Fatalf("upsert test company: %v", err)
	}

	return databaseURL
}

func TestLoadBankCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dbURL := setupTestStore(t)
	cfg := testConfig(dbURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Write CSV to a temp dir
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "bank.csv")
	if err := os.WriteFile(csvPath, []byte(validCSV), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	var stdout bytes.Buffer
	run := newRun(&stdout)

	// Test 1: Successful bank load
	args := []string{"bank", "--company", testCompany, "--month", "2026-09", "--file", csvPath}
	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("run(bank): %v", err)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, `"inserted": 2`) || !strings.Contains(outStr, `"total": 2`) {
		t.Errorf("stdout does not contain expected summary: %s", outStr)
	}

	// Test 2: Idempotent re-run
	stdout.Reset()
	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("run(bank) second time: %v", err)
	}
	outStr2 := stdout.String()
	if !strings.Contains(outStr2, `"inserted": 0`) || !strings.Contains(outStr2, `"updated": 2`) {
		t.Errorf("stdout does not indicate updated rows: %s", outStr2)
	}

	// Test 3: Missing required flags
	errMissing := run(context.Background(), cfg, log, []string{"bank"})
	if errMissing == nil || !strings.Contains(errMissing.Error(), "--company and --month are required") {
		t.Errorf("want required flags error, got: %v", errMissing)
	}

	// Test 4: Unknown subcommand
	errUnknown := run(context.Background(), cfg, log, []string{"foo"})
	if errUnknown == nil || !strings.Contains(errUnknown.Error(), "unknown subcommand") {
		t.Errorf("want unknown subcommand error, got: %v", errUnknown)
	}
}

func TestLoadAllCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dbURL := setupTestStore(t)
	cfg := testConfig(dbURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	suitesDir := t.TempDir()
	suiteYAML := "suite: tiny\nhistory_months: [\"2026-08\"]\nevaluated:\n  - {company: testco, months: [\"2026-09\"]}\nclean_control: {company: testco, month: \"2026-08\"}\n"
	if err := os.WriteFile(filepath.Join(suitesDir, "tiny.yaml"), []byte(suiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	write := func(month, bank, g string) {
		dir := filepath.Join(dataDir, testCompany, month)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bank.csv"), []byte(bank), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gstr2b.json"), []byte(g), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aug := "txn_id,date,value_date,narration,ref,withdrawal,deposit,balance\nBNK-A1,2026-08-05,,RENT,,100.00,,900.00\n"
	write("2026-08", aug, `{"data":{"gstin":"27AABCT1234A1Z5","rtnprd":"082026","docdata":{"b2b":[]}}}`)
	write("2026-09", validCSV, `{"data":{"gstin":"27AABCT1234A1Z5","rtnprd":"092026","docdata":{"b2b":[]}}}`)

	var stdout bytes.Buffer
	run := newRun(&stdout)
	args := []string{"all", "--suite", "tiny", "--suites-dir", suitesDir, "--data-dir", dataDir}
	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("run(all): %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"company_months": 2`) {
		t.Errorf("want 2 company-months in total:\n%s", stdout.String())
	}

	// Reload with the Sep bank file shrunk to one row: the other row is removed.
	one := "txn_id,date,value_date,narration,ref,withdrawal,deposit,balance\nBNK-001,2026-09-01,,RENT PAYMENT,,50000.00,,950000.00\n"
	write("2026-09", one, `{"data":{"gstin":"27AABCT1234A1Z5","rtnprd":"092026","docdata":{"b2b":[]}}}`)
	stdout.Reset()
	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("run(all) second time: %v", err)
	}
	if !strings.Contains(stdout.String(), `"removed": 1`) {
		t.Errorf("want one removed row:\n%s", stdout.String())
	}
}
