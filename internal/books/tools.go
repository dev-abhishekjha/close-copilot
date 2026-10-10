package books

// The seven read-only books tools on /mcp (CC-502). Each tool has one typed
// input and one typed output struct; the SDK derives the JSON Schemas from
// them, so the structs and their jsonschema tags are the contract the model
// sees. Unknown input properties are refused by the schema (structs get
// additionalProperties false), and every handler validates its input again
// before anything reaches ERPNext.
//
// No tool takes raw Frappe fields or order_by. The account, party and
// supplier filters are sent to Frappe only as filter values, never as field
// names, and the client's query check (CC-205) is the second line of
// defence.
//
// Every tool takes the company ID ("sharma"), resolves it to the ERPNext
// company name through a CompanyResolver, and reads submitted documents
// only (docstatus 1). Amounts are money.Paise and dates YYYY-MM-DD.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

// Tool names, as listed on /mcp.
const (
	ToolGetTrialBalance        = "get_trial_balance"
	ToolListGLEntries          = "list_gl_entries"
	ToolListPurchaseInvoices   = "list_purchase_invoices"
	ToolListSalesInvoices      = "list_sales_invoices"
	ToolListPayments           = "list_payments"
	ToolGetAccountHistory      = "get_account_history"
	ToolListRecurringSuppliers = "list_recurring_suppliers"
)

// ToolNames lists the read-only tools RegisterTools adds, in catalog order.
var ToolNames = []string{
	ToolGetTrialBalance,
	ToolListGLEntries,
	ToolListPurchaseInvoices,
	ToolListSalesInvoices,
	ToolListPayments,
	ToolGetAccountHistory,
	ToolListRecurringSuppliers,
}

// ErrUnknownCompany is wrapped when a company ID has no profile.
var ErrUnknownCompany = errors.New("unknown company ID")

// CompanyResolver maps a company ID ("sharma") to the name ERPNext knows
// the company by ("Sharma Traders Pvt Ltd").
type CompanyResolver interface {
	ERPCompany(id string) (string, error)
}

// profileCompanies is a CompanyResolver over the company profiles.
type profileCompanies map[string]string

// ProfileCompanies resolves company IDs through the profiles' erp_company
// fields (config/companies/<id>.yaml, loaded with seed.LoadProfiles).
func ProfileCompanies(profiles []seed.Profile) CompanyResolver {
	m := make(profileCompanies, len(profiles))
	for _, p := range profiles {
		m[p.ID] = p.ERPCompany
	}
	return m
}

// ERPCompany returns the ERPNext company name of id, or an error wrapping
// ErrUnknownCompany.
func (p profileCompanies) ERPCompany(id string) (string, error) {
	name, ok := p[id]
	if !ok || name == "" {
		return "", fmt.Errorf("%w %q", ErrUnknownCompany, id)
	}
	return name, nil
}

// ToolDeps are what the books tools need. Client should hold the bot key
// (ERP_API_KEY, Accounts User); the tools only read. Now, when nil, is
// time.Now; get_account_history uses it for its default through_month.
// Logger receives the full error of every failed call (the model sees
// only a fixed message); nil means slog.Default().
type ToolDeps struct {
	Client    *frappe.Client
	Companies CompanyResolver
	Now       func() time.Time
	Logger    *slog.Logger
}

// Limits on tool input.
const (
	// GLPageSize is the number of GL Entries list_gl_entries returns a page.
	GLPageSize = 500
	// MaxRangeDays is the longest from_date..to_date range, both days
	// included.
	MaxRangeDays = 400
	// MaxToolMonths bounds months, lookback_months and min_occurrences.
	MaxToolMonths = 12
	// MaxFilterLen is the longest account, party or supplier filter, in
	// characters.
	MaxFilterLen = 140
	// nameChunk is how many invoice names one Payment Entry lookup sends,
	// so the query string stays well under a proxy's line limit.
	nameChunk = 100
)

// readOnly is the annotation every tool here carries.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true}
}

// RegisterTools adds the seven read-only books tools to s.
func RegisterTools(s *mcp.Server, deps ToolDeps) {
	h := &handlers{deps: deps, timeout: toolTimeout}
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolGetTrialBalance,
		Description: "Trial balance of the company for a date range, computed from submitted GL Entries: " +
			"one row per leaf account with a non-zero value, giving the opening balance, the period's debit and credit totals " +
			"and the closing balance, plus column totals. Balances are signed, debit positive and credit negative; " +
			"profit-and-loss accounts open at zero on 1 April (Indian fiscal year). " +
			"Use it to see account balances at month end or to find which accounts moved. " +
			"All amounts are integers in paise (1 rupee = 100 paise). Dates are YYYY-MM-DD; the range is at most 400 days.",
		Annotations: readOnly(),
	}, tool(h, h.getTrialBalance))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListGLEntries,
		Description: "General-ledger entries (submitted, not cancelled) of the company posted in a date range, ordered by entry name, " +
			"500 a page. Optionally filter by exact account name (e.g. \"HDFC Current 0001 - STPL\") or exact party name. " +
			"If next_cursor is non-empty, call again with the same arguments and cursor set to it for the next page; " +
			"an empty next_cursor means the last page. Use it to trace the postings behind a balance or a voucher. " +
			"Debit and credit are integers in paise (1 rupee = 100 paise). Dates are YYYY-MM-DD; the range is at most 400 days.",
		Annotations: readOnly(),
	}, tool(h, h.listGLEntries))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListPurchaseInvoices,
		Description: "Submitted purchase invoices (supplier bills) of the company posted in a date range, optionally for one supplier, " +
			"with the supplier's GSTIN, bill number and date, taxable value, IGST, CGST and SGST, each expense line " +
			"(account, description, amount) and each tax row. Use it to check bills against GSTR-2B, find duplicate bills " +
			"or see what a supplier charged. All amounts are integers in paise (1 rupee = 100 paise). " +
			"Dates are YYYY-MM-DD; the range is at most 400 days.",
		Annotations: readOnly(),
	}, tool(h, h.listPurchaseInvoices))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListSalesInvoices,
		Description: "Submitted sales invoices of the company posted in a date range, with customer, totals, outstanding amount, " +
			"lines and tax rows, and collected_via: the account the customer's payment went into (the bank account, or " +
			"Payment Gateway Clearing for gateway collections), empty while unpaid. Use it to follow sales into bank or " +
			"gateway receipts. All amounts are integers in paise (1 rupee = 100 paise). " +
			"Dates are YYYY-MM-DD; the range is at most 400 days.",
		Annotations: readOnly(),
	}, tool(h, h.listSalesInvoices))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListPayments,
		Description: "Submitted payment entries (payments to suppliers, receipts from customers, internal transfers) of the company " +
			"posted in a date range, optionally for one party (exact party name), with amount, bank reference number, " +
			"accounts and the invoices each payment settles. Use it to match bank lines to payments or to see how an " +
			"invoice was paid. All amounts are integers in paise (1 rupee = 100 paise). " +
			"Dates are YYYY-MM-DD; the range is at most 400 days.",
		Annotations: readOnly(),
	}, tool(h, h.listPayments))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolGetAccountHistory,
		Description: "Monthly debit, credit and net (debit minus credit) of one leaf account over the last 1 to 12 calendar months " +
			"ending with through_month (default: the current month), oldest first, with zeros for months without entries. " +
			"Use it to compare this month with earlier months, for example to spot a missing or unusual charge. " +
			"Amounts are integers in paise (1 rupee = 100 paise); months are YYYY-MM.",
		Annotations: readOnly(),
	}, tool(h, h.getAccountHistory))
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListRecurringSuppliers,
		Description: "Suppliers that billed regularly in the lookback_months calendar months before before_month: billed in at least " +
			"min_occurrences distinct months with every bill within amount_band_pct percent (default 20) of their median. " +
			"Returns each supplier's median bill, typical billing day and the months seen. Use it to expect this month's " +
			"recurring bills (rent, utilities, subscriptions) and spot one that is missing. " +
			"Amounts are integers in paise (1 rupee = 100 paise); months are YYYY-MM.",
		Annotations: readOnly(),
	}, tool(h, h.listRecurringSuppliers))
}

type handlers struct {
	deps    ToolDeps
	timeout time.Duration // the whole call's deadline, ToolTimeout
}

// ---- inputs and outputs ----

// TrialBalanceInput is the input of get_trial_balance.
type TrialBalanceInput struct {
	Company  string `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate string `json:"from_date" jsonschema:"first day of the period, YYYY-MM-DD"`
	ToDate   string `json:"to_date" jsonschema:"last day of the period, YYYY-MM-DD, inclusive"`
}

// TrialBalanceOutput is the output of get_trial_balance. Rows and Totals
// follow TB.
type TrialBalanceOutput struct {
	Company  string   `json:"company" jsonschema:"company ID"`
	FromDate string   `json:"from_date" jsonschema:"first day of the period, YYYY-MM-DD"`
	ToDate   string   `json:"to_date" jsonschema:"last day of the period, YYYY-MM-DD"`
	Rows     []TBRow  `json:"rows" jsonschema:"one row per leaf account with a non-zero value; amounts in paise, balances debit positive"`
	Totals   TBTotals `json:"totals" jsonschema:"column totals in paise; a balanced ledger has debit equal to credit and zero opening and closing"`
}

// GLEntriesInput is the input of list_gl_entries.
type GLEntriesInput struct {
	Company  string `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate string `json:"from_date" jsonschema:"first posting date, YYYY-MM-DD"`
	ToDate   string `json:"to_date" jsonschema:"last posting date, YYYY-MM-DD, inclusive"`
	Account  string `json:"account,omitempty" jsonschema:"optional exact account name, e.g. HDFC Current 0001 - STPL"`
	Party    string `json:"party,omitempty" jsonschema:"optional exact party name (a Supplier or Customer ID)"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page; omit for the first page"`
}

// GLEntriesOutput is one page of list_gl_entries.
type GLEntriesOutput struct {
	Entries    []GLEntryOut `json:"entries" jsonschema:"GL Entries ordered by name"`
	NextCursor string       `json:"next_cursor" jsonschema:"pass as cursor to get the next page; empty on the last page"`
}

// GLEntryOut is one GL Entry.
type GLEntryOut struct {
	Name        string      `json:"name" jsonschema:"GL Entry name"`
	Account     string      `json:"account"`
	Debit       money.Paise `json:"debit" jsonschema:"paise"`
	Credit      money.Paise `json:"credit" jsonschema:"paise"`
	PostingDate string      `json:"posting_date" jsonschema:"YYYY-MM-DD"`
	VoucherType string      `json:"voucher_type" jsonschema:"e.g. Journal Entry, Purchase Invoice, Payment Entry"`
	VoucherNo   string      `json:"voucher_no"`
	PartyType   string      `json:"party_type,omitempty"`
	Party       string      `json:"party,omitempty"`
	Against     string      `json:"against,omitempty" jsonschema:"the other accounts or parties of the voucher"`
	Remarks     string      `json:"remarks,omitempty"`
	IsOpening   bool        `json:"is_opening" jsonschema:"true for an opening entry"`
}

// PurchaseInvoicesInput is the input of list_purchase_invoices.
type PurchaseInvoicesInput struct {
	Company  string `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate string `json:"from_date" jsonschema:"first posting date, YYYY-MM-DD"`
	ToDate   string `json:"to_date" jsonschema:"last posting date, YYYY-MM-DD, inclusive"`
	Supplier string `json:"supplier,omitempty" jsonschema:"optional exact Supplier name (ID)"`
}

// PurchaseInvoicesOutput is the output of list_purchase_invoices.
type PurchaseInvoicesOutput struct {
	Invoices []PurchaseInvoiceOut `json:"invoices" jsonschema:"invoices ordered by posting date, then name"`
}

// PurchaseInvoiceOut is one submitted Purchase Invoice.
type PurchaseInvoiceOut struct {
	Name              string            `json:"name"`
	Supplier          string            `json:"supplier" jsonschema:"Supplier name (ID)"`
	SupplierName      string            `json:"supplier_name,omitempty"`
	SupplierGSTIN     string            `json:"supplier_gstin,omitempty"`
	CompanyGSTIN      string            `json:"company_gstin,omitempty"`
	PlaceOfSupply     string            `json:"place_of_supply,omitempty"`
	BillNo            string            `json:"bill_no,omitempty" jsonschema:"the supplier's invoice number"`
	BillDate          string            `json:"bill_date,omitempty" jsonschema:"the supplier's invoice date, YYYY-MM-DD"`
	PostingDate       string            `json:"posting_date" jsonschema:"YYYY-MM-DD"`
	CreditTo          string            `json:"credit_to,omitempty" jsonschema:"payable account"`
	IsReturn          bool              `json:"is_return" jsonschema:"true for a debit note"`
	Taxable           money.Paise       `json:"taxable" jsonschema:"net total before tax, paise"`
	IGST              money.Paise       `json:"igst" jsonschema:"paise"`
	CGST              money.Paise       `json:"cgst" jsonschema:"paise"`
	SGST              money.Paise       `json:"sgst" jsonschema:"paise"`
	GrandTotal        money.Paise       `json:"grand_total" jsonschema:"paise"`
	OutstandingAmount money.Paise       `json:"outstanding_amount" jsonschema:"unpaid amount, paise"`
	Remarks           string            `json:"remarks,omitempty"`
	Lines             []PurchaseLineOut `json:"lines" jsonschema:"item rows"`
	Taxes             []TaxRowOut       `json:"taxes" jsonschema:"tax rows"`
}

// PurchaseLineOut is one Purchase Invoice Item row.
type PurchaseLineOut struct {
	Name        string      `json:"name"`
	ItemCode    string      `json:"item_code,omitempty"`
	Description string      `json:"description,omitempty"`
	Account     string      `json:"account" jsonschema:"expense account"`
	Amount      money.Paise `json:"amount" jsonschema:"paise"`
}

// TaxRowOut is one tax row of an invoice. AddDeductTax and Category are
// empty on sales invoices.
type TaxRowOut struct {
	Name         string      `json:"name"`
	AccountHead  string      `json:"account_head"`
	ChargeType   string      `json:"charge_type,omitempty"`
	Rate         string      `json:"rate,omitempty" jsonschema:"percentage as decimal text, e.g. 9 or 2.5"`
	TaxAmount    money.Paise `json:"tax_amount" jsonschema:"paise"`
	AddDeductTax string      `json:"add_deduct_tax,omitempty" jsonschema:"Add or Deduct"`
	Category     string      `json:"category,omitempty"`
	Description  string      `json:"description,omitempty"`
	GSTTaxType   string      `json:"gst_tax_type,omitempty" jsonschema:"igst, cgst, sgst or another India Compliance tax type"`
}

// SalesInvoicesInput is the input of list_sales_invoices.
type SalesInvoicesInput struct {
	Company  string `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate string `json:"from_date" jsonschema:"first posting date, YYYY-MM-DD"`
	ToDate   string `json:"to_date" jsonschema:"last posting date, YYYY-MM-DD, inclusive"`
}

// SalesInvoicesOutput is the output of list_sales_invoices.
type SalesInvoicesOutput struct {
	Invoices []SalesInvoiceOut `json:"invoices" jsonschema:"invoices ordered by posting date, then name"`
}

// SalesInvoiceOut is one submitted Sales Invoice.
type SalesInvoiceOut struct {
	Name              string         `json:"name"`
	Customer          string         `json:"customer" jsonschema:"Customer name (ID)"`
	CustomerName      string         `json:"customer_name,omitempty"`
	PostingDate       string         `json:"posting_date" jsonschema:"YYYY-MM-DD"`
	NetTotal          money.Paise    `json:"net_total" jsonschema:"total before tax, paise"`
	GrandTotal        money.Paise    `json:"grand_total" jsonschema:"paise"`
	OutstandingAmount money.Paise    `json:"outstanding_amount" jsonschema:"unpaid amount, paise"`
	DebitTo           string         `json:"debit_to,omitempty" jsonschema:"receivable account"`
	CollectedVia      string         `json:"collected_via" jsonschema:"account the customer's payment went into (bank or Payment Gateway Clearing); several are joined with '; '; empty while unpaid"`
	Remarks           string         `json:"remarks,omitempty"`
	Lines             []SalesLineOut `json:"lines" jsonschema:"item rows"`
	Taxes             []TaxRowOut    `json:"taxes" jsonschema:"tax rows"`
}

// SalesLineOut is one Sales Invoice Item row.
type SalesLineOut struct {
	Name        string      `json:"name"`
	ItemCode    string      `json:"item_code,omitempty"`
	Description string      `json:"description,omitempty"`
	Account     string      `json:"account" jsonschema:"income account"`
	Amount      money.Paise `json:"amount" jsonschema:"paise"`
}

// PaymentsInput is the input of list_payments.
type PaymentsInput struct {
	Company  string `json:"company" jsonschema:"company ID, e.g. sharma"`
	FromDate string `json:"from_date" jsonschema:"first posting date, YYYY-MM-DD"`
	ToDate   string `json:"to_date" jsonschema:"last posting date, YYYY-MM-DD, inclusive"`
	Party    string `json:"party,omitempty" jsonschema:"optional exact party name (a Supplier or Customer ID)"`
}

// PaymentsOutput is the output of list_payments.
type PaymentsOutput struct {
	Payments []PaymentOut `json:"payments" jsonschema:"payment entries ordered by posting date, then name"`
}

// PaymentOut is one submitted Payment Entry.
type PaymentOut struct {
	Name              string          `json:"name"`
	PaymentType       string          `json:"payment_type" jsonschema:"Pay, Receive or Internal Transfer"`
	PartyType         string          `json:"party_type,omitempty"`
	Party             string          `json:"party,omitempty"`
	PartyName         string          `json:"party_name,omitempty"`
	Amount            money.Paise     `json:"amount" jsonschema:"received amount for Receive, paid amount otherwise; paise"`
	PaidAmount        money.Paise     `json:"paid_amount" jsonschema:"paise"`
	ReceivedAmount    money.Paise     `json:"received_amount" jsonschema:"paise"`
	PaidFrom          string          `json:"paid_from,omitempty" jsonschema:"account the money left"`
	PaidTo            string          `json:"paid_to,omitempty" jsonschema:"account the money went into"`
	ReferenceNo       string          `json:"reference_no,omitempty" jsonschema:"bank reference (UTR, cheque number)"`
	ReferenceDate     string          `json:"reference_date,omitempty" jsonschema:"YYYY-MM-DD"`
	PostingDate       string          `json:"posting_date" jsonschema:"YYYY-MM-DD"`
	Remarks           string          `json:"remarks,omitempty"`
	UnallocatedAmount money.Paise     `json:"unallocated_amount" jsonschema:"amount not allocated to any invoice (an advance), paise"`
	Invoices          []PaymentRefOut `json:"invoices" jsonschema:"invoices this payment settles"`
}

// PaymentRefOut is one Payment Entry Reference row.
type PaymentRefOut struct {
	Name              string      `json:"name"`
	ReferenceDoctype  string      `json:"reference_doctype" jsonschema:"e.g. Purchase Invoice or Sales Invoice"`
	ReferenceName     string      `json:"reference_name"`
	AllocatedAmount   money.Paise `json:"allocated_amount" jsonschema:"paise"`
	TotalAmount       money.Paise `json:"total_amount" jsonschema:"paise"`
	OutstandingAmount money.Paise `json:"outstanding_amount" jsonschema:"paise"`
}

// AccountHistoryInput is the input of get_account_history.
type AccountHistoryInput struct {
	Company      string `json:"company" jsonschema:"company ID, e.g. sharma"`
	Account      string `json:"account" jsonschema:"exact leaf account name, e.g. Bank Charges - STPL"`
	Months       int    `json:"months" jsonschema:"number of months, 1 to 12"`
	ThroughMonth string `json:"through_month,omitempty" jsonschema:"last month, YYYY-MM; default the current month"`
}

// AccountHistoryOutput is the output of get_account_history.
type AccountHistoryOutput struct {
	Account      string       `json:"account"`
	ThroughMonth string       `json:"through_month" jsonschema:"last month, YYYY-MM"`
	History      []MonthTotal `json:"history" jsonschema:"one row per month, oldest first; amounts in paise"`
}

// RecurringSuppliersInput is the input of list_recurring_suppliers.
type RecurringSuppliersInput struct {
	Company        string `json:"company" jsonschema:"company ID, e.g. sharma"`
	BeforeMonth    string `json:"before_month" jsonschema:"month being closed, YYYY-MM; only earlier months are read"`
	LookbackMonths int    `json:"lookback_months" jsonschema:"months before before_month to read, 1 to 12"`
	MinOccurrences int    `json:"min_occurrences" jsonschema:"fewest distinct months a supplier must have billed in, 1 to 12"`
	AmountBandPct  *int   `json:"amount_band_pct,omitempty" jsonschema:"largest allowed difference of any bill from the median, percent, 0 to 100; default 20"`
}

// RecurringSuppliersOutput is the output of list_recurring_suppliers.
type RecurringSuppliersOutput struct {
	Suppliers []RecurringSupplier `json:"suppliers" jsonschema:"recurring suppliers ordered by name"`
}

// ---- handlers ----

func (h *handlers) client() (*frappe.Client, error) {
	if h.deps.Client == nil {
		return nil, errors.New("books tools: no ERPNext client configured")
	}
	return h.deps.Client, nil
}

// company validates the company ID and returns its ERPNext name.
func (h *handlers) company(id string) (string, error) {
	if id == "" {
		return "", invalid("company", "is required (a company ID such as sharma)")
	}
	if err := checkText("company", id); err != nil {
		return "", err
	}
	if h.deps.Companies == nil {
		return "", errors.New("books tools: no company directory configured")
	}
	erp, err := h.deps.Companies.ERPCompany(id)
	if err != nil {
		if errors.Is(err, ErrUnknownCompany) {
			return "", invalid("company", fmt.Sprintf("unknown company ID %q; pass the run's company ID", id))
		}
		return "", fmt.Errorf("books tools: resolve company %q: %w", id, err)
	}
	return erp, nil
}

func (h *handlers) now() time.Time {
	if h.deps.Now != nil {
		return h.deps.Now()
	}
	return time.Now()
}

func (h *handlers) getTrialBalance(ctx context.Context, in TrialBalanceInput) (TrialBalanceOutput, error) {
	var out TrialBalanceOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	from, to, err := dateRange(in.FromDate, in.ToDate)
	if err != nil {
		return out, err
	}
	tb, err := TrialBalance(ctx, FrappeLedger{C: c}, erp, from, to)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolGetTrialBalance, err)
	}
	rows := tb.Rows
	if rows == nil {
		rows = []TBRow{}
	}
	return TrialBalanceOutput{
		Company:  in.Company,
		FromDate: in.FromDate,
		ToDate:   in.ToDate,
		Rows:     rows,
		Totals:   tb.Totals,
	}, nil
}

func (h *handlers) listGLEntries(ctx context.Context, in GLEntriesInput) (GLEntriesOutput, error) {
	var out GLEntriesOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	from, to, err := dateRange(in.FromDate, in.ToDate)
	if err != nil {
		return out, err
	}
	if err := checkOptionalText("account", in.Account); err != nil {
		return out, err
	}
	if err := checkOptionalText("party", in.Party); err != nil {
		return out, err
	}
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return out, err
	}
	page, next, err := glPage(ctx, c, erp, from, to, in.Account, in.Party, after)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListGLEntries, err)
	}
	out.Entries = make([]GLEntryOut, 0, len(page))
	for _, g := range page {
		out.Entries = append(out.Entries, glOut(g))
	}
	out.NextCursor = next
	return out, nil
}

// glPage reads one keyset page of GL Entries: up to GLPageSize entries
// with a name after after (all when after is empty), ordered by name. next
// is the cursor of the following page, empty when this one is the last.
// The account and party filters are filter values only.
func glPage(ctx context.Context, c *frappe.Client, erp string, from, to time.Time, account, party, after string) ([]frappe.GLEntry, string, error) {
	filters := [][]any{
		{"company", "=", erp},
		{"docstatus", "=", 1},
		{"is_cancelled", "=", 0},
		{"posting_date", "between", []string{from.Format(dateLayout), to.Format(dateLayout)}},
	}
	if account != "" {
		filters = append(filters, []any{"account", "=", account})
	}
	if party != "" {
		filters = append(filters, []any{"party", "=", party})
	}
	if after != "" {
		filters = append(filters, []any{"name", ">", after})
	}
	// One row more than a page tells whether another page follows.
	raw, err := frappe.List[frappe.GLEntryRaw](ctx, c, frappe.DocTypeGLEntry, frappe.Query{
		Fields:  frappe.Fields[frappe.GLEntryRaw](),
		Filters: filters,
		OrderBy: "name asc",
		Limit:   GLPageSize + 1,
	})
	if err != nil {
		return nil, "", fmt.Errorf("gl entries for %s: %w", erp, err)
	}
	next := ""
	if len(raw) > GLPageSize {
		raw = raw[:GLPageSize]
		next = encodeCursor(raw[len(raw)-1].Name)
	}
	out := make([]frappe.GLEntry, 0, len(raw))
	for _, r := range raw {
		g, err := r.Domain()
		if err != nil {
			return nil, "", fmt.Errorf("gl entries for %s: %w", erp, err)
		}
		if g.Company != erp || g.IsCancelled || g.Docstatus != 1 {
			return nil, "", fmt.Errorf("gl entries for %s: ERPNext returned GL Entry %s outside the filters", erp, g.Name)
		}
		out = append(out, g)
	}
	return out, next, nil
}

func (h *handlers) listPurchaseInvoices(ctx context.Context, in PurchaseInvoicesInput) (PurchaseInvoicesOutput, error) {
	var out PurchaseInvoicesOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	from, to, err := dateRange(in.FromDate, in.ToDate)
	if err != nil {
		return out, err
	}
	if err := checkOptionalText("supplier", in.Supplier); err != nil {
		return out, err
	}
	invs, err := purchaseInvoices(ctx, c, erp, from, to, in.Supplier, "to_date")
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListPurchaseInvoices, err)
	}
	out.Invoices = make([]PurchaseInvoiceOut, 0, len(invs))
	for _, p := range invs {
		out.Invoices = append(out.Invoices, purchaseOut(p))
	}
	return out, nil
}

func (h *handlers) listSalesInvoices(ctx context.Context, in SalesInvoicesInput) (SalesInvoicesOutput, error) {
	var out SalesInvoicesOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	from, to, err := dateRange(in.FromDate, in.ToDate)
	if err != nil {
		return out, err
	}
	invs, err := salesInvoices(ctx, c, erp, from, to)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListSalesInvoices, err)
	}
	names := make([]string, len(invs))
	for i, s := range invs {
		names[i] = s.Name
	}
	via, err := collectedVia(ctx, c, erp, names)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListSalesInvoices, err)
	}
	out.Invoices = make([]SalesInvoiceOut, 0, len(invs))
	for _, s := range invs {
		out.Invoices = append(out.Invoices, salesOut(s, via[s.Name]))
	}
	return out, nil
}

func (h *handlers) listPayments(ctx context.Context, in PaymentsInput) (PaymentsOutput, error) {
	var out PaymentsOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	from, to, err := dateRange(in.FromDate, in.ToDate)
	if err != nil {
		return out, err
	}
	if err := checkOptionalText("party", in.Party); err != nil {
		return out, err
	}
	pays, err := paymentEntries(ctx, c, erp, from, to, in.Party)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListPayments, err)
	}
	out.Payments = make([]PaymentOut, 0, len(pays))
	for _, p := range pays {
		out.Payments = append(out.Payments, paymentOut(p))
	}
	return out, nil
}

func (h *handlers) getAccountHistory(ctx context.Context, in AccountHistoryInput) (AccountHistoryOutput, error) {
	var out AccountHistoryOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	if in.Account == "" {
		return out, invalid("account", "is required (an exact leaf account name)")
	}
	if err := checkText("account", in.Account); err != nil {
		return out, err
	}
	if err := checkCount("months", in.Months); err != nil {
		return out, err
	}
	through := in.ThroughMonth
	if through == "" {
		through = h.now().UTC().Format(monthLayout)
	} else if _, err := parseMonth("through_month", through); err != nil {
		return out, err
	}
	// Check the account first so a wrong name is an input error the model
	// can fix; the cached ledger saves AccountHistory a second read.
	ledger, err := newCachedLedger(ctx, c, erp)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolGetAccountHistory, err)
	}
	if err := ledger.checkLeaf(in.Account); err != nil {
		return out, err
	}
	hist, err := AccountHistory(ctx, ledger, erp, in.Account, through, in.Months)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolGetAccountHistory, err)
	}
	return AccountHistoryOutput{Account: in.Account, ThroughMonth: through, History: hist}, nil
}

func (h *handlers) listRecurringSuppliers(ctx context.Context, in RecurringSuppliersInput) (RecurringSuppliersOutput, error) {
	var out RecurringSuppliersOutput
	c, err := h.client()
	if err != nil {
		return out, err
	}
	erp, err := h.company(in.Company)
	if err != nil {
		return out, err
	}
	before, err := parseMonth("before_month", in.BeforeMonth)
	if err != nil {
		return out, err
	}
	if err := checkCount("lookback_months", in.LookbackMonths); err != nil {
		return out, err
	}
	if err := checkCount("min_occurrences", in.MinOccurrences); err != nil {
		return out, err
	}
	band := DefaultAmountBandPct
	if in.AmountBandPct != nil {
		band = *in.AmountBandPct
		if band < 0 || band > 100 {
			return out, invalid("amount_band_pct", fmt.Sprintf("must be 0 to 100, got %d", band))
		}
	}
	from, to := RecurringWindow(before, in.LookbackMonths)
	invs, err := purchaseInvoices(ctx, c, erp, from, to, "", "lookback_months")
	if err != nil {
		return out, fmt.Errorf("%s: %w", ToolListRecurringSuppliers, err)
	}
	out.Suppliers = RecurringSuppliers(invs, in.MinOccurrences, band)
	if out.Suppliers == nil {
		out.Suppliers = []RecurringSupplier{}
	}
	return out, nil
}

// RecurringWindow is the date range list_recurring_suppliers reads: the
// lookback calendar months before the month starting at before (UTC
// midnight on the 1st), from the first day of the earliest to the last day
// of the month before before.
func RecurringWindow(before time.Time, lookback int) (from, to time.Time) {
	return before.AddDate(0, -lookback, 0), before.AddDate(0, 0, -1)
}

// ---- ERPNext reads ----

// submittedFilters are the list filters for submitted documents of erp
// posted from from to to, plus field = value when value is set. value is
// a filter value only; field is always a constant of this package.
func submittedFilters(erp string, from, to time.Time, field, value string) [][]any {
	f := [][]any{
		{"company", "=", erp},
		{"docstatus", "=", 1},
		{"posting_date", "between", []string{from.Format(dateLayout), to.Format(dateLayout)}},
	}
	if value != "" {
		f = append(f, []any{field, "=", value})
	}
	return f
}

// named is a list row holding only the document name.
type named struct {
	Name string `json:"name"`
}

// listNames lists the names of documents of doctype matching filters,
// ordered by posting date and name. More than MaxDocsPerCall matches is an
// input error on capField, so the caller fetches no document.
func listNames(ctx context.Context, c *frappe.Client, doctype string, filters [][]any, capField string) ([]string, error) {
	rows, err := frappe.List[named](ctx, c, doctype, frappe.Query{
		Fields:  []string{"name"},
		Filters: filters,
		OrderBy: "posting_date asc, name asc",
		Limit:   MaxDocsPerCall + 1,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxDocsPerCall {
		return nil, tooMany(capField)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names, nil
}

// getSubmitted fetches each named document in full (child tables
// included), converts it, and checks that it is submitted and belongs to
// erp.
func getSubmitted[R, D any](ctx context.Context, c *frappe.Client, doctype, erp string, names []string,
	conv func(R) (D, error), check func(D) (name, company string, docstatus int)) ([]D, error) {
	raws, err := frappe.GetMany[R](ctx, c, doctype, names)
	if err != nil {
		return nil, err
	}
	out := make([]D, 0, len(raws))
	for i, r := range raws {
		d, err := conv(r)
		if err != nil {
			return nil, fmt.Errorf("convert %s %s: %w", doctype, names[i], err)
		}
		name, company, docstatus := check(d)
		if company != erp || docstatus != 1 {
			return nil, fmt.Errorf("%s %s is not a submitted document of %s", doctype, name, erp)
		}
		out = append(out, d)
	}
	return out, nil
}

// tooMany is the input error for a call that matches more than
// MaxDocsPerCall documents.
func tooMany(field string) error {
	return invalid(field, fmt.Sprintf("matches more than %d documents; narrow the range", MaxDocsPerCall))
}

func purchaseInvoices(ctx context.Context, c *frappe.Client, erp string, from, to time.Time, supplier, capField string) ([]frappe.PurchaseInvoice, error) {
	names, err := listNames(ctx, c, frappe.DocTypePurchaseInvoice, submittedFilters(erp, from, to, "supplier", supplier), capField)
	if err != nil {
		return nil, fmt.Errorf("purchase invoices of %s: %w", erp, err)
	}
	out, err := getSubmitted(ctx, c, frappe.DocTypePurchaseInvoice, erp, names, frappe.PurchaseInvoiceRaw.Domain,
		func(p frappe.PurchaseInvoice) (string, string, int) { return p.Name, p.Company, p.Docstatus })
	if err != nil {
		return nil, fmt.Errorf("purchase invoices of %s: %w", erp, err)
	}
	return out, nil
}

func salesInvoices(ctx context.Context, c *frappe.Client, erp string, from, to time.Time) ([]frappe.SalesInvoice, error) {
	names, err := listNames(ctx, c, frappe.DocTypeSalesInvoice, submittedFilters(erp, from, to, "", ""), "to_date")
	if err != nil {
		return nil, fmt.Errorf("sales invoices of %s: %w", erp, err)
	}
	out, err := getSubmitted(ctx, c, frappe.DocTypeSalesInvoice, erp, names, frappe.SalesInvoiceRaw.Domain,
		func(s frappe.SalesInvoice) (string, string, int) { return s.Name, s.Company, s.Docstatus })
	if err != nil {
		return nil, fmt.Errorf("sales invoices of %s: %w", erp, err)
	}
	return out, nil
}

func paymentEntries(ctx context.Context, c *frappe.Client, erp string, from, to time.Time, party string) ([]frappe.PaymentEntry, error) {
	names, err := listNames(ctx, c, frappe.DocTypePaymentEntry, submittedFilters(erp, from, to, "party", party), "to_date")
	if err != nil {
		return nil, fmt.Errorf("payment entries of %s: %w", erp, err)
	}
	out, err := getSubmitted(ctx, c, frappe.DocTypePaymentEntry, erp, names, frappe.PaymentEntryRaw.Domain,
		func(p frappe.PaymentEntry) (string, string, int) { return p.Name, p.Company, p.Docstatus })
	if err != nil {
		return nil, fmt.Errorf("payment entries of %s: %w", erp, err)
	}
	return out, nil
}

// collectedVia maps each named sales invoice to the paid_to accounts of the
// submitted Receive Payment Entries that settle it, sorted and joined with
// "; ". It finds those payments by a filter on the Payment Entry Reference
// child table (reference_name in names, a filter value), then reads each
// payment in full and keeps only references to the named Sales Invoices,
// so the mapping never rests on the filter alone.
func collectedVia(ctx context.Context, c *frappe.Client, erp string, names []string) (map[string]string, error) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var payNames []string
	seen := map[string]bool{}
	for chunk := range slices.Chunk(names, nameChunk) {
		rows, err := frappe.List[named](ctx, c, frappe.DocTypePaymentEntry, frappe.Query{
			Fields: []string{"name"},
			Filters: [][]any{
				{"company", "=", erp},
				{"docstatus", "=", 1},
				{"payment_type", "=", "Receive"},
				{"Payment Entry Reference", "reference_name", "in", chunk},
			},
			OrderBy: "name asc",
			Limit:   MaxDocsPerCall + 1,
		})
		if err != nil {
			return nil, fmt.Errorf("receipts for sales invoices of %s: %w", erp, err)
		}
		for _, r := range rows {
			if !seen[r.Name] {
				seen[r.Name] = true
				payNames = append(payNames, r.Name)
			}
		}
		if len(payNames) > MaxDocsPerCall {
			return nil, tooMany("to_date")
		}
	}
	pays, err := getSubmitted(ctx, c, frappe.DocTypePaymentEntry, erp, payNames, frappe.PaymentEntryRaw.Domain,
		func(p frappe.PaymentEntry) (string, string, int) { return p.Name, p.Company, p.Docstatus })
	if err != nil {
		return nil, fmt.Errorf("receipts for sales invoices of %s: %w", erp, err)
	}
	accounts := map[string][]string{}
	for _, p := range pays {
		if p.PaymentType != "Receive" || p.PaidTo == "" {
			continue
		}
		for _, r := range p.References {
			if r.ReferenceDoctype != frappe.DocTypeSalesInvoice || !want[r.ReferenceName] {
				continue
			}
			if !slices.Contains(accounts[r.ReferenceName], p.PaidTo) {
				accounts[r.ReferenceName] = append(accounts[r.ReferenceName], p.PaidTo)
			}
		}
	}
	out := make(map[string]string, len(accounts))
	for inv, accs := range accounts {
		slices.Sort(accs)
		out[inv] = strings.Join(accs, "; ")
	}
	return out, nil
}

// ---- conversion to tool output ----

// fmtDate is d as YYYY-MM-DD, or "" for the zero time.
func fmtDate(d time.Time) string {
	if d.IsZero() {
		return ""
	}
	return d.Format(dateLayout)
}

func glOut(g frappe.GLEntry) GLEntryOut {
	return GLEntryOut{
		Name:        g.Name,
		Account:     g.Account,
		Debit:       g.Debit,
		Credit:      g.Credit,
		PostingDate: fmtDate(g.PostingDate),
		VoucherType: g.VoucherType,
		VoucherNo:   g.VoucherNo,
		PartyType:   g.PartyType,
		Party:       g.Party,
		Against:     clipText(g.Against),
		Remarks:     clipText(g.Remarks),
		IsOpening:   g.IsOpening,
	}
}

// gstKind is the GST head of a tax row: its gst_tax_type when that is
// igst, cgst or sgst (India Compliance sets it from GST Settings), or,
// when gst_tax_type is empty, the IGST, CGST or SGST word in the account
// head. Anything else (cess, RCM, TDS, freight) is "".
func gstKind(gstTaxType, accountHead string) string {
	switch t := strings.ToLower(gstTaxType); t {
	case "igst", "cgst", "sgst":
		return t
	case "":
		for _, k := range []string{"IGST", "CGST", "SGST"} {
			if strings.Contains(accountHead, k) {
				return strings.ToLower(k)
			}
		}
	}
	return ""
}

// gstSplit sums a purchase invoice's IGST, CGST and SGST rows; a Deduct
// row counts negative.
func gstSplit(taxes []frappe.PurchaseTaxesAndCharges) (igst, cgst, sgst money.Paise) {
	for _, t := range taxes {
		amt := t.TaxAmount
		if t.AddDeductTax == "Deduct" {
			amt = -amt
		}
		switch gstKind(t.GSTTaxType, t.AccountHead) {
		case "igst":
			igst += amt
		case "cgst":
			cgst += amt
		case "sgst":
			sgst += amt
		}
	}
	return igst, cgst, sgst
}

func purchaseOut(p frappe.PurchaseInvoice) PurchaseInvoiceOut {
	igst, cgst, sgst := gstSplit(p.Taxes)
	lines := make([]PurchaseLineOut, 0, len(p.Items))
	for _, it := range p.Items {
		lines = append(lines, PurchaseLineOut{
			Name:        it.Name,
			ItemCode:    it.ItemCode,
			Description: clipText(it.Description),
			Account:     it.ExpenseAccount,
			Amount:      it.Amount,
		})
	}
	taxes := make([]TaxRowOut, 0, len(p.Taxes))
	for _, t := range p.Taxes {
		taxes = append(taxes, TaxRowOut{
			Name:         t.Name,
			AccountHead:  t.AccountHead,
			ChargeType:   t.ChargeType,
			Rate:         t.Rate.String(),
			TaxAmount:    t.TaxAmount,
			AddDeductTax: t.AddDeductTax,
			Category:     t.Category,
			Description:  clipText(t.Description),
			GSTTaxType:   t.GSTTaxType,
		})
	}
	return PurchaseInvoiceOut{
		Name:              p.Name,
		Supplier:          p.Supplier,
		SupplierName:      clipText(p.SupplierName),
		SupplierGSTIN:     p.SupplierGSTIN,
		CompanyGSTIN:      p.CompanyGSTIN,
		PlaceOfSupply:     p.PlaceOfSupply,
		BillNo:            clipText(p.BillNo),
		BillDate:          fmtDate(p.BillDate),
		PostingDate:       fmtDate(p.PostingDate),
		CreditTo:          p.CreditTo,
		IsReturn:          p.IsReturn,
		Taxable:           p.NetTotal,
		IGST:              igst,
		CGST:              cgst,
		SGST:              sgst,
		GrandTotal:        p.GrandTotal,
		OutstandingAmount: p.OutstandingAmount,
		Remarks:           clipText(p.Remarks),
		Lines:             lines,
		Taxes:             taxes,
	}
}

func salesOut(s frappe.SalesInvoice, via string) SalesInvoiceOut {
	lines := make([]SalesLineOut, 0, len(s.Items))
	for _, it := range s.Items {
		lines = append(lines, SalesLineOut{
			Name:        it.Name,
			ItemCode:    it.ItemCode,
			Description: clipText(it.Description),
			Account:     it.IncomeAccount,
			Amount:      it.Amount,
		})
	}
	taxes := make([]TaxRowOut, 0, len(s.Taxes))
	for _, t := range s.Taxes {
		taxes = append(taxes, TaxRowOut{
			Name:        t.Name,
			AccountHead: t.AccountHead,
			ChargeType:  t.ChargeType,
			Rate:        t.Rate.String(),
			TaxAmount:   t.TaxAmount,
			Description: clipText(t.Description),
			GSTTaxType:  t.GSTTaxType,
		})
	}
	return SalesInvoiceOut{
		Name:              s.Name,
		Customer:          s.Customer,
		CustomerName:      clipText(s.CustomerName),
		PostingDate:       fmtDate(s.PostingDate),
		NetTotal:          s.NetTotal,
		GrandTotal:        s.GrandTotal,
		OutstandingAmount: s.OutstandingAmount,
		DebitTo:           s.DebitTo,
		CollectedVia:      via,
		Remarks:           clipText(s.Remarks),
		Lines:             lines,
		Taxes:             taxes,
	}
}

func paymentOut(p frappe.PaymentEntry) PaymentOut {
	refs := make([]PaymentRefOut, 0, len(p.References))
	for _, r := range p.References {
		refs = append(refs, PaymentRefOut{
			Name:              r.Name,
			ReferenceDoctype:  r.ReferenceDoctype,
			ReferenceName:     r.ReferenceName,
			AllocatedAmount:   r.AllocatedAmount,
			TotalAmount:       r.TotalAmount,
			OutstandingAmount: r.OutstandingAmount,
		})
	}
	amount := p.PaidAmount
	if p.PaymentType == "Receive" {
		amount = p.ReceivedAmount
	}
	return PaymentOut{
		Name:              p.Name,
		PaymentType:       p.PaymentType,
		PartyType:         p.PartyType,
		Party:             p.Party,
		PartyName:         clipText(p.PartyName),
		Amount:            amount,
		PaidAmount:        p.PaidAmount,
		ReceivedAmount:    p.ReceivedAmount,
		PaidFrom:          p.PaidFrom,
		PaidTo:            p.PaidTo,
		ReferenceNo:       clipText(p.ReferenceNo),
		ReferenceDate:     fmtDate(p.ReferenceDate),
		PostingDate:       fmtDate(p.PostingDate),
		Remarks:           clipText(p.Remarks),
		UnallocatedAmount: p.UnallocatedAmount,
		Invoices:          refs,
	}
}

// cachedLedger is a FrappeLedger that reads the company's accounts once,
// so a tool can check an account before running a report on it.
type cachedLedger struct {
	FrappeLedger
	accounts []frappe.Account
}

var _ Ledger = (*cachedLedger)(nil)

func newCachedLedger(ctx context.Context, c *frappe.Client, erp string) (*cachedLedger, error) {
	l := FrappeLedger{C: c}
	accs, err := l.Accounts(ctx, erp)
	if err != nil {
		return nil, err
	}
	return &cachedLedger{FrappeLedger: l, accounts: accs}, nil
}

// Accounts returns the accounts read when the ledger was made.
func (l *cachedLedger) Accounts(context.Context, string) ([]frappe.Account, error) {
	return l.accounts, nil
}

// checkLeaf requires account to be a leaf account of the company.
func (l *cachedLedger) checkLeaf(account string) error {
	for _, a := range l.accounts {
		if a.Name != account {
			continue
		}
		if a.IsGroup {
			return invalid("account", fmt.Sprintf("%q is a group account; pass a leaf account (GL Entries post only to leaf accounts)", account))
		}
		return nil
	}
	return invalid("account", fmt.Sprintf("no account %q in the company; pass the exact account name, e.g. from get_trial_balance", account))
}
