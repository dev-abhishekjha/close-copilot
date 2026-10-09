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

	var databaseURL string
	if envURL := os.Getenv("TEST_DATABASE_URL"); envURL != "" {
		databaseURL = envURL
	} else {
		pgContainer, err := postgres.Run(ctx,
			"pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot_load"),
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
	defer st.Close()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := st.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("migration: %v", err)
	}

	companiesDir := filepath.Join("..", "..", "config", "companies")
	if err := st.SeedCompaniesFromDir(ctx, companiesDir); err != nil {
		t.Fatalf("seed companies: %v", err)
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
	args := []string{"bank", "--company", "sharma", "--month", "2026-09", "--file", csvPath}
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
