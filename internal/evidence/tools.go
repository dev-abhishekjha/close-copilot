// Package evidence handles evidence ingestion (bank statements, GSTR-2B) and provides
// MCP tool handlers for querying evidence records from Postgres.
//
// Built in CC-402, CC-403, CC-503, CC-805.
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/retrieval"
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

// Option configures RegisterTools.
type Option func(*options)

type options struct {
	scope    *mcpkit.Scoper
	searcher DocSearcher
}

// Document types search_documents accepts: the fixed list, never caller text.
const (
	DocTypePolicy    = "policy"
	DocTypeContract  = "contract"
	DocTypeCloseNote = "close_note"
)

// MaxSnippetChars is the most runes a search_documents snippet holds,
// including the ellipsis of a cut one.
const MaxSnippetChars = 600

const (
	maxQueryChars = 500
	defaultK      = 5
	maxK          = 10
)

// SearchDocumentsInput is the input of the search_documents MCP tool. There
// is no date argument: documents are not month-bound.
type SearchDocumentsInput struct {
	Query    string   `json:"query" jsonschema:"search text, at most 500 characters"`
	Company  string   `json:"company,omitempty" jsonschema:"company ID; defaults to the run's company, any other company is refused"`
	DocTypes []string `json:"doc_types,omitempty" jsonschema:"optional filter: any of policy, contract, close_note"`
	K        int      `json:"k,omitempty" jsonschema:"number of results, 1 to 10 (default 5)"`
}

// SearchDocumentsOutput is one search hit, best first.
type SearchDocumentsOutput struct {
	DocID   string `json:"doc_id"`
	DocType string `json:"doc_type"`
	Section string `json:"section"`
	Title   string `json:"title"`
	Snippet string `json:"snippet" jsonschema:"the chunk text, whitespace-collapsed, at most 600 characters"`
	// Score is the raw cross-encoder score as a JSON number. It is a
	// json.Number because the nofloat analyzer bans float types in this
	// package; the value is never computed on here.
	Score json.Number `json:"score" jsonschema:"raw cross-encoder rerank score"`
}

// DocSearcher is what search_documents needs from retrieval.Searcher.
type DocSearcher interface {
	Search(ctx context.Context, query, company string, docTypes []string, k int) ([]retrieval.Hit, error)
}

// WithSearcher gives search_documents its searcher. Without one the tool is
// listed but answers "document search is not configured".
func WithSearcher(s DocSearcher) Option {
	return func(o *options) { o.searcher = s }
}

// snippet collapses whitespace and cuts to MaxSnippetChars runes at a rune
// boundary; a cut snippet ends in an ellipsis within the limit.
func snippet(content string) string {
	s := strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(s) <= MaxSnippetChars {
		return s
	}
	r := []rune(s)
	return string(r[:MaxSnippetChars-1]) + "\u2026"
}

// scoreNumber wraps a rerank score's decimal text as a JSON number. A
// non-finite score (never produced by TEI) becomes 0.
func scoreNumber(text string) json.Number {
	n := json.Number(text)
	if _, err := json.Marshal(n); err != nil {
		return "0"
	}
	return n
}

// validDocTypes checks and de-duplicates doc_types against the fixed list.
func validDocTypes(in []string) ([]string, error) {
	allowed := []string{DocTypePolicy, DocTypeContract, DocTypeCloseNote}
	var out []string
	for _, d := range in {
		if !slices.Contains(allowed, d) {
			return nil, fmt.Errorf("invalid doc_types entry: must be one of %s", strings.Join(allowed, ", "))
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// WithScope binds every call to its run's company and window (CC-506): a
// call outside it gets an out_of_scope tool error before any Postgres
// read. Without it the tools are unscoped (stdio and tests only); the HTTP
// server always sets it.
func WithScope(s *mcpkit.Scoper) Option {
	return func(o *options) { o.scope = s }
}

// RegisterTools registers the evidence MCP tools on the provided MCP server.
func RegisterTools(server *mcp.Server, st *store.Store, opts ...Option) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	mcpkit.AddTool(server, &mcp.Tool{
		Name:        "list_bank_lines",
		Description: "List bank statement lines for a company within a date range, ordered by transaction date and txn_id.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListBankLinesInput) (*mcp.CallToolResult, []BankLineOutput, error) {
		if err := o.scope.CheckCompany(req, in.Company); err != nil {
			return nil, nil, err
		}
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
		if err := o.scope.CheckRange(req, in.Company, fromDate.Format(dateLayout), toDate.Format(dateLayout)); err != nil {
			return nil, nil, err
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

	mcpkit.AddTool(server, &mcp.Tool{
		Name:        "list_gstr2b_entries",
		Description: "List GSTR-2B inward supply entries for a company and return period (YYYY-MM), optionally filtered by supplier GSTIN.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListGSTR2BEntriesInput) (*mcp.CallToolResult, []GSTR2BEntryOutput, error) {
		if err := o.scope.CheckCompany(req, in.Company); err != nil {
			return nil, nil, err
		}
		if in.Company == "" {
			return nil, nil, errors.New("company is required")
		}
		if in.Period == "" {
			return nil, nil, errors.New("period is required (YYYY-MM)")
		}
		period, err := time.Parse("2006-01", in.Period)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid period %q: must be YYYY-MM", in.Period)
		}
		// The period reads the first to the last day of its month.
		if err := o.scope.CheckRange(req, in.Company, period.Format(dateLayout), period.AddDate(0, 1, -1).Format(dateLayout)); err != nil {
			return nil, nil, err
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

	// json.Number would be described as a string; the score is a number.
	searchOut, err := jsonschema.For[[]SearchDocumentsOutput](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.Number](): {Type: "number"}},
	})
	if err != nil {
		panic(fmt.Sprintf("evidence: search_documents output schema: %v", err)) // a programming error, caught by every test
	}
	mcpkit.AddTool(server, &mcp.Tool{
		Name:         "search_documents",
		OutputSchema: searchOut,
		Description:  "Search the policy, contract and close-note documents readable by the run's company; returns the best chunks with snippets of at most 600 characters.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SearchDocumentsInput) (*mcp.CallToolResult, []SearchDocumentsOutput, error) {
		company := in.Company
		if o.scope != nil {
			if company == "" {
				if sc, ok := o.scope.Scope(req); ok {
					company = sc.Company
				}
			}
			if err := o.scope.CheckCompany(req, company); err != nil {
				return nil, nil, err
			}
		} else if strings.TrimSpace(company) == "" {
			return nil, nil, errors.New("company is required")
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, nil, errors.New("query is required")
		}
		if utf8.RuneCountInString(in.Query) > maxQueryChars {
			return nil, nil, fmt.Errorf("query must be at most %d characters", maxQueryChars)
		}
		docTypes, err := validDocTypes(in.DocTypes)
		if err != nil {
			return nil, nil, err
		}
		k := in.K
		if k == 0 {
			k = defaultK
		}
		if k < 1 || k > maxK {
			return nil, nil, fmt.Errorf("k must be 1 to %d", maxK)
		}
		if o.searcher == nil {
			return nil, nil, errors.New("document search is not configured")
		}

		hits, err := o.searcher.Search(ctx, in.Query, company, docTypes, k)
		if err != nil {
			return nil, nil, fmt.Errorf("search documents: %w", err)
		}
		out := make([]SearchDocumentsOutput, 0, len(hits))
		for _, h := range hits {
			if h.CompanyID != nil && *h.CompanyID != company {
				slog.Warn("search_documents dropped a hit of another company",
					"doc_id", h.DocID, "hit_company", *h.CompanyID, "scope_company", company)
				continue
			}
			out = append(out, SearchDocumentsOutput{
				DocID: h.DocID, DocType: h.DocType, Section: h.Section, Title: h.Title,
				Snippet: snippet(h.Content), Score: scoreNumber(h.ScoreText()),
			})
		}
		return nil, out, nil
	})
}
