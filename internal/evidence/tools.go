// Package evidence handles evidence ingestion (bank statements, GSTR-2B) and provides
// MCP tool handlers for querying evidence records from Postgres.
//
// Built in CC-402, CC-403, CC-503, CC-805.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const dateLayout = "2006-01-02"

// ListBankLinesInput is the input parameter structure for the list_bank_lines MCP tool.
type ListBankLinesInput struct {
	Company   string       `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate  string       `json:"from_date" jsonschema:"start of transaction date range (YYYY-MM-DD)"`
	ToDate    string       `json:"to_date" jsonschema:"end of transaction date range (YYYY-MM-DD)"`
	MinAmount *money.Paise `json:"min_amount,omitempty" jsonschema:"optional minimum absolute amount in paise"`
}

// BankLineOutput is the output representation of a single bank statement line.
type BankLineOutput struct {
	TxnID     string       `json:"txn_id"`
	Date      string       `json:"date"`
	Narration string       `json:"narration"`
	Ref       *string      `json:"ref,omitempty"`
	Amount    money.Paise  `json:"amount"`
	Balance   *money.Paise `json:"balance,omitempty"`
}

// ListGSTR2BEntriesInput is the input parameter structure for the list_gstr2b_entries MCP tool.
type ListGSTR2BEntriesInput struct {
	Company       string  `json:"company" jsonschema:"company ID, e.g. sharma"`
	Period        string  `json:"period" jsonschema:"return period in YYYY-MM format"`
	SupplierGSTIN *string `json:"supplier_gstin,omitempty" jsonschema:"optional supplier GSTIN to filter entries"`
}

// GSTR2BEntryOutput is the output representation of a single GSTR-2B inward supply invoice.
type GSTR2BEntryOutput struct {
	SupplierGSTIN string      `json:"supplier_gstin"`
	SupplierName  *string     `json:"supplier_name,omitempty"`
	InvoiceNo     string      `json:"invoice_no"`
	InvoiceNoNorm string      `json:"invoice_no_norm"`
	InvoiceDate   string      `json:"invoice_date"`
	Taxable       money.Paise `json:"taxable"`
	IGST          money.Paise `json:"igst"`
	CGST          money.Paise `json:"cgst"`
	SGST          money.Paise `json:"sgst"`
	ITCAvailable  bool        `json:"itc_available"`
}

// RegisterTools registers the evidence MCP tools on the provided MCP server.
func RegisterTools(server *mcp.Server, st *store.Store) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_bank_lines",
		Description: "List bank statement lines for a company within a date range, ordered by transaction date and txn_id.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListBankLinesInput) (*mcp.CallToolResult, []BankLineOutput, error) {
		if in.Company == "" {
			return nil, nil, errors.New("company is required")
		}
		if in.FromDate == "" {
			return nil, nil, errors.New("from_date is required (YYYY-MM-DD)")
		}
		if in.ToDate == "" {
			return nil, nil, errors.New("to_date is required (YYYY-MM-DD)")
		}

		fromDate, err := time.Parse(dateLayout, in.FromDate)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid from_date %q: must be YYYY-MM-DD", in.FromDate)
		}
		toDate, err := time.Parse(dateLayout, in.ToDate)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid to_date %q: must be YYYY-MM-DD", in.ToDate)
		}
		if fromDate.After(toDate) {
			return nil, nil, fmt.Errorf("from_date %q cannot be after to_date %q", in.FromDate, in.ToDate)
		}

		filter := store.BankLineFilter{
			CompanyID: in.Company,
			FromDate:  &fromDate,
			ToDate:    &toDate,
			MinAmount: in.MinAmount,
		}

		lines, err := st.ListBankLines(ctx, filter)
		if err != nil {
			return nil, nil, fmt.Errorf("list bank lines: %w", err)
		}

		out := make([]BankLineOutput, 0, len(lines))
		for _, l := range lines {
			out = append(out, BankLineOutput{
				TxnID:     l.TxnID,
				Date:      l.TxnDate.Format(dateLayout),
				Narration: l.Narration,
				Ref:       l.Ref,
				Amount:    l.AmountPaise,
				Balance:   l.BalancePaise,
			})
		}

		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_gstr2b_entries",
		Description: "List GSTR-2B inward supply entries for a company and return period (YYYY-MM), optionally filtered by supplier GSTIN.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListGSTR2BEntriesInput) (*mcp.CallToolResult, []GSTR2BEntryOutput, error) {
		if in.Company == "" {
			return nil, nil, errors.New("company is required")
		}
		if in.Period == "" {
			return nil, nil, errors.New("period is required (YYYY-MM)")
		}
		if _, err := time.Parse("2006-01", in.Period); err != nil {
			return nil, nil, fmt.Errorf("invalid period %q: must be YYYY-MM", in.Period)
		}

		entries, err := st.ListGSTR2BEntries(ctx, in.Company, in.Period)
		if err != nil {
			return nil, nil, fmt.Errorf("list gstr2b entries: %w", err)
		}

		out := make([]GSTR2BEntryOutput, 0, len(entries))
		for _, e := range entries {
			if in.SupplierGSTIN != nil && *in.SupplierGSTIN != "" && !strings.EqualFold(e.SupplierGSTIN, *in.SupplierGSTIN) {
				continue
			}
			out = append(out, GSTR2BEntryOutput{
				SupplierGSTIN: e.SupplierGSTIN,
				SupplierName:  e.SupplierName,
				InvoiceNo:     e.InvoiceNo,
				InvoiceNoNorm: e.InvoiceNoNorm,
				InvoiceDate:   e.InvoiceDate.Format(dateLayout),
				Taxable:       e.TaxablePaise,
				IGST:          e.IGSTPaise,
				CGST:          e.CGSTPaise,
				SGST:          e.SGSTPaise,
				ITCAvailable:  e.ITCAvailable,
			})
		}

		return nil, out, nil
	})
}
