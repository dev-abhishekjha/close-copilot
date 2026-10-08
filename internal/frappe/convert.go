package frappe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Converters from the raw ERPNext structs to the domain structs (CC-203).
//
// Every converter is a Domain method on the raw struct. Errors name the
// DocType, the document and the field, for example
//
//	GL Entry ACC-GLE-2026-00042: debit: money: json number "abc": invalid amount
//
// and child-table fields carry their row index, as in items[2].amount.
//
// Amounts must be present: Frappe stores Currency fields as NOT NULL
// DEFAULT 0, so an empty json.Number means the field was not fetched.
// posting_date is required on every DocType that has it; bill_date,
// reference_date and cheque_date are optional in ERPNext and stay the zero
// time.Time when empty.

// dateLayout is the YYYY-MM-DD form ERPNext uses for Date fields.
const dateLayout = time.DateOnly

var errEmpty = errors.New("empty")

// conv converts the fields of one document and keeps the first error,
// prefixed with the DocType, document name and field name.
type conv struct {
	doctype string
	name    string
	err     error
}

func newConv(doctype, name string) *conv {
	return &conv{doctype: doctype, name: name}
}

func (c *conv) fail(field string, err error) {
	if c.err != nil {
		return
	}
	name := c.name
	if name == "" {
		name = "(unnamed)"
	}
	c.err = fmt.Errorf("%s %s: %s: %w", c.doctype, name, field, err)
}

func (c *conv) amount(field string, n json.Number) money.Paise {
	if n == "" {
		c.fail(field, errEmpty)
		return 0
	}
	p, err := money.FromJSONNumber(n)
	if err != nil {
		c.fail(field, err)
		return 0
	}
	return p
}

// date parses a required YYYY-MM-DD date to UTC midnight.
func (c *conv) date(field, s string) time.Time {
	if s == "" {
		c.fail(field, errEmpty)
		return time.Time{}
	}
	return c.optDate(field, s)
}

// optDate parses an optional YYYY-MM-DD date; empty gives the zero time.
func (c *conv) optDate(field, s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		c.fail(field, fmt.Errorf("date %q: %w", s, err))
		return time.Time{}
	}
	return t // time.Parse without a zone gives UTC
}

// check converts a Check field, 0 or 1.
func (c *conv) check(field string, v int) bool {
	switch v {
	case 0:
		return false
	case 1:
		return true
	}
	c.fail(field, fmt.Errorf("check value %d is not 0 or 1", v))
	return false
}

func rowField(table string, i int, field string) string {
	return table + "[" + strconv.Itoa(i) + "]." + field
}

// Domain converts an Account. root_type must be one of Asset, Liability,
// Equity, Income and Expense.
func (r AccountRaw) Domain() (Account, error) {
	c := newConv(DocTypeAccount, r.Name)
	a := Account{
		Name:            r.Name,
		Docstatus:       r.Docstatus,
		AccountName:     r.AccountName,
		Company:         r.Company,
		ParentAccount:   r.ParentAccount,
		IsGroup:         c.check("is_group", r.IsGroup),
		RootType:        r.RootType,
		AccountType:     r.AccountType,
		AccountCurrency: r.AccountCurrency,
	}
	switch r.RootType {
	case RootTypeAsset, RootTypeLiability, RootTypeEquity, RootTypeIncome, RootTypeExpense:
	default:
		c.fail("root_type", fmt.Errorf("%q is not Asset, Liability, Equity, Income or Expense", r.RootType))
	}
	if c.err != nil {
		return Account{}, c.err
	}
	return a, nil
}

// Domain converts a GL Entry. is_opening must be "Yes" or "No".
func (r GLEntryRaw) Domain() (GLEntry, error) {
	c := newConv(DocTypeGLEntry, r.Name)
	g := GLEntry{
		Name:        r.Name,
		Docstatus:   r.Docstatus,
		Company:     r.Company,
		Account:     r.Account,
		Debit:       c.amount("debit", r.Debit),
		Credit:      c.amount("credit", r.Credit),
		PostingDate: c.date("posting_date", r.PostingDate),
		VoucherType: r.VoucherType,
		VoucherNo:   r.VoucherNo,
		PartyType:   r.PartyType,
		Party:       r.Party,
		IsCancelled: c.check("is_cancelled", r.IsCancelled),
		FiscalYear:  r.FiscalYear,
		Against:     r.Against,
		Remarks:     r.Remarks,
	}
	switch r.IsOpening {
	case "Yes":
		g.IsOpening = true
	case "No":
	default:
		c.fail("is_opening", fmt.Errorf("%q is not Yes or No", r.IsOpening))
	}
	if c.err != nil {
		return GLEntry{}, c.err
	}
	return g, nil
}

// Domain converts a Supplier. Supplier has only text fields, so the two
// structs have the same fields and this is a plain conversion (the compiler
// rejects it if they drift apart). It cannot fail; it returns an error for
// symmetry with the other converters.
func (r SupplierRaw) Domain() (Supplier, error) {
	return Supplier(r), nil
}

// Domain converts a Customer, as a plain conversion like SupplierRaw.Domain.
// It cannot fail; it returns an error for symmetry with the other
// converters.
func (r CustomerRaw) Domain() (Customer, error) {
	return Customer(r), nil
}

// Domain converts a Purchase Invoice with its items and taxes.
func (r PurchaseInvoiceRaw) Domain() (PurchaseInvoice, error) {
	c := newConv(DocTypePurchaseInvoice, r.Name)
	p := PurchaseInvoice{
		Name:              r.Name,
		Docstatus:         r.Docstatus,
		Company:           r.Company,
		Supplier:          r.Supplier,
		SupplierName:      r.SupplierName,
		BillNo:            r.BillNo,
		BillDate:          c.optDate("bill_date", r.BillDate),
		PostingDate:       c.date("posting_date", r.PostingDate),
		Remarks:           r.Remarks,
		CreditTo:          r.CreditTo,
		NetTotal:          c.amount("net_total", r.NetTotal),
		GrandTotal:        c.amount("grand_total", r.GrandTotal),
		OutstandingAmount: c.amount("outstanding_amount", r.OutstandingAmount),
		IsReturn:          c.check("is_return", r.IsReturn),
		SupplierGSTIN:     r.SupplierGSTIN,
		CompanyGSTIN:      r.CompanyGSTIN,
		PlaceOfSupply:     r.PlaceOfSupply,
	}
	if r.Items != nil {
		p.Items = make([]PurchaseInvoiceItem, len(r.Items))
		for i, it := range r.Items {
			p.Items[i] = PurchaseInvoiceItem{
				Name:           it.Name,
				ItemCode:       it.ItemCode,
				Description:    it.Description,
				ExpenseAccount: it.ExpenseAccount,
				Amount:         c.amount(rowField("items", i, "amount"), it.Amount),
			}
		}
	}
	if r.Taxes != nil {
		p.Taxes = make([]PurchaseTaxesAndCharges, len(r.Taxes))
		for i, tx := range r.Taxes {
			p.Taxes[i] = PurchaseTaxesAndCharges{
				Name:         tx.Name,
				AccountHead:  tx.AccountHead,
				TaxAmount:    c.amount(rowField("taxes", i, "tax_amount"), tx.TaxAmount),
				ChargeType:   tx.ChargeType,
				Rate:         tx.Rate,
				AddDeductTax: tx.AddDeductTax,
				Category:     tx.Category,
				Description:  tx.Description,
				GSTTaxType:   tx.GSTTaxType,
			}
		}
	}
	if c.err != nil {
		return PurchaseInvoice{}, c.err
	}
	return p, nil
}

// Domain converts a Sales Invoice with its items and taxes.
func (r SalesInvoiceRaw) Domain() (SalesInvoice, error) {
	c := newConv(DocTypeSalesInvoice, r.Name)
	s := SalesInvoice{
		Name:              r.Name,
		Docstatus:         r.Docstatus,
		Company:           r.Company,
		Customer:          r.Customer,
		CustomerName:      r.CustomerName,
		PostingDate:       c.date("posting_date", r.PostingDate),
		NetTotal:          c.amount("net_total", r.NetTotal),
		GrandTotal:        c.amount("grand_total", r.GrandTotal),
		OutstandingAmount: c.amount("outstanding_amount", r.OutstandingAmount),
		DebitTo:           r.DebitTo,
		Remarks:           r.Remarks,
	}
	if r.Items != nil {
		s.Items = make([]SalesInvoiceItem, len(r.Items))
		for i, it := range r.Items {
			s.Items[i] = SalesInvoiceItem{
				Name:          it.Name,
				ItemCode:      it.ItemCode,
				Description:   it.Description,
				IncomeAccount: it.IncomeAccount,
				Amount:        c.amount(rowField("items", i, "amount"), it.Amount),
			}
		}
	}
	if r.Taxes != nil {
		s.Taxes = make([]SalesTaxesAndCharges, len(r.Taxes))
		for i, tx := range r.Taxes {
			s.Taxes[i] = SalesTaxesAndCharges{
				Name:        tx.Name,
				AccountHead: tx.AccountHead,
				TaxAmount:   c.amount(rowField("taxes", i, "tax_amount"), tx.TaxAmount),
				ChargeType:  tx.ChargeType,
				Rate:        tx.Rate,
				Description: tx.Description,
				GSTTaxType:  tx.GSTTaxType,
			}
		}
	}
	if c.err != nil {
		return SalesInvoice{}, c.err
	}
	return s, nil
}

// Domain converts a Payment Entry with its references.
func (r PaymentEntryRaw) Domain() (PaymentEntry, error) {
	c := newConv(DocTypePaymentEntry, r.Name)
	p := PaymentEntry{
		Name:              r.Name,
		Docstatus:         r.Docstatus,
		Company:           r.Company,
		PaymentType:       r.PaymentType,
		PartyType:         r.PartyType,
		Party:             r.Party,
		PartyName:         r.PartyName,
		PaidAmount:        c.amount("paid_amount", r.PaidAmount),
		ReceivedAmount:    c.amount("received_amount", r.ReceivedAmount),
		PaidFrom:          r.PaidFrom,
		PaidTo:            r.PaidTo,
		ReferenceNo:       r.ReferenceNo,
		ReferenceDate:     c.optDate("reference_date", r.ReferenceDate),
		PostingDate:       c.date("posting_date", r.PostingDate),
		Remarks:           r.Remarks,
		UnallocatedAmount: c.amount("unallocated_amount", r.UnallocatedAmount),
	}
	if r.References != nil {
		p.References = make([]PaymentEntryReference, len(r.References))
		for i, ref := range r.References {
			p.References[i] = PaymentEntryReference{
				Name:              ref.Name,
				ReferenceDoctype:  ref.ReferenceDoctype,
				ReferenceName:     ref.ReferenceName,
				AllocatedAmount:   c.amount(rowField("references", i, "allocated_amount"), ref.AllocatedAmount),
				TotalAmount:       c.amount(rowField("references", i, "total_amount"), ref.TotalAmount),
				OutstandingAmount: c.amount(rowField("references", i, "outstanding_amount"), ref.OutstandingAmount),
			}
		}
	}
	if c.err != nil {
		return PaymentEntry{}, c.err
	}
	return p, nil
}

// Domain converts a Journal Entry with its account rows.
func (r JournalEntryRaw) Domain() (JournalEntry, error) {
	c := newConv(DocTypeJournalEntry, r.Name)
	j := JournalEntry{
		Name:        r.Name,
		Docstatus:   r.Docstatus,
		Company:     r.Company,
		VoucherType: r.VoucherType,
		PostingDate: c.date("posting_date", r.PostingDate),
		UserRemark:  r.UserRemark,
		ChequeNo:    r.ChequeNo,
		ChequeDate:  c.optDate("cheque_date", r.ChequeDate),
		TotalDebit:  c.amount("total_debit", r.TotalDebit),
		TotalCredit: c.amount("total_credit", r.TotalCredit),
	}
	if r.Accounts != nil {
		j.Accounts = make([]JournalEntryAccount, len(r.Accounts))
		for i, a := range r.Accounts {
			j.Accounts[i] = JournalEntryAccount{
				Name:                    a.Name,
				Account:                 a.Account,
				DebitInAccountCurrency:  c.amount(rowField("accounts", i, "debit_in_account_currency"), a.DebitInAccountCurrency),
				CreditInAccountCurrency: c.amount(rowField("accounts", i, "credit_in_account_currency"), a.CreditInAccountCurrency),
				Debit:                   c.amount(rowField("accounts", i, "debit"), a.Debit),
				Credit:                  c.amount(rowField("accounts", i, "credit"), a.Credit),
				PartyType:               a.PartyType,
				Party:                   a.Party,
				ReferenceType:           a.ReferenceType,
				ReferenceName:           a.ReferenceName,
				CostCenter:              a.CostCenter,
				UserRemark:              a.UserRemark,
			}
		}
	}
	if c.err != nil {
		return JournalEntry{}, c.err
	}
	return j, nil
}
