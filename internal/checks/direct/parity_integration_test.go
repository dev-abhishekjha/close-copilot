//go:build integration

package direct

// Findings parity (CC-702): the bank reconciliation check (CC-602) run
// over direct.Books + checks.DirectEvidence and over agent.MCPBooks +
// agent.MCPEvidence (the books and evidence MCP servers in process) must
// give the same findings, IDs and run IDs aside. Postgres runs in
// testcontainers, or at TEST_DATABASE_URL when set.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

func parityStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start testcontainers postgres: %v", err)
		}
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(pg); err != nil {
				t.Logf("terminate testcontainers postgres: %v", err)
			}
		})
		databaseURL, err = pg.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("postgres connection string: %v", err)
		}
	}
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func rupees(t *testing.T, s string) money.Paise {
	t.Helper()
	p, err := money.ParseRupees(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func clearIDs(fs []checks.Finding) []checks.Finding {
	out := make([]checks.Finding, len(fs))
	for i, f := range fs {
		f.ID, f.RunID = uuid.Nil, uuid.Nil
		out[i] = f
	}
	return out
}

func TestParityFindings(t *testing.T) {
	ctx := t.Context()
	st := parityStore(t)
	lines, err := fakeerp.ParityStatement()
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeerp.Seed(ctx, st, lines); err != nil {
		t.Fatal(err)
	}

	_, client := newParityERP(t)
	reg := registry(t, booksMCP(t, client), serveMCP(t, fakeerp.EvidenceMCPServer(st)))

	in := checks.Inputs{
		Company:      parityCompanyID,
		Month:        "2026-09",
		BankAccounts: []string{bankAccount},
	}
	directIn, mcpIn := in, in
	directIn.Books = &Books{Client: client, Companies: st}
	directIn.Evidence = &checks.DirectEvidence{Store: st}
	mcpIn.Books = &agent.MCPBooks{Registry: reg, Companies: st}
	mcpIn.Evidence = &agent.MCPEvidence{Registry: reg}

	runner := checks.NewRunner(nil)
	direct, err := runner.Run(ctx, uuid.New(), directIn, &checks.BankRecCheck{})
	if err != nil {
		t.Fatalf("direct run: %v", err)
	}
	viaMCP, err := runner.Run(ctx, uuid.New(), mcpIn, &checks.BankRecCheck{})
	if err != nil {
		t.Fatalf("MCP run: %v", err)
	}

	if !reflect.DeepEqual(clearIDs(direct), clearIDs(viaMCP)) {
		t.Errorf("findings differ\ndirect: %+v\nMCP:    %+v", clearIDs(direct), clearIDs(viaMCP))
	}

	// The three planted charges are found (both ways, as they are equal).
	charges := map[string]money.Paise{}
	counts := map[string]int{}
	for _, f := range direct {
		counts[f.Type]++
		if f.Type == checks.TypeUnrecordedBankCharge && f.AmountPaise != nil {
			charges[f.Keys["bank_txn_id"]] = *f.AmountPaise
		}
	}
	if len(charges) != len(fakeerp.PlantedCharges) {
		t.Errorf("unrecorded bank charges = %v, want the %d planted ones", charges, len(fakeerp.PlantedCharges))
	}
	for _, c := range fakeerp.PlantedCharges {
		if got, want := charges[c.TxnID], -rupees(t, c.Amount); got != want {
			t.Errorf("charge %s = %d paise, want %d", c.TxnID, got, want)
		}
	}
	if counts[checks.TypeUnmatchedBankLine] != 1 || counts[checks.TypeUnmatchedLedgerEntry] != 10 {
		t.Errorf("finding counts = %v, want 1 unmatched bank line and 10 unmatched ledger entries", counts)
	}
}
