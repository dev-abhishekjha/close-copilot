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
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
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

// plantedCharges are the three bank charges on the statement that the
// books never recorded.
var plantedCharges = []struct{ txn, date, narration, amount string }{
	{"HDFC-20260915-C1", "2026-09-15", "NEFT CHARGES INCL GST", "-5.90"},
	{"HDFC-20260920-C2", "2026-09-20", "DEBIT CARD ANNUAL FEE", "-590.00"},
	{"HDFC-20260930-C3", "2026-09-30", "SMS CHGS JUL-SEP 2026", "-17.70"},
}

// parityBankLines is September's statement: the rent payment (by UTR),
// the gateway receipt, all but the last ten cash sales, the three planted
// charges and one unexplained deposit.
func parityBankLines(t *testing.T) []store.BankLine {
	t.Helper()
	line := func(txn, date, narration, amount string, ref *string) store.BankLine {
		d, err := time.Parse(time.DateOnly, date)
		if err != nil {
			t.Fatal(err)
		}
		return store.BankLine{CompanyID: parityCompanyID, TxnID: txn, TxnDate: d, Narration: narration, Ref: ref,
			AmountPaise: rupees(t, amount), SourceFile: "parity-hdfc-2026-09.csv"}
	}
	utr := "UTR2026091000123"
	out := []store.BankLine{
		line("HDFC-20260910-R1", "2026-09-10", "NEFT VARDHAN ESTATES RENT", "-59000.00", &utr),
		line("HDFC-20260913-P1", "2026-09-13", "PG SETTL TATVA RETAIL", "1180.00", nil),
		line("HDFC-20260918-X1", "2026-09-18", "IMPS UNKNOWN REMITTER", "2500.00", nil),
	}
	for i := range parityCashSales - 10 {
		out = append(out, line("HDFC-CASH-"+cashSaleDate(i)+"-"+cashSale(i), cashSaleDate(i), "CASH DEPOSIT BRANCH", cashSale(i), nil))
	}
	for _, c := range plantedCharges {
		out = append(out, line(c.txn, c.date, c.narration, c.amount, nil))
	}
	return out
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
	if err := st.UpsertCompany(ctx, store.Company{ID: parityCompanyID, ERPCompany: parityERP, GSTIN: parityGSTIN}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBankLines(ctx, parityBankLines(t)); err != nil {
		t.Fatal(err)
	}

	_, client := newParityERP(t)
	es := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-evidence", Version: "parity"}, nil)
	evidence.RegisterTools(es, st)
	reg := registry(t, booksMCP(t, client), serveMCP(t, es))

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
	if len(charges) != len(plantedCharges) {
		t.Errorf("unrecorded bank charges = %v, want the %d planted ones", charges, len(plantedCharges))
	}
	for _, c := range plantedCharges {
		if got, want := charges[c.txn], -rupees(t, c.amount); got != want {
			t.Errorf("charge %s = %d paise, want %d", c.txn, got, want)
		}
	}
	if counts[checks.TypeUnmatchedBankLine] != 1 || counts[checks.TypeUnmatchedLedgerEntry] != 10 {
		t.Errorf("finding counts = %v, want 1 unmatched bank line and 10 unmatched ledger entries", counts)
	}
}
