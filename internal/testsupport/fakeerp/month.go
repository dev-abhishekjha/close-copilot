package fakeerp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/frappe"
)

// The synthetic company: the seeded ID, a made-up ERPNext name, and a
// second company whose documents every reader must filter out.
const (
	CompanyID  = "sharma"
	ERPCompany = "Sharma Traders Pvt Ltd"
	OtherID    = "kaveri"
	OtherERP   = "Kaveri Foods Pvt Ltd"
	// Checksum-valid, made-up GSTINs.
	CompanyGSTIN  = "27AAACS1234A1Z2"
	SupplierGSTIN = "27AABCV5678B1Z8"

	// BankAccount is the company's bank account, as the company profile
	// (config/companies/sharma.yaml) names it with its abbreviation.
	BankAccount = "HDFC Current 0001 - STPL"
	// Month is the synthetic close month.
	Month = "2026-09"
	// CashSales is the number of cash-sale journals in September: with
	// two GL Entries each, September spans more than one 500-row page.
	CashSales = 260
)

type glBuilder struct {
	seq     int
	entries []Doc
}

// post adds a balanced voucher: each leg is account, debit, credit (in
// rupees text) and party.
func (g *glBuilder) post(company, date, vtype, vno, remarks string, opening bool, cancelled bool, legs ...[4]string) {
	isOpening := "No"
	if opening {
		isOpening = "Yes"
	}
	canc := 0
	if cancelled {
		canc = 1
	}
	for _, l := range legs {
		g.seq++
		e := Doc{
			"name": fmt.Sprintf("ACC-GLE-2026-%05d", g.seq), "docstatus": 1, "company": company,
			"account": l[0], "debit": l[1], "credit": l[2], "posting_date": date,
			"voucher_type": vtype, "voucher_no": vno, "party_type": "", "party": l[3],
			"is_cancelled": canc, "is_opening": isOpening, "fiscal_year": fiscalYear(date),
			"against": "", "remarks": remarks,
		}
		if l[3] != "" {
			e["party_type"] = "Supplier"
			if strings.HasPrefix(l[3], "CUST") {
				e["party_type"] = "Customer"
			}
		}
		g.entries = append(g.entries, e)
	}
}

func fiscalYear(date string) string {
	d, _ := time.Parse(time.DateOnly, date)
	y := d.Year()
	if d.Month() < time.April {
		y--
	}
	return fmt.Sprintf("%d-%d", y, y+1)
}

func leg(account, debit, credit, party string) [4]string {
	return [4]string{account, debit, credit, party}
}

func account(name, rootType string, group bool) Doc {
	g := 0
	if group {
		g = 1
	}
	base, _, _ := strings.Cut(name, " - ")
	return Doc{"name": name, "docstatus": 0, "account_name": base, "company": ERPCompany, "parent_account": "",
		"is_group": g, "root_type": rootType, "account_type": "", "account_currency": "INR"}
}

func rentInvoice(name, date string, docstatus int, company string) Doc {
	//nolint:gosec // G101 false positive: synthetic invoice fields, no credentials.
	return Doc{
		"name": name, "docstatus": docstatus, "company": company, "supplier": "SUP-RENT", "supplier_name": "Vardhan Estates",
		"bill_no": "VE/" + name, "bill_date": date[:8] + "01", "posting_date": date, "remarks": "Office rent",
		"credit_to": "Creditors - STPL", "net_total": "50000.00", "grand_total": "59000.00", "outstanding_amount": "0",
		"is_return": 0, "supplier_gstin": SupplierGSTIN, "company_gstin": CompanyGSTIN, "place_of_supply": "27-Maharashtra",
		"items": []Doc{{"name": name + "-i1", "item_code": "RENT", "description": "Office rent", "expense_account": "Rent - STPL", "amount": "50000.00"}},
		"taxes": []Doc{
			{"name": name + "-t1", "account_head": "Input Tax CGST - STPL", "tax_amount": "4500.00", "charge_type": "On Net Total",
				"rate": json.Number("9"), "add_deduct_tax": "Add", "category": "Total", "description": "CGST", "gst_tax_type": "cgst"},
			{"name": name + "-t2", "account_head": "Input Tax SGST - STPL", "tax_amount": "4500.00", "charge_type": "On Net Total",
				"rate": json.Number("9"), "add_deduct_tax": "Add", "category": "Total", "description": "SGST", "gst_tax_type": "sgst"},
		},
	}
}

// CashSale is the amount of cash sale i, in rupees text.
func CashSale(i int) string {
	p := 100000 + int64(i)*733 + int64(i%7)*25 // paise
	return fmt.Sprintf("%d.%02d", p/100, p%100)
}

// CashSaleDate is the posting date of cash sale i.
func CashSaleDate(i int) string {
	return fmt.Sprintf("2026-09-%02d", 1+i%29)
}

// NewMonth builds the synthetic month: a chart of accounts, an opening,
// four months of rent bills from one supplier (a recurring supplier),
// September sales, a receipt and a payment, 260 cash-sale journals (more
// than one page of GL Entries), bank charges in July and August, and noise
// the filters must drop: a draft, a cancelled document, a cancelled GL
// pair and another company's documents. The planted September bank
// charges are on the statement only (see Statement), never in the books.
func NewMonth() *ERP {
	f := NewERP()
	f.Add(frappe.DocTypeAccount,
		account("Application of Funds - STPL", "Asset", true),
		account(BankAccount, "Asset", false),
		account("Debtors - STPL", "Asset", false),
		account("Input Tax CGST - STPL", "Asset", false),
		account("Input Tax SGST - STPL", "Asset", false),
		account("Creditors - STPL", "Liability", false),
		account("Output Tax IGST - STPL", "Liability", false),
		account("Capital - STPL", "Equity", false),
		account("Sales - STPL", "Income", false),
		account("Rent - STPL", "Expense", false),
		account("Bank Charges - STPL", "Expense", false),
	)

	g := &glBuilder{}
	g.post(ERPCompany, "2026-04-01", "Journal Entry", "ACC-JV-2026-00001", "Opening", true, false,
		leg(BankAccount, "5000000.00", "0", ""), leg("Capital - STPL", "0", "5000000.00", ""))
	for i, m := range []string{"06", "07", "08", "09"} {
		pinv := fmt.Sprintf("ACC-PINV-2026-%05d", 30+i)
		f.Add(frappe.DocTypePurchaseInvoice, rentInvoice(pinv, "2026-"+m+"-05", 1, ERPCompany))
		g.post(ERPCompany, "2026-"+m+"-05", "Purchase Invoice", pinv, "Office rent", false, false,
			leg("Rent - STPL", "50000.00", "0", ""), leg("Input Tax CGST - STPL", "4500.00", "0", ""),
			leg("Input Tax SGST - STPL", "4500.00", "0", ""), leg("Creditors - STPL", "0", "59000.00", "SUP-RENT"))
	}
	for _, m := range []string{"07-31", "08-31"} {
		g.post(ERPCompany, "2026-"+m, "Journal Entry", "ACC-JV-BC-"+m[:2], "SMS charges", false, false,
			leg("Bank Charges - STPL", "17.70", "0", ""), leg(BankAccount, "0", "17.70", ""))
	}
	// Noise the readers must not return.
	f.Add(frappe.DocTypePurchaseInvoice,
		rentInvoice("ACC-PINV-2026-00090", "2026-09-07", 0, ERPCompany),
		rentInvoice("ACC-PINV-2026-00091", "2026-09-08", 2, ERPCompany),
		rentInvoice("ACC-PINV-2026-00092", "2026-09-08", 1, OtherERP),
	)
	g.post(ERPCompany, "2026-09-15", "Journal Entry", "ACC-JV-2026-00500", "Reversed", false, true,
		leg("Bank Charges - STPL", "999.00", "0", ""), leg(BankAccount, "0", "999.00", ""))
	g.post(OtherERP, "2026-09-15", "Journal Entry", "ACC-JV-OT-00001", "Other company", false, false,
		leg(BankAccount, "10.00", "0", ""), leg("Sales - STPL", "0", "10.00", ""))

	// September: rent paid, a sale collected through the bank.
	f.Add(frappe.DocTypePaymentEntry, Doc{
		"name": "ACC-PAY-2026-00012", "docstatus": 1, "company": ERPCompany, "payment_type": "Pay",
		"party_type": "Supplier", "party": "SUP-RENT", "party_name": "Vardhan Estates",
		"paid_amount": "59000.00", "received_amount": "59000.00", "paid_from": BankAccount, "paid_to": "Creditors - STPL",
		"reference_no": "UTR2026091000123", "reference_date": "2026-09-10", "posting_date": "2026-09-10",
		"remarks": "Rent September", "unallocated_amount": "0",
		"references": []Doc{{"name": "per-1", "reference_doctype": "Purchase Invoice", "reference_name": "ACC-PINV-2026-00033",
			"allocated_amount": "59000.00", "total_amount": "59000.00", "outstanding_amount": "59000.00"}},
	}, Doc{
		"name": "ACC-PAY-2026-00013", "docstatus": 1, "company": ERPCompany, "payment_type": "Receive",
		"party_type": "Customer", "party": "CUST-TATVA", "party_name": "Tatva Retail",
		"paid_amount": "1180.00", "received_amount": "1180.00", "paid_from": "Debtors - STPL", "paid_to": BankAccount,
		"reference_no": "", "reference_date": "", "posting_date": "2026-09-12", "remarks": "Receipt", "unallocated_amount": "0",
		"references": []Doc{{"name": "per-2", "reference_doctype": "Sales Invoice", "reference_name": "ACC-SINV-2026-00044",
			"allocated_amount": "1180.00", "total_amount": "1180.00", "outstanding_amount": "1180.00"}},
	}, Doc{
		"name": "ACC-PAY-2026-00099", "docstatus": 0, "company": ERPCompany, "payment_type": "Pay",
		"paid_amount": "1.00", "received_amount": "1.00", "posting_date": "2026-09-12", "unallocated_amount": "1.00",
		"references": []Doc{},
	})
	g.post(ERPCompany, "2026-09-10", "Payment Entry", "ACC-PAY-2026-00012", "Rent September", false, false,
		leg("Creditors - STPL", "59000.00", "0", "SUP-RENT"), leg(BankAccount, "0", "59000.00", ""))
	f.Add(frappe.DocTypeSalesInvoice, Doc{
		"name": "ACC-SINV-2026-00044", "docstatus": 1, "company": ERPCompany, "customer": "CUST-TATVA", "customer_name": "Tatva Retail",
		"posting_date": "2026-09-02", "net_total": "1000.00", "grand_total": "1180.00", "outstanding_amount": "0",
		"debit_to": "Debtors - STPL", "remarks": "",
		"items": []Doc{{"name": "sii-1", "item_code": "WIDGET", "description": "Widget", "income_account": "Sales - STPL", "amount": "1000.00"}},
		"taxes": []Doc{{"name": "stc-1", "account_head": "Output Tax IGST - STPL", "tax_amount": "180.00", "charge_type": "On Net Total",
			"rate": json.Number("18"), "description": "IGST", "gst_tax_type": "igst"}},
	}, Doc{
		"name": "ACC-SINV-2026-00045", "docstatus": 1, "company": ERPCompany, "customer": "CUST-ANAND", "customer_name": "Anand Stores",
		"posting_date": "2026-09-20", "net_total": "2500.50", "grand_total": "2950.59", "outstanding_amount": "2950.59",
		"debit_to": "Debtors - STPL", "remarks": "On credit",
		"items": []Doc{{"name": "sii-2", "item_code": "GADGET", "description": "Gadget", "income_account": "Sales - STPL", "amount": "2500.50"}},
		"taxes": []Doc{{"name": "stc-2", "account_head": "Output Tax IGST - STPL", "tax_amount": "450.09", "charge_type": "On Net Total",
			"rate": json.Number("18"), "description": "IGST", "gst_tax_type": "igst"}},
	})
	g.post(ERPCompany, "2026-09-02", "Sales Invoice", "ACC-SINV-2026-00044", "", false, false,
		leg("Debtors - STPL", "1180.00", "0", "CUST-TATVA"), leg("Sales - STPL", "0", "1000.00", ""), leg("Output Tax IGST - STPL", "0", "180.00", ""))
	g.post(ERPCompany, "2026-09-20", "Sales Invoice", "ACC-SINV-2026-00045", "On credit", false, false,
		leg("Debtors - STPL", "2950.59", "0", "CUST-ANAND"), leg("Sales - STPL", "0", "2500.50", ""), leg("Output Tax IGST - STPL", "0", "450.09", ""))
	g.post(ERPCompany, "2026-09-12", "Payment Entry", "ACC-PAY-2026-00013", "Receipt", false, false,
		leg(BankAccount, "1180.00", "0", ""), leg("Debtors - STPL", "0", "1180.00", "CUST-TATVA"))
	for i := range CashSales {
		amt := CashSale(i)
		g.post(ERPCompany, CashSaleDate(i), "Journal Entry", fmt.Sprintf("ACC-JV-2026-%05d", 1000+i), "Cash sale", false, false,
			leg(BankAccount, amt, "0", ""), leg("Sales - STPL", "0", amt, ""))
	}
	f.Add(frappe.DocTypeGLEntry, g.entries...)
	return f
}
