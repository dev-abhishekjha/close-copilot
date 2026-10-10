package ledger

import (
	"encoding/json"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Domain forms of the ERPNext DocTypes (CC-203, moved here in CC-601a).
//
// Amounts are money.Paise, dates are time.Time at UTC midnight (the zero
// time when an optional date is unset) and Check fields are bool. The raw
// JSON structs and their Domain conversions stay in internal/frappe.

// Account root types (Account.root_type).
const (
	RootTypeAsset     = "Asset"
	RootTypeLiability = "Liability"
	RootTypeEquity    = "Equity"
	RootTypeIncome    = "Income"
	RootTypeExpense   = "Expense"
)

// Account is an ERPNext Account (chart of accounts node).
type Account struct {
	Name            string
	Docstatus       int
	AccountName     string
	Company         string
	ParentAccount   string // empty for a root account
	IsGroup         bool
	RootType        string // one of the RootType constants
	AccountType     string
	AccountCurrency string
}

// GLEntry is one ERPNext GL Entry: a single debit or credit line.
type GLEntry struct {
	Name        string
	Docstatus   int
	Company     string
	Account     string
	Debit       money.Paise
	Credit      money.Paise
	PostingDate time.Time
	VoucherType string
	VoucherNo   string
	PartyType   string
	Party       string
	IsCancelled bool
	IsOpening   bool
	FiscalYear  string
	Against     string
	Remarks     string
}

// Supplier is an ERPNext Supplier.
type Supplier struct {
	Name          string
	Docstatus     int
	SupplierName  string
	SupplierGroup string
	SupplierType  string
	GSTIN         string
	GSTCategory   string
	PAN           string
}

// Customer is an ERPNext Customer.
type Customer struct {
	Name          string
	Docstatus     int
	CustomerName  string
	CustomerGroup string
	CustomerType  string
	Territory     string
	GSTIN         string
	GSTCategory   string
}

// PurchaseInvoice is an ERPNext Purchase Invoice with its child tables.
type PurchaseInvoice struct {
	Name              string
	Docstatus         int
	Company           string
	Supplier          string
	SupplierName      string
	BillNo            string
	BillDate          time.Time // zero when not set
	PostingDate       time.Time
	Remarks           string
	CreditTo          string
	NetTotal          money.Paise
	GrandTotal        money.Paise
	OutstandingAmount money.Paise
	IsReturn          bool
	SupplierGSTIN     string
	CompanyGSTIN      string
	PlaceOfSupply     string
	Items             []PurchaseInvoiceItem
	Taxes             []PurchaseTaxesAndCharges
}

// PurchaseInvoiceItem is a Purchase Invoice Item row.
type PurchaseInvoiceItem struct {
	Name           string
	ItemCode       string
	Description    string
	ExpenseAccount string
	Amount         money.Paise
}

// PurchaseTaxesAndCharges is a Purchase Taxes and Charges row.
type PurchaseTaxesAndCharges struct {
	Name        string
	AccountHead string
	TaxAmount   money.Paise
	ChargeType  string
	// Rate is the percentage as ERPNext sends it (a Float field), such as
	// "9" or "2.5". It is not money, so it stays as exact decimal text.
	Rate         json.Number
	AddDeductTax string
	Category     string
	Description  string
	GSTTaxType   string
}

// SalesInvoice is an ERPNext Sales Invoice with its child tables.
type SalesInvoice struct {
	Name              string
	Docstatus         int
	Company           string
	Customer          string
	CustomerName      string
	PostingDate       time.Time
	NetTotal          money.Paise
	GrandTotal        money.Paise
	OutstandingAmount money.Paise
	DebitTo           string
	Remarks           string
	Items             []SalesInvoiceItem
	Taxes             []SalesTaxesAndCharges
}

// SalesInvoiceItem is a Sales Invoice Item row.
type SalesInvoiceItem struct {
	Name          string
	ItemCode      string
	Description   string
	IncomeAccount string
	Amount        money.Paise
}

// SalesTaxesAndCharges is a Sales Taxes and Charges row.
type SalesTaxesAndCharges struct {
	Name        string
	AccountHead string
	TaxAmount   money.Paise
	ChargeType  string
	Rate        json.Number // see PurchaseTaxesAndCharges.Rate
	Description string
	GSTTaxType  string
}

// PaymentEntry is an ERPNext Payment Entry with its reference rows.
type PaymentEntry struct {
	Name              string
	Docstatus         int
	Company           string
	PaymentType       string
	PartyType         string
	Party             string
	PartyName         string
	PaidAmount        money.Paise
	ReceivedAmount    money.Paise
	PaidFrom          string
	PaidTo            string
	ReferenceNo       string
	ReferenceDate     time.Time // zero when not set
	PostingDate       time.Time
	Remarks           string
	UnallocatedAmount money.Paise
	References        []PaymentEntryReference
}

// PaymentEntryReference is a Payment Entry Reference row.
type PaymentEntryReference struct {
	Name              string
	ReferenceDoctype  string
	ReferenceName     string
	AllocatedAmount   money.Paise
	TotalAmount       money.Paise
	OutstandingAmount money.Paise
}

// JournalEntry is an ERPNext Journal Entry with its account rows.
type JournalEntry struct {
	Name        string
	Docstatus   int
	Company     string
	VoucherType string
	PostingDate time.Time
	UserRemark  string
	ChequeNo    string
	ChequeDate  time.Time // zero when not set
	TotalDebit  money.Paise
	TotalCredit money.Paise
	Accounts    []JournalEntryAccount
}

// JournalEntryAccount is a Journal Entry Account row.
type JournalEntryAccount struct {
	Name                    string
	Account                 string
	DebitInAccountCurrency  money.Paise
	CreditInAccountCurrency money.Paise
	Debit                   money.Paise // company currency, computed by ERPNext on save
	Credit                  money.Paise // company currency, computed by ERPNext on save
	PartyType               string
	Party                   string
	ReferenceType           string
	ReferenceName           string
	CostCenter              string
	UserRemark              string
}
