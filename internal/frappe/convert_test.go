package frappe

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestConvert(t *testing.T) {
	cases := []struct {
		name string
		run  func() (got, want any, err error)
	}{
		{"Account", func() (any, any, error) {
			got, err := AccountRaw{Name: "HDFC Bank - STPL", AccountName: "HDFC Bank", Company: "Sharma Traders Pvt Ltd", ParentAccount: "Bank Accounts - STPL", IsGroup: 0, RootType: "Asset", AccountType: "Bank", AccountCurrency: "INR"}.Domain()
			return got, Account{Name: "HDFC Bank - STPL", AccountName: "HDFC Bank", Company: "Sharma Traders Pvt Ltd", ParentAccount: "Bank Accounts - STPL", IsGroup: false, RootType: RootTypeAsset, AccountType: "Bank", AccountCurrency: "INR"}, err
		}},
		{"Account group root", func() (any, any, error) {
			got, err := AccountRaw{Name: "Expenses - STPL", IsGroup: 1, RootType: "Expense"}.Domain()
			return got, Account{Name: "Expenses - STPL", IsGroup: true, RootType: RootTypeExpense}, err
		}},
		{"GL Entry", func() (any, any, error) {
			got, err := GLEntryRaw{
				Name: "ACC-GLE-2026-00041", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Account: "Bank Charges - STPL",
				Debit: json.Number("1180.5"), Credit: json.Number("1e-05"), PostingDate: "2026-09-14", VoucherType: "Journal Entry", VoucherNo: "ACC-JV-2026-00007",
				PartyType: "Supplier", Party: "Kaveri Packaging", IsCancelled: 1, IsOpening: "Yes", FiscalYear: "2026-2027",
				Against: "HDFC Bank - STPL", Remarks: "r",
			}.Domain()
			return got, GLEntry{
				Name: "ACC-GLE-2026-00041", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Account: "Bank Charges - STPL",
				Debit: 118050, Credit: 0, PostingDate: day("2026-09-14"), VoucherType: "Journal Entry", VoucherNo: "ACC-JV-2026-00007",
				PartyType: "Supplier", Party: "Kaveri Packaging", IsCancelled: true, IsOpening: true, FiscalYear: "2026-2027",
				Against: "HDFC Bank - STPL", Remarks: "r",
			}, err
		}},
		{"Supplier", func() (any, any, error) {
			got, err := SupplierRaw{Name: "Kaveri Packaging", SupplierName: "Kaveri Packaging", SupplierGroup: "Raw Material", SupplierType: "Company", GSTIN: "27ZZZZZ9999Z1Z9", GSTCategory: "Registered Regular", PAN: "ZZZZZ9999Z"}.Domain()
			return got, Supplier{Name: "Kaveri Packaging", SupplierName: "Kaveri Packaging", SupplierGroup: "Raw Material", SupplierType: "Company", GSTIN: "27ZZZZZ9999Z1Z9", GSTCategory: "Registered Regular", PAN: "ZZZZZ9999Z"}, err
		}},
		{"Customer", func() (any, any, error) {
			got, err := CustomerRaw{Name: "Godavari Retail", CustomerName: "Godavari Retail", CustomerGroup: "Commercial", CustomerType: "Company", Territory: "India", GSTIN: "27YYYYY8888Y1Z8", GSTCategory: "Registered Regular"}.Domain()
			return got, Customer{Name: "Godavari Retail", CustomerName: "Godavari Retail", CustomerGroup: "Commercial", CustomerType: "Company", Territory: "India", GSTIN: "27YYYYY8888Y1Z8", GSTCategory: "Registered Regular"}, err
		}},
		{"Purchase Invoice", func() (any, any, error) {
			got, err := PurchaseInvoiceRaw{
				Name: "ACC-PINV-2026-00031", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Supplier: "Kaveri Packaging", SupplierName: "Kaveri Packaging",
				BillNo: "KP/0412", BillDate: "2026-09-10", PostingDate: "2026-09-12", Remarks: "r", CreditTo: "Creditors - STPL",
				NetTotal: "1000.0", GrandTotal: "1180.0", OutstandingAmount: "-1180.004", IsReturn: 1,
				SupplierGSTIN: "27ZZZZZ9999Z1Z9", CompanyGSTIN: "27XXXXX7777X1Z7", PlaceOfSupply: "27-Maharashtra",
				Items: []PurchaseInvoiceItemRaw{{Name: "i1", ItemCode: "PKG", Description: "box", ExpenseAccount: "COGS - STPL", Amount: "1000"}},
				Taxes: []PurchaseTaxesAndChargesRaw{{Name: "t1", AccountHead: "Input Tax CGST - STPL", TaxAmount: "90.005", ChargeType: "On Net Total", Rate: "9.0", AddDeductTax: "Add", Category: "Total", Description: "CGST", GSTTaxType: "cgst"}},
			}.Domain()
			return got, PurchaseInvoice{
				Name: "ACC-PINV-2026-00031", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Supplier: "Kaveri Packaging", SupplierName: "Kaveri Packaging",
				BillNo: "KP/0412", BillDate: day("2026-09-10"), PostingDate: day("2026-09-12"), Remarks: "r", CreditTo: "Creditors - STPL",
				NetTotal: 100000, GrandTotal: 118000, OutstandingAmount: -118000, IsReturn: true,
				SupplierGSTIN: "27ZZZZZ9999Z1Z9", CompanyGSTIN: "27XXXXX7777X1Z7", PlaceOfSupply: "27-Maharashtra",
				Items: []PurchaseInvoiceItem{{Name: "i1", ItemCode: "PKG", Description: "box", ExpenseAccount: "COGS - STPL", Amount: 100000}},
				Taxes: []PurchaseTaxesAndCharges{{Name: "t1", AccountHead: "Input Tax CGST - STPL", TaxAmount: 9001, ChargeType: "On Net Total", Rate: "9.0", AddDeductTax: "Add", Category: "Total", Description: "CGST", GSTTaxType: "cgst"}},
			}, err
		}},
		{"Purchase Invoice from List, no child tables, no bill date", func() (any, any, error) {
			got, err := PurchaseInvoiceRaw{Name: "P", PostingDate: "2026-09-12", NetTotal: "0", GrandTotal: "0", OutstandingAmount: "0"}.Domain()
			return got, PurchaseInvoice{Name: "P", PostingDate: day("2026-09-12")}, err
		}},
		{"Sales Invoice", func() (any, any, error) {
			got, err := SalesInvoiceRaw{
				Name: "ACC-SINV-2026-00018", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Customer: "Godavari Retail", CustomerName: "Godavari Retail",
				PostingDate: "2026-09-20", NetTotal: "25000.0", GrandTotal: "2.95E+4", OutstandingAmount: "0.0", DebitTo: "Debtors - STPL", Remarks: "r",
				Items: []SalesInvoiceItemRaw{{Name: "i1", ItemCode: "PKG-L", Description: "box", IncomeAccount: "Sales - STPL", Amount: "25000"}},
				Taxes: []SalesTaxesAndChargesRaw{{Name: "t1", AccountHead: "Output Tax IGST - STPL", TaxAmount: "4500", ChargeType: "On Net Total", Rate: "18", Description: "IGST", GSTTaxType: "igst"}},
			}.Domain()
			return got, SalesInvoice{
				Name: "ACC-SINV-2026-00018", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", Customer: "Godavari Retail", CustomerName: "Godavari Retail",
				PostingDate: day("2026-09-20"), NetTotal: 2500000, GrandTotal: 2950000, OutstandingAmount: 0, DebitTo: "Debtors - STPL", Remarks: "r",
				Items: []SalesInvoiceItem{{Name: "i1", ItemCode: "PKG-L", Description: "box", IncomeAccount: "Sales - STPL", Amount: 2500000}},
				Taxes: []SalesTaxesAndCharges{{Name: "t1", AccountHead: "Output Tax IGST - STPL", TaxAmount: 450000, ChargeType: "On Net Total", Rate: "18", Description: "IGST", GSTTaxType: "igst"}},
			}, err
		}},
		{"Payment Entry", func() (any, any, error) {
			got, err := PaymentEntryRaw{
				Name: "ACC-PAY-2026-00009", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", PaymentType: "Pay", PartyType: "Supplier",
				Party: "Kaveri Packaging", PartyName: "Kaveri Packaging", PaidAmount: "1180.5", ReceivedAmount: "1180.5",
				PaidFrom: "HDFC Bank - STPL", PaidTo: "Creditors - STPL", ReferenceNo: "UTR1", ReferenceDate: "",
				PostingDate: "2026-09-15", Remarks: "r", UnallocatedAmount: "0.5",
				References: []PaymentEntryReferenceRaw{{Name: "r1", ReferenceDoctype: "Purchase Invoice", ReferenceName: "ACC-PINV-2026-00031", AllocatedAmount: "1180", TotalAmount: "1180", OutstandingAmount: "1180"}},
			}.Domain()
			return got, PaymentEntry{
				Name: "ACC-PAY-2026-00009", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", PaymentType: "Pay", PartyType: "Supplier",
				Party: "Kaveri Packaging", PartyName: "Kaveri Packaging", PaidAmount: 118050, ReceivedAmount: 118050,
				PaidFrom: "HDFC Bank - STPL", PaidTo: "Creditors - STPL", ReferenceNo: "UTR1",
				PostingDate: day("2026-09-15"), Remarks: "r", UnallocatedAmount: 50,
				References: []PaymentEntryReference{{Name: "r1", ReferenceDoctype: "Purchase Invoice", ReferenceName: "ACC-PINV-2026-00031", AllocatedAmount: 118000, TotalAmount: 118000, OutstandingAmount: 118000}},
			}, err
		}},
		{"Journal Entry", func() (any, any, error) {
			got, err := JournalEntryRaw{
				Name: "ACC-JV-2026-00007", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", VoucherType: "Bank Entry", PostingDate: "2026-09-14",
				UserRemark: "r", ChequeNo: "CHG-1", ChequeDate: "2026-09-13", TotalDebit: "12345678.9", TotalCredit: "12345678.9",
				Accounts: []JournalEntryAccountRaw{
					{Name: "a1", Account: "Bank Charges - STPL", DebitInAccountCurrency: "12345678.9", CreditInAccountCurrency: "0", Debit: "12345678.9", Credit: "0", CostCenter: "Main - STPL", UserRemark: "u"},
					{Name: "a2", Account: "HDFC Bank - STPL", DebitInAccountCurrency: "0", CreditInAccountCurrency: "12345678.9", Debit: "0", Credit: "12345678.9", PartyType: "Supplier", Party: "Kaveri Packaging", ReferenceType: "Purchase Invoice", ReferenceName: "ACC-PINV-2026-00031"},
				},
			}.Domain()
			return got, JournalEntry{
				Name: "ACC-JV-2026-00007", Docstatus: 1, Company: "Sharma Traders Pvt Ltd", VoucherType: "Bank Entry", PostingDate: day("2026-09-14"),
				UserRemark: "r", ChequeNo: "CHG-1", ChequeDate: day("2026-09-13"), TotalDebit: 1234567890, TotalCredit: 1234567890,
				Accounts: []JournalEntryAccount{
					{Name: "a1", Account: "Bank Charges - STPL", DebitInAccountCurrency: 1234567890, Debit: 1234567890, CostCenter: "Main - STPL", UserRemark: "u"},
					{Name: "a2", Account: "HDFC Bank - STPL", CreditInAccountCurrency: 1234567890, Credit: 1234567890, PartyType: "Supplier", Party: "Kaveri Packaging", ReferenceType: "Purchase Invoice", ReferenceName: "ACC-PINV-2026-00031"},
				},
			}, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want, err := tc.run()
			if err != nil {
				t.Fatalf("Domain: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Domain\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestConvertDates checks dates land on UTC midnight.
func TestConvertDates(t *testing.T) {
	g, err := GLEntryRaw{Name: "G", Debit: "0", Credit: "0", PostingDate: "2026-09-30", IsOpening: "No"}.Domain()
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	if !g.PostingDate.Equal(want) || g.PostingDate.Location() != time.UTC {
		t.Errorf("PostingDate = %v (%v), want %v UTC", g.PostingDate, g.PostingDate.Location(), want)
	}
}

func TestConvertErrors(t *testing.T) {
	gl := func(edit func(*GLEntryRaw)) func() error {
		return func() error {
			r := GLEntryRaw{Name: "ACC-GLE-2026-00042", Debit: "1", Credit: "0", PostingDate: "2026-09-14", IsOpening: "No"}
			edit(&r)
			_, err := r.Domain()
			return err
		}
	}
	pinv := func(edit func(*PurchaseInvoiceRaw)) func() error {
		return func() error {
			r := PurchaseInvoiceRaw{
				Name: "ACC-PINV-2026-00031", PostingDate: "2026-09-12", NetTotal: "1", GrandTotal: "1", OutstandingAmount: "0",
				Items: []PurchaseInvoiceItemRaw{{Amount: "1"}, {Amount: "1"}},
				Taxes: []PurchaseTaxesAndChargesRaw{{TaxAmount: "0"}},
			}
			edit(&r)
			_, err := r.Domain()
			return err
		}
	}
	sinv := func(edit func(*SalesInvoiceRaw)) func() error {
		return func() error {
			r := SalesInvoiceRaw{
				Name: "ACC-SINV-2026-00018", PostingDate: "2026-09-20", NetTotal: "1", GrandTotal: "1", OutstandingAmount: "0",
				Items: []SalesInvoiceItemRaw{{Amount: "1"}},
				Taxes: []SalesTaxesAndChargesRaw{{TaxAmount: "0"}},
			}
			edit(&r)
			_, err := r.Domain()
			return err
		}
	}
	pe := func(edit func(*PaymentEntryRaw)) func() error {
		return func() error {
			r := PaymentEntryRaw{
				Name: "ACC-PAY-2026-00009", PostingDate: "2026-09-15", PaidAmount: "1", ReceivedAmount: "1", UnallocatedAmount: "0",
				References: []PaymentEntryReferenceRaw{{AllocatedAmount: "1", TotalAmount: "1", OutstandingAmount: "1"}},
			}
			edit(&r)
			_, err := r.Domain()
			return err
		}
	}
	je := func(edit func(*JournalEntryRaw)) func() error {
		return func() error {
			r := JournalEntryRaw{
				Name: "ACC-JV-2026-00007", PostingDate: "2026-09-14", TotalDebit: "1", TotalCredit: "1",
				Accounts: []JournalEntryAccountRaw{{DebitInAccountCurrency: "1", CreditInAccountCurrency: "0", Debit: "1", Credit: "0"}},
			}
			edit(&r)
			_, err := r.Domain()
			return err
		}
	}

	cases := []struct {
		name    string
		run     func() error
		want    string // the error starts with this
		wantErr error  // and wraps this, when set
	}{
		{"account bad root type", func() error {
			_, err := AccountRaw{Name: "X - STPL", RootType: "Assets"}.Domain()
			return err
		}, "Account X - STPL: root_type: ", nil},
		{"account empty root type", func() error {
			_, err := AccountRaw{Name: "X - STPL"}.Domain()
			return err
		}, "Account X - STPL: root_type: ", nil},
		{"account bad is_group", func() error {
			_, err := AccountRaw{Name: "X - STPL", RootType: "Asset", IsGroup: 2}.Domain()
			return err
		}, "Account X - STPL: is_group: ", nil},
		{"gl bad amount", gl(func(r *GLEntryRaw) { r.Debit = "abc" }), "GL Entry ACC-GLE-2026-00042: debit: ", money.ErrSyntax},
		{"gl empty amount", gl(func(r *GLEntryRaw) { r.Credit = "" }), "GL Entry ACC-GLE-2026-00042: credit: ", errEmpty},
		{"gl overflow", gl(func(r *GLEntryRaw) { r.Credit = "1e30" }), "GL Entry ACC-GLE-2026-00042: credit: ", money.ErrOverflow},
		{"gl bad date", gl(func(r *GLEntryRaw) { r.PostingDate = "2026-02-30" }), "GL Entry ACC-GLE-2026-00042: posting_date: ", nil},
		{"gl datetime not a date", gl(func(r *GLEntryRaw) { r.PostingDate = "2026-09-14 10:00:00" }), "GL Entry ACC-GLE-2026-00042: posting_date: ", nil},
		{"gl empty date", gl(func(r *GLEntryRaw) { r.PostingDate = "" }), "GL Entry ACC-GLE-2026-00042: posting_date: ", errEmpty},
		{"gl bad is_cancelled", gl(func(r *GLEntryRaw) { r.IsCancelled = -1 }), "GL Entry ACC-GLE-2026-00042: is_cancelled: ", nil},
		{"gl bad is_opening", gl(func(r *GLEntryRaw) { r.IsOpening = "yes" }), "GL Entry ACC-GLE-2026-00042: is_opening: ", nil},
		{"gl unnamed", gl(func(r *GLEntryRaw) { r.Name = ""; r.Debit = "x" }), "GL Entry (unnamed): debit: ", money.ErrSyntax},
		{"gl first error wins", gl(func(r *GLEntryRaw) { r.Debit = "x"; r.PostingDate = "bad" }), "GL Entry ACC-GLE-2026-00042: debit: ", money.ErrSyntax},
		{"pinv bad grand total", pinv(func(r *PurchaseInvoiceRaw) { r.GrandTotal = "1,180.00" }), "Purchase Invoice ACC-PINV-2026-00031: grand_total: ", money.ErrSyntax},
		{"pinv bad bill date", pinv(func(r *PurchaseInvoiceRaw) { r.BillDate = "10-09-2026" }), "Purchase Invoice ACC-PINV-2026-00031: bill_date: ", nil},
		{"pinv empty posting date", pinv(func(r *PurchaseInvoiceRaw) { r.PostingDate = "" }), "Purchase Invoice ACC-PINV-2026-00031: posting_date: ", errEmpty},
		{"pinv bad is_return", pinv(func(r *PurchaseInvoiceRaw) { r.IsReturn = 3 }), "Purchase Invoice ACC-PINV-2026-00031: is_return: ", nil},
		{"pinv bad item amount", pinv(func(r *PurchaseInvoiceRaw) { r.Items[1].Amount = "1..0" }), "Purchase Invoice ACC-PINV-2026-00031: items[1].amount: ", money.ErrSyntax},
		{"pinv empty tax amount", pinv(func(r *PurchaseInvoiceRaw) { r.Taxes[0].TaxAmount = "" }), "Purchase Invoice ACC-PINV-2026-00031: taxes[0].tax_amount: ", errEmpty},
		{"sinv bad net total", sinv(func(r *SalesInvoiceRaw) { r.NetTotal = "NaN" }), "Sales Invoice ACC-SINV-2026-00018: net_total: ", money.ErrSyntax},
		{"sinv bad date", sinv(func(r *SalesInvoiceRaw) { r.PostingDate = "2026-13-01" }), "Sales Invoice ACC-SINV-2026-00018: posting_date: ", nil},
		{"sinv bad item amount", sinv(func(r *SalesInvoiceRaw) { r.Items[0].Amount = "-" }), "Sales Invoice ACC-SINV-2026-00018: items[0].amount: ", money.ErrSyntax},
		{"sinv bad tax amount", sinv(func(r *SalesInvoiceRaw) { r.Taxes[0].TaxAmount = "1e" }), "Sales Invoice ACC-SINV-2026-00018: taxes[0].tax_amount: ", money.ErrSyntax},
		{"pe bad paid amount", pe(func(r *PaymentEntryRaw) { r.PaidAmount = "1e999" }), "Payment Entry ACC-PAY-2026-00009: paid_amount: ", money.ErrOverflow},
		{"pe bad reference date", pe(func(r *PaymentEntryRaw) { r.ReferenceDate = "2026/09/15" }), "Payment Entry ACC-PAY-2026-00009: reference_date: ", nil},
		{"pe empty unallocated", pe(func(r *PaymentEntryRaw) { r.UnallocatedAmount = "" }), "Payment Entry ACC-PAY-2026-00009: unallocated_amount: ", errEmpty},
		{"pe bad reference amount", pe(func(r *PaymentEntryRaw) { r.References[0].OutstandingAmount = "x" }), "Payment Entry ACC-PAY-2026-00009: references[0].outstanding_amount: ", money.ErrSyntax},
		{"je bad total", je(func(r *JournalEntryRaw) { r.TotalCredit = "abc" }), "Journal Entry ACC-JV-2026-00007: total_credit: ", money.ErrSyntax},
		{"je bad cheque date", je(func(r *JournalEntryRaw) { r.ChequeDate = "yesterday" }), "Journal Entry ACC-JV-2026-00007: cheque_date: ", nil},
		{"je empty posting date", je(func(r *JournalEntryRaw) { r.PostingDate = "" }), "Journal Entry ACC-JV-2026-00007: posting_date: ", errEmpty},
		{"je bad account debit", je(func(r *JournalEntryRaw) { r.Accounts[0].Debit = "1.2.3" }), "Journal Entry ACC-JV-2026-00007: accounts[0].debit: ", money.ErrSyntax},
		{"je bad account credit in currency", je(func(r *JournalEntryRaw) { r.Accounts[0].CreditInAccountCurrency = "" }), "Journal Entry ACC-JV-2026-00007: accounts[0].credit_in_account_currency: ", errEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("Domain succeeded, want an error starting %q", tc.want)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("error %q\nwant prefix %q", err, tc.want)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error %v does not wrap %v", err, tc.wantErr)
			}
		})
	}
}

// TestConvertOptionalDates checks the optional dates stay zero when empty.
func TestConvertOptionalDates(t *testing.T) {
	p, err := PaymentEntryRaw{Name: "P", PostingDate: "2026-09-15", PaidAmount: "0", ReceivedAmount: "0", UnallocatedAmount: "0"}.Domain()
	if err != nil || !p.ReferenceDate.IsZero() {
		t.Errorf("Payment Entry reference_date: %v, %v; want zero", p.ReferenceDate, err)
	}
	j, err := JournalEntryRaw{Name: "J", PostingDate: "2026-09-15", TotalDebit: "0", TotalCredit: "0"}.Domain()
	if err != nil || !j.ChequeDate.IsZero() {
		t.Errorf("Journal Entry cheque_date: %v, %v; want zero", j.ChequeDate, err)
	}
	pi, err := PurchaseInvoiceRaw{Name: "PI", PostingDate: "2026-09-15", NetTotal: "0", GrandTotal: "0", OutstandingAmount: "0"}.Domain()
	if err != nil || !pi.BillDate.IsZero() {
		t.Errorf("Purchase Invoice bill_date: %v, %v; want zero", pi.BillDate, err)
	}
}

// TestConvertDecoded runs every fixture in testdata/models through its
// converter, so the recorded shapes and the converters agree.
func TestConvertDecoded(t *testing.T) {
	c := fixtureServer(t)
	ctx := t.Context()
	check := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Run("Account", func(t *testing.T) {
		rows, err := List[AccountRaw](ctx, c, DocTypeAccount, Query{})
		check(t, err)
		for _, r := range rows {
			_, err := r.Domain()
			check(t, err)
		}
	})
	t.Run("GL Entry", func(t *testing.T) {
		rows, err := List[GLEntryRaw](ctx, c, DocTypeGLEntry, Query{})
		check(t, err)
		var debit, credit money.Paise
		for _, r := range rows {
			g, err := r.Domain()
			check(t, err)
			debit += g.Debit
			credit += g.Credit
		}
		if debit != 118050 || credit != 118050 {
			t.Errorf("debit %s credit %s, want 1180.50 each", debit.Rupees(), credit.Rupees())
		}
	})
	t.Run("Purchase Invoice", func(t *testing.T) {
		r, err := Get[PurchaseInvoiceRaw](ctx, c, DocTypePurchaseInvoice, "ACC-PINV-2026-00031")
		check(t, err)
		p, err := r.Domain()
		check(t, err)
		var sum money.Paise
		for _, it := range p.Items {
			sum += it.Amount
		}
		for _, tx := range p.Taxes {
			sum += tx.TaxAmount
		}
		if sum != p.GrandTotal || p.GrandTotal.Format() != "₹1,180.00" {
			t.Errorf("items+taxes %s, grand total %s", sum.Format(), p.GrandTotal.Format())
		}
	})
	t.Run("Sales Invoice", func(t *testing.T) {
		r, err := Get[SalesInvoiceRaw](ctx, c, DocTypeSalesInvoice, "ACC-SINV-2026-00018")
		check(t, err)
		s, err := r.Domain()
		check(t, err)
		if len(s.Items) != 1 || len(s.Taxes) != 1 || s.GrandTotal != 2950000 {
			t.Errorf("sales invoice %+v", s)
		}
	})
	t.Run("Payment Entry", func(t *testing.T) {
		r, err := Get[PaymentEntryRaw](ctx, c, DocTypePaymentEntry, "ACC-PAY-2026-00009")
		check(t, err)
		p, err := r.Domain()
		check(t, err)
		if len(p.References) != 1 || p.References[0].AllocatedAmount != p.PaidAmount || !p.ReferenceDate.Equal(day("2026-09-15")) {
			t.Errorf("payment entry %+v", p)
		}
	})
	t.Run("Journal Entry", func(t *testing.T) {
		r, err := Get[JournalEntryRaw](ctx, c, DocTypeJournalEntry, "ACC-JV-2026-00007")
		check(t, err)
		j, err := r.Domain()
		check(t, err)
		var dr, cr money.Paise
		for _, a := range j.Accounts {
			dr += a.Debit
			cr += a.Credit
		}
		if dr != j.TotalDebit || cr != j.TotalCredit || dr != 118050 {
			t.Errorf("journal entry rows %d/%d, totals %d/%d", dr, cr, j.TotalDebit, j.TotalCredit)
		}
	})
}
