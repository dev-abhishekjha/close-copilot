package frappe

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/ledger"
)

// DocType models (CC-203).
//
// Each DocType has two structs. The XRaw struct mirrors ERPNext's JSON: the
// json tags are the field names in docs/erpnext-schema, amounts are
// json.Number (the client decodes with UseNumber), dates are "YYYY-MM-DD"
// strings and Check fields are 0 or 1. The X struct is the domain form:
// amounts in money.Paise, dates as time.Time at UTC midnight, Checks as
// bool. XRaw.Domain (convert.go) turns one into the other.
//
// The domain structs live in internal/ledger (CC-601a), which has no
// dependency on this client; X here is a type alias for ledger.X.
//
// Fetch raw structs with List or Get, asking for Fields[XRaw]() so every
// field the converter needs is present. List never returns child tables;
// Get does.

// DocType names, as List and Get take them.
const (
	DocTypeAccount         = "Account"
	DocTypeGLEntry         = "GL Entry"
	DocTypeSupplier        = "Supplier"
	DocTypeCustomer        = "Customer"
	DocTypePurchaseInvoice = "Purchase Invoice"
	DocTypeSalesInvoice    = "Sales Invoice"
	DocTypePaymentEntry    = "Payment Entry"
	DocTypeJournalEntry    = "Journal Entry"
)

// Account root types (Account.root_type), as defined in internal/ledger.
const (
	RootTypeAsset     = ledger.RootTypeAsset
	RootTypeLiability = ledger.RootTypeLiability
	RootTypeEquity    = ledger.RootTypeEquity
	RootTypeIncome    = ledger.RootTypeIncome
	RootTypeExpense   = ledger.RootTypeExpense
)

// Fields returns the JSON field names of T's scalar fields, in declaration
// order, for Query.Fields. Child tables (slice fields) are left out because
// List can't return them.
func Fields[T any]() []string {
	t := reflect.TypeFor[T]()
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Slice {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ---- Account ----

type AccountRaw struct {
	Name            string `json:"name"`
	Docstatus       int    `json:"docstatus"`
	AccountName     string `json:"account_name"`
	Company         string `json:"company"`
	ParentAccount   string `json:"parent_account"`
	IsGroup         int    `json:"is_group"`
	RootType        string `json:"root_type"`
	AccountType     string `json:"account_type"`
	AccountCurrency string `json:"account_currency"`
}

// Account is ledger.Account, the domain form of AccountRaw.
type Account = ledger.Account

// ---- GL Entry ----

type GLEntryRaw struct {
	Name        string      `json:"name"`
	Docstatus   int         `json:"docstatus"`
	Company     string      `json:"company"`
	Account     string      `json:"account"`
	Debit       json.Number `json:"debit"`
	Credit      json.Number `json:"credit"`
	PostingDate string      `json:"posting_date"`
	VoucherType string      `json:"voucher_type"`
	VoucherNo   string      `json:"voucher_no"`
	PartyType   string      `json:"party_type"`
	Party       string      `json:"party"`
	IsCancelled int         `json:"is_cancelled"`
	IsOpening   string      `json:"is_opening"` // "No" or "Yes"
	FiscalYear  string      `json:"fiscal_year"`
	Against     string      `json:"against"`
	Remarks     string      `json:"remarks"`
}

// GLEntry is ledger.GLEntry, the domain form of GLEntryRaw.
type GLEntry = ledger.GLEntry

// ---- Supplier ----

type SupplierRaw struct {
	Name          string `json:"name"`
	Docstatus     int    `json:"docstatus"`
	SupplierName  string `json:"supplier_name"`
	SupplierGroup string `json:"supplier_group"`
	SupplierType  string `json:"supplier_type"`
	GSTIN         string `json:"gstin"`
	GSTCategory   string `json:"gst_category"`
	PAN           string `json:"pan"`
}

// Supplier is ledger.Supplier, the domain form of SupplierRaw.
type Supplier = ledger.Supplier

// ---- Customer ----

type CustomerRaw struct {
	Name          string `json:"name"`
	Docstatus     int    `json:"docstatus"`
	CustomerName  string `json:"customer_name"`
	CustomerGroup string `json:"customer_group"`
	CustomerType  string `json:"customer_type"`
	Territory     string `json:"territory"`
	GSTIN         string `json:"gstin"`
	GSTCategory   string `json:"gst_category"`
}

// Customer is ledger.Customer, the domain form of CustomerRaw.
type Customer = ledger.Customer

// ---- Purchase Invoice ----

type PurchaseInvoiceRaw struct {
	Name              string                       `json:"name"`
	Docstatus         int                          `json:"docstatus"`
	Company           string                       `json:"company"`
	Supplier          string                       `json:"supplier"`
	SupplierName      string                       `json:"supplier_name"`
	BillNo            string                       `json:"bill_no"`
	BillDate          string                       `json:"bill_date"`
	PostingDate       string                       `json:"posting_date"`
	Remarks           string                       `json:"remarks"`
	CreditTo          string                       `json:"credit_to"`
	NetTotal          json.Number                  `json:"net_total"`
	GrandTotal        json.Number                  `json:"grand_total"`
	OutstandingAmount json.Number                  `json:"outstanding_amount"`
	IsReturn          int                          `json:"is_return"`
	SupplierGSTIN     string                       `json:"supplier_gstin"`
	CompanyGSTIN      string                       `json:"company_gstin"`
	PlaceOfSupply     string                       `json:"place_of_supply"`
	Items             []PurchaseInvoiceItemRaw     `json:"items"`
	Taxes             []PurchaseTaxesAndChargesRaw `json:"taxes"`
}

// PurchaseInvoice is ledger.PurchaseInvoice, the domain form of PurchaseInvoiceRaw.
type PurchaseInvoice = ledger.PurchaseInvoice

// PurchaseInvoiceItemRaw is a Purchase Invoice Item row. That DocType is
// not in docs/erpnext-schema; these fields were checked live (CC-201).
type PurchaseInvoiceItemRaw struct {
	Name           string      `json:"name"`
	ItemCode       string      `json:"item_code"`
	Description    string      `json:"description"`
	ExpenseAccount string      `json:"expense_account"`
	Amount         json.Number `json:"amount"`
}

// PurchaseInvoiceItem is ledger.PurchaseInvoiceItem, the domain form of PurchaseInvoiceItemRaw.
type PurchaseInvoiceItem = ledger.PurchaseInvoiceItem

type PurchaseTaxesAndChargesRaw struct {
	Name         string      `json:"name"`
	AccountHead  string      `json:"account_head"`
	TaxAmount    json.Number `json:"tax_amount"`
	ChargeType   string      `json:"charge_type"`
	Rate         json.Number `json:"rate"`
	AddDeductTax string      `json:"add_deduct_tax"`
	Category     string      `json:"category"`
	Description  string      `json:"description"`
	GSTTaxType   string      `json:"gst_tax_type"`
}

// PurchaseTaxesAndCharges is ledger.PurchaseTaxesAndCharges, the domain form of PurchaseTaxesAndChargesRaw.
type PurchaseTaxesAndCharges = ledger.PurchaseTaxesAndCharges

// ---- Sales Invoice ----

type SalesInvoiceRaw struct {
	Name              string                    `json:"name"`
	Docstatus         int                       `json:"docstatus"`
	Company           string                    `json:"company"`
	Customer          string                    `json:"customer"`
	CustomerName      string                    `json:"customer_name"`
	PostingDate       string                    `json:"posting_date"`
	NetTotal          json.Number               `json:"net_total"`
	GrandTotal        json.Number               `json:"grand_total"`
	OutstandingAmount json.Number               `json:"outstanding_amount"`
	DebitTo           string                    `json:"debit_to"`
	Remarks           string                    `json:"remarks"`
	Items             []SalesInvoiceItemRaw     `json:"items"`
	Taxes             []SalesTaxesAndChargesRaw `json:"taxes"`
}

// SalesInvoice is ledger.SalesInvoice, the domain form of SalesInvoiceRaw.
type SalesInvoice = ledger.SalesInvoice

// SalesInvoiceItemRaw is a Sales Invoice Item row. That DocType is not in
// docs/erpnext-schema; the fields are ERPNext's standard ones, the sales
// counterpart of PurchaseInvoiceItemRaw.
type SalesInvoiceItemRaw struct {
	Name          string      `json:"name"`
	ItemCode      string      `json:"item_code"`
	Description   string      `json:"description"`
	IncomeAccount string      `json:"income_account"`
	Amount        json.Number `json:"amount"`
}

// SalesInvoiceItem is ledger.SalesInvoiceItem, the domain form of SalesInvoiceItemRaw.
type SalesInvoiceItem = ledger.SalesInvoiceItem

// SalesTaxesAndChargesRaw is a Sales Taxes and Charges row. That DocType is
// not in docs/erpnext-schema; it has the fields of Purchase Taxes and
// Charges except add_deduct_tax and category.
type SalesTaxesAndChargesRaw struct {
	Name        string      `json:"name"`
	AccountHead string      `json:"account_head"`
	TaxAmount   json.Number `json:"tax_amount"`
	ChargeType  string      `json:"charge_type"`
	Rate        json.Number `json:"rate"`
	Description string      `json:"description"`
	GSTTaxType  string      `json:"gst_tax_type"`
}

// SalesTaxesAndCharges is ledger.SalesTaxesAndCharges, the domain form of SalesTaxesAndChargesRaw.
type SalesTaxesAndCharges = ledger.SalesTaxesAndCharges

// ---- Payment Entry ----

type PaymentEntryRaw struct {
	Name              string                     `json:"name"`
	Docstatus         int                        `json:"docstatus"`
	Company           string                     `json:"company"`
	PaymentType       string                     `json:"payment_type"`
	PartyType         string                     `json:"party_type"`
	Party             string                     `json:"party"`
	PartyName         string                     `json:"party_name"`
	PaidAmount        json.Number                `json:"paid_amount"`
	ReceivedAmount    json.Number                `json:"received_amount"`
	PaidFrom          string                     `json:"paid_from"`
	PaidTo            string                     `json:"paid_to"`
	ReferenceNo       string                     `json:"reference_no"`
	ReferenceDate     string                     `json:"reference_date"`
	PostingDate       string                     `json:"posting_date"`
	Remarks           string                     `json:"remarks"`
	UnallocatedAmount json.Number                `json:"unallocated_amount"`
	References        []PaymentEntryReferenceRaw `json:"references"`
}

// PaymentEntry is ledger.PaymentEntry, the domain form of PaymentEntryRaw.
type PaymentEntry = ledger.PaymentEntry

type PaymentEntryReferenceRaw struct {
	Name              string      `json:"name"`
	ReferenceDoctype  string      `json:"reference_doctype"`
	ReferenceName     string      `json:"reference_name"`
	AllocatedAmount   json.Number `json:"allocated_amount"`
	TotalAmount       json.Number `json:"total_amount"`
	OutstandingAmount json.Number `json:"outstanding_amount"`
}

// PaymentEntryReference is ledger.PaymentEntryReference, the domain form of PaymentEntryReferenceRaw.
type PaymentEntryReference = ledger.PaymentEntryReference

// ---- Journal Entry ----

type JournalEntryRaw struct {
	Name        string                   `json:"name"`
	Docstatus   int                      `json:"docstatus"`
	Company     string                   `json:"company"`
	VoucherType string                   `json:"voucher_type"`
	PostingDate string                   `json:"posting_date"`
	UserRemark  string                   `json:"user_remark"`
	ChequeNo    string                   `json:"cheque_no"`
	ChequeDate  string                   `json:"cheque_date"`
	TotalDebit  json.Number              `json:"total_debit"`
	TotalCredit json.Number              `json:"total_credit"`
	Accounts    []JournalEntryAccountRaw `json:"accounts"`
}

// JournalEntry is ledger.JournalEntry, the domain form of JournalEntryRaw.
type JournalEntry = ledger.JournalEntry

type JournalEntryAccountRaw struct {
	Name                    string      `json:"name"`
	Account                 string      `json:"account"`
	DebitInAccountCurrency  json.Number `json:"debit_in_account_currency"`
	CreditInAccountCurrency json.Number `json:"credit_in_account_currency"`
	Debit                   json.Number `json:"debit"`
	Credit                  json.Number `json:"credit"`
	PartyType               string      `json:"party_type"`
	Party                   string      `json:"party"`
	ReferenceType           string      `json:"reference_type"`
	ReferenceName           string      `json:"reference_name"`
	CostCenter              string      `json:"cost_center"`
	UserRemark              string      `json:"user_remark"`
}

// JournalEntryAccount is ledger.JournalEntryAccount, the domain form of JournalEntryAccountRaw.
type JournalEntryAccount = ledger.JournalEntryAccount
