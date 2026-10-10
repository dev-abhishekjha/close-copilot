package agent

// MCP-backed readers (CC-702). MCPBooks implements checks.BooksReader over
// the seven books tools and MCPEvidence implements checks.EvidenceReader
// over the evidence tools, so the deterministic checks run unchanged over
// MCP. They share the Registry's sessions.
//
// The books tools' output structs live in internal/books, which imports the
// ERPNext client and so is off limits here (noerpimport). The private wire
// structs below mirror the tools' JSON instead; CC-505's schema goldens
// (internal/books/testdata/schemas, internal/evidence/testdata/schemas) pin
// that contract, and readers_test.go fails when a struct and its golden
// drift apart.
//
// Amounts decode straight into money.Paise (int64): a fraction or a float
// in the JSON is a decode error, never a rounding. Results are read from
// the tool's JSON text content, which the SDK fills from the exact output
// bytes; the SDK's own StructuredContent is a generic any whose numbers are
// float64, so it is never used for money.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Tool names on the two servers, as the readers call them.
const (
	toolGetTrialBalance        = "get_trial_balance"
	toolListGLEntries          = "list_gl_entries"
	toolListPurchaseInvoices   = "list_purchase_invoices"
	toolListSalesInvoices      = "list_sales_invoices"
	toolListPayments           = "list_payments"
	toolGetAccountHistory      = "get_account_history"
	toolListRecurringSuppliers = "list_recurring_suppliers"
	toolListBankLines          = "list_bank_lines"
	toolListGSTR2BEntries      = "list_gstr2b_entries"
)

// maxGLPages bounds the next_cursor loop of GLEntries, so a server that
// keeps handing out cursors can't make a reader page forever.
const maxGLPages = 1000

const (
	wireDate  = "2006-01-02"
	wireMonth = "2006-01"
)

// CompanyDirectory resolves a company ID ("sharma") to its stored record,
// whose ERPCompany is the name ERPNext knows the company by.
// *store.Store implements it.
type CompanyDirectory interface {
	GetCompany(ctx context.Context, id string) (store.Company, error)
}

var _ CompanyDirectory = (*store.Store)(nil)

// MCPBooks implements checks.BooksReader over the books MCP tools.
//
// The tools return submitted documents of the company only, so every
// document comes back with Docstatus 1 (and GL Entries with IsCancelled
// false). The tools take and echo the company ID; when Companies is set,
// the Company fields are filled with the ERPNext company name, as
// direct.Books does, and otherwise hold the company ID. GL Entries carry
// no FiscalYear: the tool's contract doesn't include it.
type MCPBooks struct {
	Registry  *Registry
	Companies CompanyDirectory
}

var _ checks.BooksReader = (*MCPBooks)(nil)

// MCPEvidence implements checks.EvidenceReader over the evidence MCP tools.
// Bank lines come back without ValueDate and SourceFile, which the tool's
// contract doesn't include.
type MCPEvidence struct {
	Registry *Registry
}

var _ checks.EvidenceReader = (*MCPEvidence)(nil)

// ---- wire structs: the tools' outputs as JSON ----

type wireTrialBalance struct {
	Company  string       `json:"company"`
	FromDate string       `json:"from_date"`
	ToDate   string       `json:"to_date"`
	Rows     []wireTBRow  `json:"rows"`
	Totals   wireTBTotals `json:"totals"`
}

type wireTBRow struct {
	Account     string      `json:"account"`
	AccountName string      `json:"account_name"`
	RootType    string      `json:"root_type"`
	Opening     money.Paise `json:"opening"`
	Debit       money.Paise `json:"debit"`
	Credit      money.Paise `json:"credit"`
	Closing     money.Paise `json:"closing"`
}

type wireTBTotals struct {
	Opening money.Paise `json:"opening"`
	Debit   money.Paise `json:"debit"`
	Credit  money.Paise `json:"credit"`
	Closing money.Paise `json:"closing"`
}

type wireGLEntries struct {
	Entries    []wireGLEntry `json:"entries"`
	NextCursor string        `json:"next_cursor"`
}

type wireGLEntry struct {
	Name        string      `json:"name"`
	Account     string      `json:"account"`
	Debit       money.Paise `json:"debit"`
	Credit      money.Paise `json:"credit"`
	PostingDate string      `json:"posting_date"`
	VoucherType string      `json:"voucher_type"`
	VoucherNo   string      `json:"voucher_no"`
	PartyType   string      `json:"party_type"`
	Party       string      `json:"party"`
	Against     string      `json:"against"`
	Remarks     string      `json:"remarks"`
	IsOpening   bool        `json:"is_opening"`
}

type wirePurchaseInvoices struct {
	Invoices []wirePurchaseInvoice `json:"invoices"`
}

type wirePurchaseInvoice struct {
	Name              string            `json:"name"`
	Supplier          string            `json:"supplier"`
	SupplierName      string            `json:"supplier_name"`
	SupplierGSTIN     string            `json:"supplier_gstin"`
	CompanyGSTIN      string            `json:"company_gstin"`
	PlaceOfSupply     string            `json:"place_of_supply"`
	BillNo            string            `json:"bill_no"`
	BillDate          string            `json:"bill_date"`
	PostingDate       string            `json:"posting_date"`
	CreditTo          string            `json:"credit_to"`
	IsReturn          bool              `json:"is_return"`
	Taxable           money.Paise       `json:"taxable"`
	IGST              money.Paise       `json:"igst"`
	CGST              money.Paise       `json:"cgst"`
	SGST              money.Paise       `json:"sgst"`
	GrandTotal        money.Paise       `json:"grand_total"`
	OutstandingAmount money.Paise       `json:"outstanding_amount"`
	Remarks           string            `json:"remarks"`
	Lines             []wireInvoiceLine `json:"lines"`
	Taxes             []wireTaxRow      `json:"taxes"`
}

// wireInvoiceLine is an item row of a purchase or sales invoice; account
// is the expense or the income account.
type wireInvoiceLine struct {
	Name        string      `json:"name"`
	ItemCode    string      `json:"item_code"`
	Description string      `json:"description"`
	Account     string      `json:"account"`
	Amount      money.Paise `json:"amount"`
}

type wireTaxRow struct {
	Name         string      `json:"name"`
	AccountHead  string      `json:"account_head"`
	ChargeType   string      `json:"charge_type"`
	Rate         json.Number `json:"rate"` // decimal text, e.g. "9" or "2.5"
	TaxAmount    money.Paise `json:"tax_amount"`
	AddDeductTax string      `json:"add_deduct_tax"`
	Category     string      `json:"category"`
	Description  string      `json:"description"`
	GSTTaxType   string      `json:"gst_tax_type"`
}

type wireSalesInvoices struct {
	Invoices []wireSalesInvoice `json:"invoices"`
}

type wireSalesInvoice struct {
	Name              string            `json:"name"`
	Customer          string            `json:"customer"`
	CustomerName      string            `json:"customer_name"`
	PostingDate       string            `json:"posting_date"`
	NetTotal          money.Paise       `json:"net_total"`
	GrandTotal        money.Paise       `json:"grand_total"`
	OutstandingAmount money.Paise       `json:"outstanding_amount"`
	DebitTo           string            `json:"debit_to"`
	CollectedVia      string            `json:"collected_via"`
	Remarks           string            `json:"remarks"`
	Lines             []wireInvoiceLine `json:"lines"`
	Taxes             []wireTaxRow      `json:"taxes"`
}

type wirePayments struct {
	Payments []wirePayment `json:"payments"`
}

type wirePayment struct {
	Name              string           `json:"name"`
	PaymentType       string           `json:"payment_type"`
	PartyType         string           `json:"party_type"`
	Party             string           `json:"party"`
	PartyName         string           `json:"party_name"`
	Amount            money.Paise      `json:"amount"`
	PaidAmount        money.Paise      `json:"paid_amount"`
	ReceivedAmount    money.Paise      `json:"received_amount"`
	PaidFrom          string           `json:"paid_from"`
	PaidTo            string           `json:"paid_to"`
	ReferenceNo       string           `json:"reference_no"`
	ReferenceDate     string           `json:"reference_date"`
	PostingDate       string           `json:"posting_date"`
	Remarks           string           `json:"remarks"`
	UnallocatedAmount money.Paise      `json:"unallocated_amount"`
	Invoices          []wirePaymentRef `json:"invoices"`
}

type wirePaymentRef struct {
	Name              string      `json:"name"`
	ReferenceDoctype  string      `json:"reference_doctype"`
	ReferenceName     string      `json:"reference_name"`
	AllocatedAmount   money.Paise `json:"allocated_amount"`
	TotalAmount       money.Paise `json:"total_amount"`
	OutstandingAmount money.Paise `json:"outstanding_amount"`
}

type wireAccountHistory struct {
	Account      string           `json:"account"`
	ThroughMonth string           `json:"through_month"`
	History      []wireMonthTotal `json:"history"`
}

type wireMonthTotal struct {
	Month  string      `json:"month"`
	Debit  money.Paise `json:"debit"`
	Credit money.Paise `json:"credit"`
	Net    money.Paise `json:"net"`
}

type wireRecurringSuppliers struct {
	Suppliers []wireRecurringSupplier `json:"suppliers"`
}

type wireRecurringSupplier struct {
	Supplier     string      `json:"supplier"`
	MedianAmount money.Paise `json:"median_amount"`
	TypicalDay   int         `json:"typical_day"`
	MonthsSeen   []string    `json:"months_seen"`
}

// list_bank_lines returns a JSON array of these (CC-503).
type wireBankLine struct {
	TxnID     string       `json:"txn_id"`
	Date      string       `json:"date"`
	Narration string       `json:"narration"`
	Ref       *string      `json:"ref"`
	Amount    money.Paise  `json:"amount"`
	Balance   *money.Paise `json:"balance"`
}

// list_gstr2b_entries returns a JSON array of these.
type wireGSTR2BEntry struct {
	SupplierGSTIN string      `json:"supplier_gstin"`
	SupplierName  *string     `json:"supplier_name"`
	InvoiceNo     string      `json:"invoice_no"`
	InvoiceNoNorm string      `json:"invoice_no_norm"`
	InvoiceDate   string      `json:"invoice_date"`
	Taxable       money.Paise `json:"taxable"`
	IGST          money.Paise `json:"igst"`
	CGST          money.Paise `json:"cgst"`
	SGST          money.Paise `json:"sgst"`
	ITCAvailable  bool        `json:"itc_available"`
}

// ---- tool inputs ----

type rangeArgs struct {
	Company  string `json:"company"`
	FromDate string `json:"from_date"`
	ToDate   string `json:"to_date"`
	Cursor   string `json:"cursor,omitempty"`
}

type historyArgs struct {
	Company      string `json:"company"`
	Account      string `json:"account"`
	Months       int    `json:"months"`
	ThroughMonth string `json:"through_month,omitempty"`
}

type recurringArgs struct {
	Company        string `json:"company"`
	BeforeMonth    string `json:"before_month"`
	LookbackMonths int    `json:"lookback_months"`
	MinOccurrences int    `json:"min_occurrences"`
	AmountBandPct  int    `json:"amount_band_pct"`
}

type periodArgs struct {
	Company string `json:"company"`
	Period  string `json:"period"`
}

// ---- calling and decoding ----

// callJSON calls server's tool with args and decodes its JSON result into
// out. Errors wrap one of the registry's error classes.
func (r *Registry) callJSON(ctx context.Context, server, tool string, args, out any) error {
	if r == nil {
		return fmt.Errorf("%s%s%s: %w", server, toolSep, tool, ErrUnavailable)
	}
	if _, ok := r.tools[server+toolSep+tool]; !ok {
		return fmt.Errorf("%s%s%s: %w", server, toolSep, tool, ErrUnknownTool)
	}
	res, err := r.call(ctx, toolRef{server: server, tool: tool}, args)
	if err != nil {
		return err
	}
	raw, err := resultJSON(res)
	if err != nil {
		return fmt.Errorf("%s%s%s: %w", server, toolSep, tool, err)
	}
	if err := decodeWire(raw, out); err != nil {
		return fmt.Errorf("%s%s%s: %w", server, toolSep, tool, err)
	}
	return nil
}

// resultJSON returns the JSON text of a structured tool result: its first
// text content that is valid JSON.
func resultJSON(res *mcp.CallToolResult) ([]byte, error) {
	for _, c := range res.Content {
		t, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		b := []byte(t.Text)
		if json.Valid(b) {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w: no JSON text content", ErrBadResult)
}

// decodeWire decodes one JSON value into out. Amounts are int64 paise, so
// a fractional or exponent number fails here rather than rounding.
func decodeWire(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(out); err != nil {
		return wireError(err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", ErrBadResult)
	}
	return nil
}

// wireError maps a JSON decode error to ErrBadResult plus, for a type
// mismatch, the field path (our own JSON names). Tool results are
// untrusted ERPNext text, so the offending value never goes into the
// error: not the literal, not the offset's context.
func wireError(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return fmt.Errorf("%w: field %s does not decode", ErrBadResult, te.Field)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("%w: invalid JSON", ErrBadResult)
	}
	return fmt.Errorf("%w: does not decode", ErrBadResult)
}

// ident is a document identifier from a tool result for an error: quoted
// and cut to 40 characters, since it is untrusted text.
func ident(s string) string {
	return fmt.Sprintf("%.40q", s)
}

// parseDate parses a required YYYY-MM-DD date to UTC midnight. The error
// names the field only, never the untrusted value.
func parseDate(field, s string) (time.Time, error) {
	t, err := time.ParseInLocation(wireDate, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s is not YYYY-MM-DD", ErrBadResult, field)
	}
	return t, nil
}

// parseOptDate is parseDate with "" as the zero time.
func parseOptDate(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseDate(field, s)
}

// dateArgs formats a required date range for a tool.
func dateArgs(what string, from, to time.Time) (string, string, error) {
	if from.IsZero() || to.IsZero() {
		return "", "", fmt.Errorf("agent: %s: the MCP tools need both a from and a to date", what)
	}
	return from.UTC().Format(wireDate), to.UTC().Format(wireDate), nil
}

// erpCompany is the value for the Company fields of the documents read for
// company: the ERPNext name when Companies is set, else the ID.
func (b *MCPBooks) erpCompany(ctx context.Context, company string) (string, error) {
	if b.Registry == nil {
		return "", errors.New("agent: MCPBooks has nil registry")
	}
	if company == "" {
		return "", errors.New("agent: MCPBooks: empty company ID")
	}
	if b.Companies == nil {
		return company, nil
	}
	c, err := b.Companies.GetCompany(ctx, company)
	if err != nil {
		return "", fmt.Errorf("agent: resolve company %q: %w", company, err)
	}
	if c.ERPCompany == "" {
		return "", fmt.Errorf("agent: resolve company %q: no ERPNext company name", company)
	}
	return c.ERPCompany, nil
}

// ---- MCPBooks ----

// TrialBalance reads get_trial_balance.
func (b *MCPBooks) TrialBalance(ctx context.Context, company string, from, to time.Time) (ledger.TB, error) {
	erp, err := b.erpCompany(ctx, company)
	if err != nil {
		return ledger.TB{}, err
	}
	f, t, err := dateArgs("trial balance", from, to)
	if err != nil {
		return ledger.TB{}, err
	}
	var w wireTrialBalance
	if err := b.Registry.callJSON(ctx, ServerBooks, toolGetTrialBalance, rangeArgs{Company: company, FromDate: f, ToDate: t}, &w); err != nil {
		return ledger.TB{}, fmt.Errorf("agent: trial balance: %w", err)
	}
	return w.toLedger(erp)
}

func (w wireTrialBalance) toLedger(company string) (ledger.TB, error) {
	from, err := parseDate("from_date", w.FromDate)
	if err != nil {
		return ledger.TB{}, err
	}
	to, err := parseDate("to_date", w.ToDate)
	if err != nil {
		return ledger.TB{}, err
	}
	tb := ledger.TB{
		Company: company,
		From:    from,
		To:      to,
		Rows:    make([]ledger.TBRow, 0, len(w.Rows)),
		Totals:  ledger.TBTotals(w.Totals),
	}
	for _, r := range w.Rows {
		tb.Rows = append(tb.Rows, ledger.TBRow(r))
	}
	return tb, nil
}

// GLEntries reads every page of list_gl_entries, following next_cursor
// until it is empty.
func (b *MCPBooks) GLEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error) {
	erp, err := b.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	f, t, err := dateArgs("gl entries", from, to)
	if err != nil {
		return nil, err
	}
	out := []ledger.GLEntry{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxGLPages {
			return nil, fmt.Errorf("agent: gl entries: stopped after %d pages: %w", maxGLPages, ErrBadResult)
		}
		var w wireGLEntries
		args := rangeArgs{Company: company, FromDate: f, ToDate: t, Cursor: cursor}
		if err := b.Registry.callJSON(ctx, ServerBooks, toolListGLEntries, args, &w); err != nil {
			return nil, fmt.Errorf("agent: gl entries: %w", err)
		}
		for _, e := range w.Entries {
			g, err := e.toLedger(erp)
			if err != nil {
				return nil, fmt.Errorf("agent: gl entries: %w", err)
			}
			out = append(out, g)
		}
		if w.NextCursor == "" {
			return out, nil
		}
		if seen[w.NextCursor] {
			return nil, fmt.Errorf("agent: gl entries: next_cursor repeats: %w", ErrBadResult)
		}
		seen[w.NextCursor] = true
		cursor = w.NextCursor
	}
}

func (e wireGLEntry) toLedger(company string) (ledger.GLEntry, error) {
	posting, err := parseDate("posting_date", e.PostingDate)
	if err != nil {
		return ledger.GLEntry{}, fmt.Errorf("gl entry %s: %w", ident(e.Name), err)
	}
	return ledger.GLEntry{
		Name:        e.Name,
		Docstatus:   1,
		Company:     company,
		Account:     e.Account,
		Debit:       e.Debit,
		Credit:      e.Credit,
		PostingDate: posting,
		VoucherType: e.VoucherType,
		VoucherNo:   e.VoucherNo,
		PartyType:   e.PartyType,
		Party:       e.Party,
		IsOpening:   e.IsOpening,
		Against:     e.Against,
		Remarks:     e.Remarks,
	}, nil
}

// PurchaseInvoices reads list_purchase_invoices. It returns nil when there
// are none.
func (b *MCPBooks) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	erp, err := b.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	f, t, err := dateArgs("purchase invoices", from, to)
	if err != nil {
		return nil, err
	}
	var w wirePurchaseInvoices
	if err := b.Registry.callJSON(ctx, ServerBooks, toolListPurchaseInvoices, rangeArgs{Company: company, FromDate: f, ToDate: t}, &w); err != nil {
		return nil, fmt.Errorf("agent: purchase invoices: %w", err)
	}
	return w.toLedger(erp)
}

func (w wirePurchaseInvoices) toLedger(company string) ([]ledger.PurchaseInvoice, error) {
	if len(w.Invoices) == 0 {
		return nil, nil
	}
	out := make([]ledger.PurchaseInvoice, 0, len(w.Invoices))
	for _, p := range w.Invoices {
		posting, err := parseDate("posting_date", p.PostingDate)
		if err != nil {
			return nil, fmt.Errorf("purchase invoice %s: %w", ident(p.Name), err)
		}
		bill, err := parseOptDate("bill_date", p.BillDate)
		if err != nil {
			return nil, fmt.Errorf("purchase invoice %s: %w", ident(p.Name), err)
		}
		inv := ledger.PurchaseInvoice{
			Name:              p.Name,
			Docstatus:         1,
			Company:           company,
			Supplier:          p.Supplier,
			SupplierName:      p.SupplierName,
			BillNo:            p.BillNo,
			BillDate:          bill,
			PostingDate:       posting,
			Remarks:           p.Remarks,
			CreditTo:          p.CreditTo,
			NetTotal:          p.Taxable,
			GrandTotal:        p.GrandTotal,
			OutstandingAmount: p.OutstandingAmount,
			IsReturn:          p.IsReturn,
			SupplierGSTIN:     p.SupplierGSTIN,
			CompanyGSTIN:      p.CompanyGSTIN,
			PlaceOfSupply:     p.PlaceOfSupply,
		}
		if p.Lines != nil {
			inv.Items = make([]ledger.PurchaseInvoiceItem, len(p.Lines))
			for i, l := range p.Lines {
				inv.Items[i] = ledger.PurchaseInvoiceItem{
					Name:           l.Name,
					ItemCode:       l.ItemCode,
					Description:    l.Description,
					ExpenseAccount: l.Account,
					Amount:         l.Amount,
				}
			}
		}
		if p.Taxes != nil {
			inv.Taxes = make([]ledger.PurchaseTaxesAndCharges, len(p.Taxes))
			for i, t := range p.Taxes {
				inv.Taxes[i] = ledger.PurchaseTaxesAndCharges{
					Name:         t.Name,
					AccountHead:  t.AccountHead,
					TaxAmount:    t.TaxAmount,
					ChargeType:   t.ChargeType,
					Rate:         t.Rate,
					AddDeductTax: t.AddDeductTax,
					Category:     t.Category,
					Description:  t.Description,
					GSTTaxType:   t.GSTTaxType,
				}
			}
		}
		out = append(out, inv)
	}
	return out, nil
}

// SalesInvoices reads list_sales_invoices. It returns nil when there are
// none.
func (b *MCPBooks) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	erp, err := b.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	f, t, err := dateArgs("sales invoices", from, to)
	if err != nil {
		return nil, err
	}
	var w wireSalesInvoices
	if err := b.Registry.callJSON(ctx, ServerBooks, toolListSalesInvoices, rangeArgs{Company: company, FromDate: f, ToDate: t}, &w); err != nil {
		return nil, fmt.Errorf("agent: sales invoices: %w", err)
	}
	return w.toLedger(erp)
}

func (w wireSalesInvoices) toLedger(company string) ([]ledger.SalesInvoice, error) {
	if len(w.Invoices) == 0 {
		return nil, nil
	}
	out := make([]ledger.SalesInvoice, 0, len(w.Invoices))
	for _, s := range w.Invoices {
		posting, err := parseDate("posting_date", s.PostingDate)
		if err != nil {
			return nil, fmt.Errorf("sales invoice %s: %w", ident(s.Name), err)
		}
		inv := ledger.SalesInvoice{
			Name:              s.Name,
			Docstatus:         1,
			Company:           company,
			Customer:          s.Customer,
			CustomerName:      s.CustomerName,
			PostingDate:       posting,
			NetTotal:          s.NetTotal,
			GrandTotal:        s.GrandTotal,
			OutstandingAmount: s.OutstandingAmount,
			DebitTo:           s.DebitTo,
			Remarks:           s.Remarks,
		}
		if s.Lines != nil {
			inv.Items = make([]ledger.SalesInvoiceItem, len(s.Lines))
			for i, l := range s.Lines {
				inv.Items[i] = ledger.SalesInvoiceItem{
					Name:          l.Name,
					ItemCode:      l.ItemCode,
					Description:   l.Description,
					IncomeAccount: l.Account,
					Amount:        l.Amount,
				}
			}
		}
		if s.Taxes != nil {
			inv.Taxes = make([]ledger.SalesTaxesAndCharges, len(s.Taxes))
			for i, t := range s.Taxes {
				inv.Taxes[i] = ledger.SalesTaxesAndCharges{
					Name:        t.Name,
					AccountHead: t.AccountHead,
					TaxAmount:   t.TaxAmount,
					ChargeType:  t.ChargeType,
					Rate:        t.Rate,
					Description: t.Description,
					GSTTaxType:  t.GSTTaxType,
				}
			}
		}
		out = append(out, inv)
	}
	return out, nil
}

// PaymentEntries reads list_payments. It returns nil when there are none.
func (b *MCPBooks) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	erp, err := b.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	f, t, err := dateArgs("payment entries", from, to)
	if err != nil {
		return nil, err
	}
	var w wirePayments
	if err := b.Registry.callJSON(ctx, ServerBooks, toolListPayments, rangeArgs{Company: company, FromDate: f, ToDate: t}, &w); err != nil {
		return nil, fmt.Errorf("agent: payment entries: %w", err)
	}
	return w.toLedger(erp)
}

func (w wirePayments) toLedger(company string) ([]ledger.PaymentEntry, error) {
	if len(w.Payments) == 0 {
		return nil, nil
	}
	out := make([]ledger.PaymentEntry, 0, len(w.Payments))
	for _, p := range w.Payments {
		posting, err := parseDate("posting_date", p.PostingDate)
		if err != nil {
			return nil, fmt.Errorf("payment entry %s: %w", ident(p.Name), err)
		}
		refDate, err := parseOptDate("reference_date", p.ReferenceDate)
		if err != nil {
			return nil, fmt.Errorf("payment entry %s: %w", ident(p.Name), err)
		}
		pe := ledger.PaymentEntry{
			Name:              p.Name,
			Docstatus:         1,
			Company:           company,
			PaymentType:       p.PaymentType,
			PartyType:         p.PartyType,
			Party:             p.Party,
			PartyName:         p.PartyName,
			PaidAmount:        p.PaidAmount,
			ReceivedAmount:    p.ReceivedAmount,
			PaidFrom:          p.PaidFrom,
			PaidTo:            p.PaidTo,
			ReferenceNo:       p.ReferenceNo,
			ReferenceDate:     refDate,
			PostingDate:       posting,
			Remarks:           p.Remarks,
			UnallocatedAmount: p.UnallocatedAmount,
		}
		if p.Invoices != nil {
			pe.References = make([]ledger.PaymentEntryReference, len(p.Invoices))
			for i, r := range p.Invoices {
				pe.References[i] = ledger.PaymentEntryReference(r)
			}
		}
		out = append(out, pe)
	}
	return out, nil
}

// AccountHistory reads get_account_history.
func (b *MCPBooks) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error) {
	if _, err := b.erpCompany(ctx, company); err != nil {
		return nil, err
	}
	var w wireAccountHistory
	args := historyArgs{Company: company, Account: account, Months: months, ThroughMonth: through}
	if err := b.Registry.callJSON(ctx, ServerBooks, toolGetAccountHistory, args, &w); err != nil {
		return nil, fmt.Errorf("agent: account history: %w", err)
	}
	return w.toLedger()
}

func (w wireAccountHistory) toLedger() ([]ledger.MonthTotal, error) {
	out := make([]ledger.MonthTotal, 0, len(w.History))
	for _, m := range w.History {
		if _, err := time.Parse(wireMonth, m.Month); err != nil {
			return nil, fmt.Errorf("%w: history month is not YYYY-MM", ErrBadResult)
		}
		out = append(out, ledger.MonthTotal(m))
	}
	return out, nil
}

// RecurringSuppliers reads list_recurring_suppliers. It returns nil when
// no supplier qualifies.
func (b *MCPBooks) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	if _, err := b.erpCompany(ctx, company); err != nil {
		return nil, err
	}
	var w wireRecurringSuppliers
	args := recurringArgs{
		Company:        company,
		BeforeMonth:    beforeMonth,
		LookbackMonths: lookbackMonths,
		MinOccurrences: minOccurrences,
		AmountBandPct:  amountBandPct,
	}
	if err := b.Registry.callJSON(ctx, ServerBooks, toolListRecurringSuppliers, args, &w); err != nil {
		return nil, fmt.Errorf("agent: recurring suppliers: %w", err)
	}
	return w.toLedger(), nil
}

func (w wireRecurringSuppliers) toLedger() []checks.RecurringSupplier {
	if len(w.Suppliers) == 0 {
		return nil
	}
	out := make([]checks.RecurringSupplier, len(w.Suppliers))
	for i, s := range w.Suppliers {
		out[i] = ledger.RecurringSupplier(s)
	}
	return out
}

// ---- MCPEvidence ----

// BankLines reads list_bank_lines for the date range (both dates required).
func (e *MCPEvidence) BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	if e.Registry == nil {
		return nil, errors.New("agent: MCPEvidence has nil registry")
	}
	if company == "" {
		return nil, errors.New("agent: MCPEvidence: empty company ID")
	}
	f, t, err := dateArgs("bank lines", from, to)
	if err != nil {
		return nil, err
	}
	var w []wireBankLine
	if err := e.Registry.callJSON(ctx, ServerEvidence, toolListBankLines, rangeArgs{Company: company, FromDate: f, ToDate: t}, &w); err != nil {
		return nil, fmt.Errorf("agent: bank lines: %w", err)
	}
	return bankLinesToStore(company, w)
}

func bankLinesToStore(company string, w []wireBankLine) ([]store.BankLine, error) {
	out := make([]store.BankLine, 0, len(w))
	for _, l := range w {
		d, err := parseDate("date", l.Date)
		if err != nil {
			return nil, fmt.Errorf("bank line %s: %w", ident(l.TxnID), err)
		}
		out = append(out, store.BankLine{
			CompanyID:    company,
			TxnID:        l.TxnID,
			TxnDate:      d,
			Narration:    l.Narration,
			Ref:          l.Ref,
			AmountPaise:  l.Amount,
			BalancePaise: l.Balance,
		})
	}
	return out, nil
}

// GSTR2BEntries reads list_gstr2b_entries for the return period (YYYY-MM).
func (e *MCPEvidence) GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error) {
	if e.Registry == nil {
		return nil, errors.New("agent: MCPEvidence has nil registry")
	}
	if company == "" {
		return nil, errors.New("agent: MCPEvidence: empty company ID")
	}
	var w []wireGSTR2BEntry
	if err := e.Registry.callJSON(ctx, ServerEvidence, toolListGSTR2BEntries, periodArgs{Company: company, Period: period}, &w); err != nil {
		return nil, fmt.Errorf("agent: gstr2b entries: %w", err)
	}
	return gstr2bToStore(company, period, w)
}

func gstr2bToStore(company, period string, w []wireGSTR2BEntry) ([]store.GSTR2BEntry, error) {
	out := make([]store.GSTR2BEntry, 0, len(w))
	for _, g := range w {
		d, err := parseDate("invoice_date", g.InvoiceDate)
		if err != nil {
			return nil, fmt.Errorf("gstr-2b entry %s: %w", ident(g.InvoiceNo), err)
		}
		out = append(out, store.GSTR2BEntry{
			CompanyID:     company,
			Period:        period,
			SupplierGSTIN: g.SupplierGSTIN,
			SupplierName:  g.SupplierName,
			InvoiceNo:     g.InvoiceNo,
			InvoiceNoNorm: g.InvoiceNoNorm,
			InvoiceDate:   d,
			TaxablePaise:  g.Taxable,
			IGSTPaise:     g.IGST,
			CGSTPaise:     g.CGST,
			SGSTPaise:     g.SGST,
			ITCAvailable:  g.ITCAvailable,
		})
	}
	return out, nil
}
