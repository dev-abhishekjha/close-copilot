package direct

// Direct-versus-MCP parity (CC-702). One fake ERPNext is read twice: by
// direct.Books through the frappe client, and by agent.MCPBooks through
// the books MCP server (books.RegisterTools over a frappe client pointed at
// the same fake), served over HTTP with mcpkit.BuildHandler (both from
// internal/testsupport/fakeerp). Every
// BooksReader method must give deep-equal results both ways. This lives
// here because only a package outside internal/agent may import both
// paths.

import (
	"context"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/testsupport/fakeerp"
)

// The synthetic company and month come from internal/testsupport/fakeerp.
const (
	parityCompanyID = fakeerp.CompanyID
	parityERP       = fakeerp.ERPCompany
	parityToken     = "parity-agent-token"
	bankAccount     = fakeerp.BankAccount
	parityCashSales = fakeerp.CashSales
)

// newParityERP serves fakeerp's synthetic month: a chart of accounts, an
// opening, four months of rent bills from one supplier (a recurring
// supplier), September sales, a receipt and a payment, 260 cash-sale
// journals (more than one page of GL Entries), bank charges in July and
// August, and noise the filters must drop: a draft, a cancelled document,
// a cancelled GL pair and another company's documents.
func newParityERP(t *testing.T) (*fakeerp.ERP, *frappe.Client) {
	t.Helper()
	s, err := fakeerp.Serve(fakeerp.NewMonth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s.ERP, s.Client
}

func discard() *slog.Logger { return fakeerp.Discard() }

// serveMCP serves s at /mcp behind the agent token and returns the URL.
func serveMCP(t *testing.T, s *mcp.Server) string {
	t.Helper()
	e := fakeerp.ServeMCP(s, parityToken)
	t.Cleanup(e.Close)
	return e.URL
}

// booksMCP serves the real books tools over client.
func booksMCP(t *testing.T, client *frappe.Client) string {
	t.Helper()
	return serveMCP(t, fakeerp.BooksMCPServer(client))
}

// registry connects an agent registry to the two servers.
func registry(t *testing.T, booksURL, evidenceURL string) *agent.Registry {
	t.Helper()
	cfg := config.Config{BooksMCPURL: booksURL, EvidenceMCPURL: evidenceURL, MCPTokenAgent: config.NewSecret(parityToken)}
	r, err := agent.NewRegistry(t.Context(), cfg, discard())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// stubEvidence is an evidence server with a read-only list_bank_lines that
// returns nothing, so the registry can start without Postgres.
func stubEvidence(t *testing.T) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "stub-evidence", Version: "parity"}, nil)
	type in struct {
		Company  string `json:"company"`
		FromDate string `json:"from_date"`
		ToDate   string `json:"to_date"`
	}
	type line struct {
		TxnID string `json:"txn_id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "list_bank_lines", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, []line, error) {
			return nil, nil, nil
		})
	return serveMCP(t, s)
}

// withoutFiscalYear clears GLEntry.FiscalYear: list_gl_entries's contract
// (CC-502, pinned by its schema golden) doesn't carry it and no check
// reads it, so the MCP reader leaves it empty. Every other field must
// match.
func withoutFiscalYear(es []frappe.GLEntry) []frappe.GLEntry {
	out := slices.Clone(es)
	for i := range out {
		out[i].FiscalYear = ""
	}
	return out
}

func TestParityBooks(t *testing.T) {
	_, client := newParityERP(t)
	dir := fakeDirectory{parityCompanyID: {ID: parityCompanyID, ERPCompany: parityERP}}
	direct := &Books{Client: client, Companies: dir}
	viaMCP := &agent.MCPBooks{Registry: registry(t, booksMCP(t, client), stubEvidence(t)), Companies: dir}
	var _ checks.BooksReader = viaMCP

	ctx := t.Context()
	sepFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sepTo := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	junFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	two := func(name string, d, m func() (any, error), check func(t *testing.T, v any)) {
		t.Run(name, func(t *testing.T) {
			dv, derr := d()
			mv, merr := m()
			if derr != nil || merr != nil {
				t.Fatalf("direct err %v, MCP err %v", derr, merr)
			}
			if !reflect.DeepEqual(dv, mv) {
				t.Errorf("direct and MCP differ\ndirect: %+v\nMCP:    %+v", dv, mv)
			}
			check(t, dv)
		})
	}

	two("TrialBalance",
		func() (any, error) { return direct.TrialBalance(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.TrialBalance(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			tb := v.(books.TB)
			if len(tb.Rows) < 8 || tb.Totals.Debit != tb.Totals.Credit || tb.Totals.Debit == 0 {
				t.Errorf("trial balance looks empty or unbalanced: %d rows, totals %+v", len(tb.Rows), tb.Totals)
			}
		})

	two("GLEntries",
		func() (any, error) {
			es, err := direct.GLEntries(ctx, parityCompanyID, sepFrom, sepTo)
			return withoutFiscalYear(es), err
		},
		func() (any, error) { return viaMCP.GLEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			es := v.([]frappe.GLEntry)
			if len(es) <= books.GLPageSize {
				t.Errorf("%d GL Entries; the month must span more than one %d-row page", len(es), books.GLPageSize)
			}
			for _, e := range es {
				if e.Company != parityERP || e.IsCancelled {
					t.Fatalf("filtered entry returned: %+v", e)
				}
			}
		})

	for _, rng := range []struct {
		name     string
		from, to time.Time
	}{{"September", sepFrom, sepTo}, {"June to September", junFrom, sepTo}} {
		two("PurchaseInvoices/"+rng.name,
			func() (any, error) { return direct.PurchaseInvoices(ctx, parityCompanyID, rng.from, rng.to) },
			func() (any, error) { return viaMCP.PurchaseInvoices(ctx, parityCompanyID, rng.from, rng.to) },
			func(t *testing.T, v any) {
				if len(v.([]frappe.PurchaseInvoice)) == 0 {
					t.Error("no purchase invoices")
				}
			})
	}

	two("SalesInvoices",
		func() (any, error) { return direct.SalesInvoices(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.SalesInvoices(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			if len(v.([]frappe.SalesInvoice)) != 2 {
				t.Errorf("sales invoices = %d, want 2", len(v.([]frappe.SalesInvoice)))
			}
		})

	two("PaymentEntries",
		func() (any, error) { return direct.PaymentEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func() (any, error) { return viaMCP.PaymentEntries(ctx, parityCompanyID, sepFrom, sepTo) },
		func(t *testing.T, v any) {
			if len(v.([]frappe.PaymentEntry)) != 2 {
				t.Errorf("payment entries = %d, want 2", len(v.([]frappe.PaymentEntry)))
			}
		})

	two("PaymentEntries/none",
		func() (any, error) {
			return direct.PaymentEntries(ctx, parityCompanyID, junFrom, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
		},
		func() (any, error) {
			return viaMCP.PaymentEntries(ctx, parityCompanyID, junFrom, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))
		},
		func(t *testing.T, v any) {
			if v.([]frappe.PaymentEntry) != nil {
				t.Errorf("June payments = %v, want none", v)
			}
		})

	two("AccountHistory",
		func() (any, error) {
			return direct.AccountHistory(ctx, parityCompanyID, "Bank Charges - STPL", "2026-09", 4)
		},
		func() (any, error) {
			return viaMCP.AccountHistory(ctx, parityCompanyID, "Bank Charges - STPL", "2026-09", 4)
		},
		func(t *testing.T, v any) {
			h := v.([]books.MonthTotal)
			if len(h) != 4 || h[1].Debit != 1770 || h[2].Debit != 1770 {
				t.Errorf("history = %+v, want 17.70 in July and August", h)
			}
		})

	two("RecurringSuppliers",
		func() (any, error) { return direct.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 3, 20) },
		func() (any, error) { return viaMCP.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 3, 20) },
		func(t *testing.T, v any) {
			rs := v.([]checks.RecurringSupplier)
			if len(rs) != 1 || rs[0].Supplier != "SUP-RENT" || rs[0].MedianAmount != 5900000 {
				t.Errorf("recurring = %+v, want SUP-RENT at 59,000.00", rs)
			}
		})

	two("RecurringSuppliers/none",
		func() (any, error) { return direct.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 4, 20) },
		func() (any, error) { return viaMCP.RecurringSuppliers(ctx, parityCompanyID, "2026-09", 3, 4, 20) },
		func(t *testing.T, v any) {
			if v.([]checks.RecurringSupplier) != nil {
				t.Errorf("recurring = %v, want none", v)
			}
		})
}
