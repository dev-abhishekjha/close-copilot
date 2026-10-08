package frappe

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixtureServer serves the recorded-shape responses in testdata/models:
// list_<x>.json for GET /api/resource/<DocType> (one page, then an empty
// one) and get_<x>.json for GET /api/resource/<DocType>/<name>.
func fixtureServer(t *testing.T) *Client {
	t.Helper()
	lists := map[string]string{
		"/api/resource/" + DocTypeAccount:  "list_account.json",
		"/api/resource/" + DocTypeGLEntry:  "list_gl_entry.json",
		"/api/resource/" + DocTypeSupplier: "list_supplier.json",
		"/api/resource/" + DocTypeCustomer: "list_customer.json",
	}
	gets := map[string]string{
		"/api/resource/" + DocTypePurchaseInvoice + "/ACC-PINV-2026-00031": "get_purchase_invoice.json",
		"/api/resource/" + DocTypeSalesInvoice + "/ACC-SINV-2026-00018":    "get_sales_invoice.json",
		"/api/resource/" + DocTypePaymentEntry + "/ACC-PAY-2026-00009":     "get_payment_entry.json",
		"/api/resource/" + DocTypeJournalEntry + "/ACC-JV-2026-00007":      "get_journal_entry.json",
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "models", name))
		if err != nil {
			t.Errorf("fixture: %v", err)
			return ""
		}
		return string(b)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := lists[r.URL.Path]; ok {
			if r.URL.Query().Get("limit_start") != "0" {
				writeRaw(w, http.StatusOK, `{"data":[]}`)
				return
			}
			writeRaw(w, http.StatusOK, read(f))
			return
		}
		if f, ok := gets[r.URL.Path]; ok {
			writeRaw(w, http.StatusOK, read(f))
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		writeRaw(w, http.StatusNotFound, `{"exc_type":"DoesNotExistError"}`)
	})
	c, _ := newTestClient(t, h)
	return c
}

func TestModelsDecode(t *testing.T) {
	c := fixtureServer(t)
	ctx := t.Context()

	cases := []struct {
		name string
		run  func() (got, want any, err error)
	}{
		{"Account", func() (any, any, error) {
			got, err := List[AccountRaw](ctx, c, DocTypeAccount, Query{Fields: Fields[AccountRaw]()})
			return got, []AccountRaw{
				{Name: "Application of Funds (Assets) - STPL", AccountName: "Application of Funds (Assets)", Company: "Sharma Traders Pvt Ltd", IsGroup: 1, RootType: "Asset", AccountCurrency: "INR"},
				{Name: "HDFC Bank - STPL", AccountName: "HDFC Bank", Company: "Sharma Traders Pvt Ltd", ParentAccount: "Bank Accounts - STPL", RootType: "Asset", AccountType: "Bank", AccountCurrency: "INR"},
				{Name: "Bank Charges - STPL", AccountName: "Bank Charges", Company: "Sharma Traders Pvt Ltd", ParentAccount: "Indirect Expenses - STPL", RootType: "Expense", AccountCurrency: "INR"},
			}, err
		}},
		{"GL Entry", func() (any, any, error) {
			got, err := List[GLEntryRaw](ctx, c, DocTypeGLEntry, Query{Fields: Fields[GLEntryRaw]()})
			return got, []GLEntryRaw{
				{Name: "ACC-GLE-2026-00041", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Account: "Bank Charges - STPL", Debit: "1180.5", Credit: "0.0", PostingDate: "2026-09-14", VoucherType: "Journal Entry", VoucherNo: "ACC-JV-2026-00007", IsOpening: "No", FiscalYear: "2026-2027", Against: "HDFC Bank - STPL", Remarks: "Bank charges for September"},
				{Name: "ACC-GLE-2026-00042", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Account: "HDFC Bank - STPL", Debit: "0.0", Credit: "1180.5", PostingDate: "2026-09-14", VoucherType: "Journal Entry", VoucherNo: "ACC-JV-2026-00007", IsOpening: "No", FiscalYear: "2026-2027", Against: "Bank Charges - STPL", Remarks: "Bank charges for September"},
			}, err
		}},
		{"Supplier", func() (any, any, error) {
			got, err := List[SupplierRaw](ctx, c, DocTypeSupplier, Query{Fields: Fields[SupplierRaw]()})
			return got, []SupplierRaw{
				{Name: "Kaveri Packaging", SupplierName: "Kaveri Packaging", SupplierGroup: "Raw Material", SupplierType: "Company", GSTIN: "27ZZZZZ9999Z1Z9", GSTCategory: "Registered Regular", PAN: "ZZZZZ9999Z"},
			}, err
		}},
		{"Customer", func() (any, any, error) {
			got, err := List[CustomerRaw](ctx, c, DocTypeCustomer, Query{Fields: Fields[CustomerRaw]()})
			return got, []CustomerRaw{
				{Name: "Godavari Retail", CustomerName: "Godavari Retail", CustomerGroup: "Commercial", CustomerType: "Company", Territory: "India", GSTIN: "27YYYYY8888Y1Z8", GSTCategory: "Registered Regular"},
			}, err
		}},
		{"Purchase Invoice", func() (any, any, error) {
			got, err := Get[PurchaseInvoiceRaw](ctx, c, DocTypePurchaseInvoice, "ACC-PINV-2026-00031")
			return got, PurchaseInvoiceRaw{
				Name: "ACC-PINV-2026-00031", Docstatus: 1, Company: "Sharma Traders Pvt Ltd",
				Supplier: "Kaveri Packaging", SupplierName: "Kaveri Packaging",
				BillNo: "KP/26-27/0412", BillDate: "2026-09-10", PostingDate: "2026-09-12",
				Remarks: "Packaging material, September", CreditTo: "Creditors - STPL",
				NetTotal: "1000.0", GrandTotal: "1180.0", OutstandingAmount: "1180.0",
				SupplierGSTIN: "27ZZZZZ9999Z1Z9", CompanyGSTIN: "27XXXXX7777X1Z7", PlaceOfSupply: "27-Maharashtra",
				Items: []PurchaseInvoiceItemRaw{
					{Name: "a1b2c3d4e5", ItemCode: "PKG-BOX-S", Description: "Corrugated box, small", ExpenseAccount: "Cost of Goods Sold - STPL", Amount: "1000.0"},
				},
				Taxes: []PurchaseTaxesAndChargesRaw{
					{Name: "f6g7h8i9j0", AccountHead: "Input Tax CGST - STPL", TaxAmount: "90.0", ChargeType: "On Net Total", Rate: "9.0", AddDeductTax: "Add", Category: "Total", Description: "CGST @ 9.0", GSTTaxType: "cgst"},
					{Name: "k1l2m3n4o5", AccountHead: "Input Tax SGST - STPL", TaxAmount: "90.0", ChargeType: "On Net Total", Rate: "9.0", AddDeductTax: "Add", Category: "Total", Description: "SGST @ 9.0", GSTTaxType: "sgst"},
				},
			}, err
		}},
		{"Sales Invoice", func() (any, any, error) {
			got, err := Get[SalesInvoiceRaw](ctx, c, DocTypeSalesInvoice, "ACC-SINV-2026-00018")
			return got, SalesInvoiceRaw{
				Name: "ACC-SINV-2026-00018", Docstatus: 1, Company: "Sharma Traders Pvt Ltd",
				Customer: "Godavari Retail", CustomerName: "Godavari Retail", PostingDate: "2026-09-20",
				NetTotal: "25000.0", GrandTotal: "29500.0", OutstandingAmount: "0.0",
				DebitTo: "Debtors - STPL", Remarks: "No Remarks",
				Items: []SalesInvoiceItemRaw{
					{Name: "p1q2r3s4t5", ItemCode: "PKG-BOX-L", Description: "Corrugated box, large", IncomeAccount: "Sales - STPL", Amount: "25000.0"},
				},
				Taxes: []SalesTaxesAndChargesRaw{
					{Name: "u6v7w8x9y0", AccountHead: "Output Tax IGST - STPL", TaxAmount: "4500.0", ChargeType: "On Net Total", Rate: "18.0", Description: "IGST @ 18.0", GSTTaxType: "igst"},
				},
			}, err
		}},
		{"Payment Entry", func() (any, any, error) {
			got, err := Get[PaymentEntryRaw](ctx, c, DocTypePaymentEntry, "ACC-PAY-2026-00009")
			return got, PaymentEntryRaw{
				Name: "ACC-PAY-2026-00009", Docstatus: 1, Company: "Sharma Traders Pvt Ltd",
				PaymentType: "Pay", PartyType: "Supplier", Party: "Kaveri Packaging", PartyName: "Kaveri Packaging",
				PaidAmount: "1180.0", ReceivedAmount: "1180.0",
				PaidFrom: "HDFC Bank - STPL", PaidTo: "Creditors - STPL",
				ReferenceNo: "UTR2026091500123", ReferenceDate: "2026-09-15", PostingDate: "2026-09-15",
				Remarks: "Amount INR 1,180.00 paid to Kaveri Packaging", UnallocatedAmount: "0.0",
				References: []PaymentEntryReferenceRaw{
					{Name: "z1y2x3w4v5", ReferenceDoctype: "Purchase Invoice", ReferenceName: "ACC-PINV-2026-00031", AllocatedAmount: "1180.0", TotalAmount: "1180.0", OutstandingAmount: "1180.0"},
				},
			}, err
		}},
		{"Journal Entry", func() (any, any, error) {
			got, err := Get[JournalEntryRaw](ctx, c, DocTypeJournalEntry, "ACC-JV-2026-00007")
			return got, JournalEntryRaw{
				Name: "ACC-JV-2026-00007", Docstatus: 1, Company: "Sharma Traders Pvt Ltd",
				VoucherType: "Bank Entry", PostingDate: "2026-09-14", UserRemark: "Bank charges for September",
				ChequeNo: "HDFC-CHG-0914", ChequeDate: "2026-09-14", TotalDebit: "1180.5", TotalCredit: "1180.5",
				Accounts: []JournalEntryAccountRaw{
					{Name: "j1k2l3m4n5", Account: "Bank Charges - STPL", DebitInAccountCurrency: "1180.5", CreditInAccountCurrency: "0.0", Debit: "1180.5", Credit: "0.0", CostCenter: "Main - STPL"},
					{Name: "o6p7q8r9s0", Account: "HDFC Bank - STPL", DebitInAccountCurrency: "0.0", CreditInAccountCurrency: "1180.5", Debit: "0.0", Credit: "1180.5", CostCenter: "Main - STPL"},
				},
			}, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want, err := tc.run()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("decoded\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestModelsDecodeTypes checks the raw structs reject a value of the wrong
// JSON type instead of silently zeroing it.
func TestModelsDecodeTypes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"amount is a word", `{"name":"G","debit":"abc"}`},
		{"amount is an object", `{"name":"G","debit":{}}`},
		{"check is a string", `{"name":"G","is_cancelled":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g GLEntryRaw
			if err := decodeJSON([]byte(tc.body), &g); err == nil {
				t.Errorf("decodeJSON(%s) = %+v, want an error", tc.body, g)
			}
		})
	}
}

func TestFields(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"Account", Fields[AccountRaw](), []string{"name", "docstatus", "account_name", "company", "parent_account", "is_group", "root_type", "account_type", "account_currency"}},
		{"Purchase Invoice skips child tables", Fields[PurchaseInvoiceRaw](), []string{
			"name", "docstatus", "company", "supplier", "supplier_name", "bill_no", "bill_date", "posting_date",
			"remarks", "credit_to", "net_total", "grand_total", "outstanding_amount", "is_return",
			"supplier_gstin", "company_gstin", "place_of_supply",
		}},
		{"Journal Entry skips accounts", Fields[JournalEntryRaw](), []string{
			"name", "docstatus", "company", "voucher_type", "posting_date", "user_remark",
			"cheque_no", "cheque_date", "total_debit", "total_credit",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Errorf("Fields = %q\nwant %q", tc.got, tc.want)
			}
		})
	}
}
