package seed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/frappe"
)

// ExtIDField is the Custom Field on Sales Invoice, Purchase Invoice,
// Payment Entry and Journal Entry that holds an event's ExtID. Bootstrap
// reads the stored fieldname back and fails if Frappe stored another one.
const ExtIDField = "copilot_ext_id"

// extIDLabel is the custom field's label.
const extIDLabel = "Copilot Ext ID"

// extIDDocTypes carry the external-ID field.
var extIDDocTypes = []string{"Sales Invoice", "Purchase Invoice", "Payment Entry", "Journal Entry"}

// The fiscal year the books need: April 2026 to March 2027.
const (
	fiscalYearStart = "2026-04-01"
	fiscalYearEnd   = "2027-03-31"
)

// GST categories (India Compliance).
const (
	gstRegisteredRegular = "Registered Regular"
	gstUnregistered      = "Unregistered"
)

const (
	countryIndia   = "India"
	addressBilling = "Billing"
	uomNos         = "Nos"
)

// DocType names Bootstrap reads or writes.
const (
	dtCompany       = "Company"
	dtFiscalYear    = "Fiscal Year"
	dtAccount       = "Account"
	dtAddress       = "Address"
	dtGSTSettings   = "GST Settings"
	dtCustomField   = "Custom Field"
	dtSupplier      = "Supplier"
	dtCustomer      = "Customer"
	dtItem          = "Item"
	dtGSTHSNCode    = "GST HSN Code"
	dtSupplierGroup = "Supplier Group"
	dtCustomerGroup = "Customer Group"
	dtItemGroup     = "Item Group"
	dtTerritory     = "Territory"

	dtItemTaxTemplate = "Item Tax Template"
)

// stateInfo is what an address in a state needs: India Compliance checks
// that the state matches the GSTIN's state code and that the pincode
// belongs to the state.
type stateInfo struct {
	Name    string // as India Compliance spells it (Address.gst_state options)
	City    string
	Pincode string
}

// states covers every state code the company profiles use. A test checks
// config/companies against it.
var states = map[string]stateInfo{
	"07": {Name: "Delhi", City: "New Delhi", Pincode: "110001"},
	"24": {Name: "Gujarat", City: "Ahmedabad", Pincode: "380001"},
	"27": {Name: "Maharashtra", City: "Mumbai", Pincode: "400001"},
	"29": {Name: "Karnataka", City: "Bengaluru", Pincode: "560001"},
	"33": {Name: "Tamil Nadu", City: "Chennai", Pincode: "600001"},
	"36": {Name: "Telangana", City: "Hyderabad", Pincode: "500001"},
}

// syntheticStreet is the first address line of every generated address.
const syntheticStreet = "1 Synthetic Road"

// Account placement. Bootstrap creates a missing account under its parent
// group, which must already exist in the company's chart (ERPNext's
// standard Indian chart has all of them). Sales, Debtors and Creditors must
// already exist; Bootstrap fails if they don't.
//
//	| Account                   | Parent group      | Root      | account_type       |
//	|---------------------------|-------------------|-----------|--------------------|
//	| <profile bank.account>    | Bank Accounts     | Asset     | Bank               |
//	| Payment Gateway Clearing  | Current Assets    | Asset     |                    |
//	| Prepaid Expenses          | Current Assets    | Asset     |                    |
//	| Office Equipment          | Fixed Assets      | Asset     | Fixed Asset        |
//	| Gateway Fees              | Indirect Expenses | Expense   |                    |
//	| Bank Charges              | Indirect Expenses | Expense   |                    |
//	| Rent                      | Indirect Expenses | Expense   |                    |
//	| Electricity               | Indirect Expenses | Expense   |                    |
//	| Telephone and Internet    | Indirect Expenses | Expense   |                    |
//	| Software Subscriptions    | Indirect Expenses | Expense   |                    |
//	| Repairs and Maintenance   | Indirect Expenses | Expense   |                    |
//	| Salaries                  | Indirect Expenses | Expense   |                    |
//	| Professional Charges      | Indirect Expenses | Expense   |                    |
//	| Cost of Goods Sold        | Direct Expenses   | Expense   | Cost of Goods Sold |
//	| Interest Income           | Indirect Income   | Income    |                    |
//	| Sales                     | (must exist)      | Income    |                    |
//	| Debtors                   | (must exist)      | Asset     |                    |
//	| Creditors                 | (must exist)      | Liability |                    |
//
// Cost of Goods Sold normally exists already (under Stock Expenses); it is
// created under Direct Expenses only when it's missing. The input GST
// accounts go under Tax Assets and the output ones under Duties and Taxes,
// both with account_type Tax, and only when GST Settings lacks them.
type accountSpec struct {
	parent      string // parent group's account_name; empty means it must exist
	rootType    string
	accountType string // owned by Bootstrap when not empty
}

const (
	rootAsset     = "Asset"
	rootLiability = "Liability"
	rootIncome    = "Income"
	rootExpense   = "Expense"

	groupBankAccounts    = "Bank Accounts"
	groupCurrentAssets   = "Current Assets"
	groupFixedAssets     = "Fixed Assets"
	groupIndirectExpense = "Indirect Expenses"
	groupDirectExpense   = "Direct Expenses"
	groupIndirectIncome  = "Indirect Income"
	groupTaxAssets       = "Tax Assets"
	groupDutiesAndTaxes  = "Duties and Taxes"
)

var accountSpecs = map[string]accountSpec{
	AccountSales:                  {rootType: rootIncome},
	AccountDebtors:                {rootType: rootAsset},
	AccountCreditors:              {rootType: rootLiability},
	AccountPaymentGatewayClearing: {parent: groupCurrentAssets, rootType: rootAsset},
	AccountPrepaidExpenses:        {parent: groupCurrentAssets, rootType: rootAsset},
	AccountOfficeEquipment:        {parent: groupFixedAssets, rootType: rootAsset, accountType: "Fixed Asset"},
	AccountGatewayFees:            {parent: groupIndirectExpense, rootType: rootExpense},
	AccountBankCharges:            {parent: groupIndirectExpense, rootType: rootExpense},
	AccountRent:                   {parent: groupIndirectExpense, rootType: rootExpense},
	AccountElectricity:            {parent: groupIndirectExpense, rootType: rootExpense},
	AccountTelephoneInternet:      {parent: groupIndirectExpense, rootType: rootExpense},
	AccountSoftwareSubscriptions:  {parent: groupIndirectExpense, rootType: rootExpense},
	AccountRepairsMaintenance:     {parent: groupIndirectExpense, rootType: rootExpense},
	AccountSalaries:               {parent: groupIndirectExpense, rootType: rootExpense},
	AccountProfessionalCharges:    {parent: groupIndirectExpense, rootType: rootExpense},
	AccountCostOfGoodsSold:        {parent: groupDirectExpense, rootType: rootExpense, accountType: "Cost of Goods Sold"},
	AccountInterestIncome:         {parent: groupIndirectIncome, rootType: rootIncome},
}

var bankAccountSpec = accountSpec{parent: groupBankAccounts, rootType: rootAsset, accountType: "Bank"}

// GST account rows (GST Settings, gst_accounts table).
const (
	gstInput  = "Input"
	gstOutput = "Output"
)

// gstDefaults are India Compliance's default GST account names, without
// the abbreviation, used when GST Settings lacks a row or an account.
var gstDefaults = map[string]struct {
	CGST, SGST, IGST string
	spec             accountSpec
}{
	gstInput:  {"Input Tax CGST", "Input Tax SGST", "Input Tax IGST", accountSpec{parent: groupTaxAssets, rootType: rootAsset, accountType: "Tax"}},
	gstOutput: {"Output Tax CGST", "Output Tax SGST", "Output Tax IGST", accountSpec{parent: groupDutiesAndTaxes, rootType: rootLiability, accountType: "Tax"}},
}

// ERPAccount returns the ERPNext name of a company's account: the base
// name and the company abbreviation, "Rent - STPL". This is ERPNext's
// naming rule for an account without an account number, and Bootstrap
// checks it for every account it resolves.
func ERPAccount(base, abbr string) string { return base + " - " + abbr }

// Item codes (item_code equals item_name). All are non-stock.
const (
	ItemSales        = "Trading Goods"
	ItemRent         = "Rent Service"
	ItemUtility      = "Utility Service"
	ItemSubscription = "Software Subscription"
	ItemGoods        = "Goods for Resale"
	ItemServices     = "Professional Service"
	ItemLaptop       = "Laptop"
)

// itemSpec is one non-stock item. HSN is an HSN or SAC code that must
// exist as a GST HSN Code record (the site requires at least 6 digits).
type itemSpec struct {
	Code        string
	Group       string // preferred Item Group
	HSN         string
	Sales       bool
	Description string
}

var itemSpecs = []itemSpec{
	{Code: ItemSales, Group: "Products", HSN: "392490", Sales: true, Description: "Goods sold to customers"},
	{Code: ItemRent, Group: "Services", HSN: "997212", Description: "Rent of non-residential premises"},
	{Code: ItemUtility, Group: "Services", HSN: "998599", Description: "Utility charges: power, telephone and internet"},
	{Code: ItemSubscription, Group: "Services", HSN: "997331", Description: "Software subscription"},
	{Code: ItemGoods, Group: "Products", HSN: "392490", Description: "Goods bought for resale"},
	{Code: ItemServices, Group: "Services", HSN: "998399", Description: "Professional and business services"},
	{Code: ItemLaptop, Group: "Products", HSN: "847130", Description: "Laptop for office use (capital asset)"},
}

// ItemFor returns the item a document line uses: the sales item for a
// sale; for a purchase, the laptop when isAsset, otherwise the supplier
// kind's item. Event kinds without item lines (payments and journals)
// return "".
func ItemFor(eventKind, supplierKind string, isAsset bool) string {
	switch eventKind {
	case EventSale:
		return ItemSales
	case EventPurchase:
		if isAsset {
			return ItemLaptop
		}
		switch supplierKind {
		case KindRent:
			return ItemRent
		case KindUtility:
			return ItemUtility
		case KindSubscription:
			return ItemSubscription
		case KindGoods:
			return ItemGoods
		case KindServices:
			return ItemServices
		}
	}
	return ""
}

// Counts are one DocType's results in a BootstrapReport.
type Counts struct {
	Created      int      `json:"created"`
	Updated      int      `json:"updated"`
	Unchanged    int      `json:"unchanged"`
	CreatedNames []string `json:"created_names,omitempty"`
	UpdatedNames []string `json:"updated_names,omitempty"`
}

// GSTAccounts are the CGST, SGST and IGST accounts of one GST Settings row.
type GSTAccounts struct {
	CGST string `json:"cgst"`
	SGST string `json:"sgst"`
	IGST string `json:"igst"`
}

// BootstrapReport is what Bootstrap did and what it resolved. CC-304 reads
// the resolved names.
type BootstrapReport struct {
	Company string `json:"company"`
	Abbr    string `json:"abbr"`
	// Counts is keyed by DocType.
	Counts map[string]*Counts `json:"counts"`
	// Accounts maps each base account name (seed.Accounts and the bank
	// account) to its ERPNext name.
	Accounts       map[string]string `json:"accounts"`
	BankAccount    string            `json:"bank_account"`
	InputGST       GSTAccounts       `json:"input_gst"`
	OutputGST      GSTAccounts       `json:"output_gst"`
	ExtIDField     string            `json:"ext_id_field"`
	CompanyAddress string            `json:"company_address"`
	// Suppliers maps a profile supplier id to its ERPNext name, and
	// SupplierAddresses a registered supplier id to its Address.
	Suppliers         map[string]string `json:"suppliers"`
	SupplierAddresses map[string]string `json:"supplier_addresses"`
	Customers         []string          `json:"customers"`
	Items             []string          `json:"items"`
	// ItemTaxTemplates maps each GST rate the company's world uses ("18")
	// to the company's Item Tax Template. ERPNext clears the rate of a tax
	// row with charge_type Actual, so India Compliance takes each line's
	// rate from its template; without one it refuses an invoice with
	// explicit (Actual) GST rows.
	ItemTaxTemplates map[string]string `json:"item_tax_templates"`
}

// Changes is the number of records created or updated.
func (r BootstrapReport) Changes() int {
	n := 0
	for _, c := range r.Counts {
		n += c.Created + c.Updated
	}
	return n
}

// Bootstrap creates the master data the books need for profile p in
// ERPNext: it checks the company and its fiscal year, then ensures the
// company's GST details and address, the accounts, GST Settings, the
// external-ID custom field, suppliers (with addresses), customers and
// items. Every record is looked up first and created only if missing; an
// existing one gets only the fields Bootstrap owns that differ. A second
// run reports zero changes. c must use the seeder (Administrator) key.
func Bootstrap(ctx context.Context, c *frappe.Client, p Profile) (BootstrapReport, error) {
	if c == nil {
		return BootstrapReport{}, errors.New("bootstrap: nil frappe client")
	}
	b := &boot{
		c: c,
		p: p,
		rep: BootstrapReport{
			Company:           p.ERPCompany,
			Abbr:              p.Abbr,
			Counts:            map[string]*Counts{},
			Accounts:          map[string]string{},
			Suppliers:         map[string]string{},
			SupplierAddresses: map[string]string{},
			Customers:         []string{},
			Items:             []string{},
			ItemTaxTemplates:  map[string]string{},
		},
		groups: map[string]string{},
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"company", b.company},
		{"company address", b.companyAddress},
		{"accounts", b.accounts},
		{"GST settings", b.gstSettings},
		{"item tax templates", b.itemTaxTemplates},
		{"custom field", b.customFields},
		{"suppliers", b.suppliers},
		{"customers", b.customers},
		{"items", b.items},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return b.rep, fmt.Errorf("bootstrap %s: %s: %w", p.ID, s.name, err)
		}
	}
	return b.rep, nil
}

type boot struct {
	c      *frappe.Client
	p      Profile
	rep    BootstrapReport
	gstin  string
	groups map[string]string // picked group per DocType
}

type doc = map[string]any

func (b *boot) counts(doctype string) *Counts {
	c, ok := b.rep.Counts[doctype]
	if !ok {
		c = &Counts{}
		b.rep.Counts[doctype] = c
	}
	return c
}

// list returns the rows of doctype matching filters, with name and fields,
// in name order.
func (b *boot) list(ctx context.Context, doctype string, filters [][]any, fields ...string) ([]doc, error) {
	return frappe.List[doc](ctx, b.c, doctype, frappe.Query{
		Fields:  append([]string{"name"}, fields...),
		Filters: filters,
		OrderBy: "name asc",
	})
}

// get returns a document, or nil when it doesn't exist.
func (b *boot) get(ctx context.Context, doctype, name string) (doc, error) {
	d, err := frappe.Get[doc](ctx, b.c, doctype, name)
	if frappe.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ensure creates want when existing is nil, or updates the owned fields of
// existing that differ from want. It returns the document's name.
func (b *boot) ensure(ctx context.Context, doctype string, existing, want doc, owned []string) (string, error) {
	cnt := b.counts(doctype)
	if existing == nil {
		body := doc{}
		for k, v := range want {
			body[k] = v
		}
		saved, err := frappe.Insert(ctx, b.c, doctype, body)
		if err != nil {
			return "", err
		}
		name := str(saved["name"])
		if name == "" {
			return "", fmt.Errorf("insert %s: no name in the response", doctype)
		}
		cnt.Created++
		cnt.CreatedNames = append(cnt.CreatedNames, name)
		return name, nil
	}
	name := str(existing["name"])
	diff := doc{}
	for _, f := range owned {
		w, ok := want[f]
		if !ok {
			continue
		}
		if str(existing[f]) != str(w) {
			diff[f] = w
		}
	}
	if len(diff) == 0 {
		cnt.Unchanged++
		return name, nil
	}
	if _, err := frappe.Update(ctx, b.c, doctype, name, diff); err != nil {
		return "", err
	}
	cnt.Updated++
	cnt.UpdatedNames = append(cnt.UpdatedNames, name)
	return name, nil
}

// str is a field value's comparable form: "" for null, the decimal text of
// a number, and the string itself otherwise.
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// company checks the company, its abbreviation and the fiscal year, and
// sets the company's GST fields.
func (b *boot) company(ctx context.Context) error {
	if b.p.ERPCompany == "" || b.p.Abbr == "" {
		return errors.New("the profile has no erp_company or abbr")
	}
	co, err := b.get(ctx, dtCompany, b.p.ERPCompany)
	if err != nil {
		return err
	}
	if co == nil {
		return fmt.Errorf("company %q does not exist in ERPNext; create it first", b.p.ERPCompany)
	}
	if abbr := str(co["abbr"]); abbr != b.p.Abbr {
		return fmt.Errorf("company %q has abbreviation %q, the profile says %q", b.p.ERPCompany, abbr, b.p.Abbr)
	}
	if err := b.fiscalYear(ctx); err != nil {
		return err
	}
	gstin, err := b.p.GSTIN()
	if err != nil {
		return err
	}
	b.gstin = gstin

	// India Compliance adds gstin, gst_category and pan to Company as
	// custom fields; set only those that exist.
	rows, err := b.list(ctx, dtCustomField, [][]any{
		{"dt", "=", dtCompany},
		{"fieldname", "in", []string{"gstin", "gst_category", "pan"}},
	}, "fieldname")
	if err != nil {
		return err
	}
	want := doc{"gstin": gstin, "gst_category": gstRegisteredRegular, "pan": b.p.PAN}
	var owned []string
	for _, r := range rows {
		owned = append(owned, str(r["fieldname"]))
	}
	sort.Strings(owned)
	_, err = b.ensure(ctx, dtCompany, co, want, owned)
	return err
}

// fiscalYear checks that an enabled Fiscal Year covering April 2026 to
// March 2027 applies to the company: its companies table is empty or
// names it.
func (b *boot) fiscalYear(ctx context.Context) error {
	rows, err := b.list(ctx, dtFiscalYear, [][]any{
		{"year_start_date", "<=", fiscalYearStart},
		{"year_end_date", ">=", fiscalYearEnd},
		{"disabled", "=", 0},
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		fy, err := b.get(ctx, dtFiscalYear, str(r["name"]))
		if err != nil {
			return err
		}
		if fy == nil {
			continue
		}
		cos, _ := fy["companies"].([]any)
		if len(cos) == 0 {
			return nil
		}
		for _, row := range cos {
			if m, ok := row.(map[string]any); ok && str(m["company"]) == b.p.ERPCompany {
				return nil
			}
		}
	}
	return fmt.Errorf("no enabled Fiscal Year covering %s to %s applies to company %q", fiscalYearStart, fiscalYearEnd, b.p.ERPCompany)
}

// address ensures a Billing address titled title, linked to
// (linkDoctype, linkName), in the given state with the GSTIN.
func (b *boot) address(ctx context.Context, title, linkDoctype, linkName, stateCode, gstin string, companyAddress bool) (string, error) {
	st, ok := states[stateCode]
	if !ok {
		return "", fmt.Errorf("no state name and pincode for state code %q; add it to seed.states", stateCode)
	}
	rows, err := b.list(ctx, dtAddress, [][]any{
		{"address_title", "=", title},
		{"address_type", "=", addressBilling},
	})
	if err != nil {
		return "", err
	}
	var existing doc
	for _, r := range rows {
		a, err := b.get(ctx, dtAddress, str(r["name"]))
		if err != nil {
			return "", err
		}
		if a != nil && linksTo(a, linkDoctype, linkName) {
			existing = a
			break
		}
	}
	isCompany := 0
	if companyAddress {
		isCompany = 1
	}
	want := doc{
		"address_title":           title,
		"address_type":            addressBilling,
		"address_line1":           syntheticStreet,
		"city":                    st.City,
		"state":                   st.Name,
		"pincode":                 st.Pincode,
		"country":                 countryIndia,
		"gstin":                   gstin,
		"gst_category":            gstRegisteredRegular,
		"is_primary_address":      1,
		"is_your_company_address": isCompany,
		"links":                   []doc{{"link_doctype": linkDoctype, "link_name": linkName}},
	}
	owned := []string{"address_line1", "city", "state", "pincode", "country", "gstin", "gst_category", "is_primary_address", "is_your_company_address"}
	return b.ensure(ctx, dtAddress, existing, want, owned)
}

func linksTo(a doc, linkDoctype, linkName string) bool {
	links, _ := a["links"].([]any)
	for _, l := range links {
		if m, ok := l.(map[string]any); ok && str(m["link_doctype"]) == linkDoctype && str(m["link_name"]) == linkName {
			return true
		}
	}
	return false
}

func (b *boot) companyAddress(ctx context.Context) error {
	name, err := b.address(ctx, b.p.ERPCompany, dtCompany, b.p.ERPCompany, b.p.StateCode, b.gstin, true)
	if err != nil {
		return err
	}
	b.rep.CompanyAddress = name
	return nil
}

// findAccount returns the company's account with this account_name, or nil.
func (b *boot) findAccount(ctx context.Context, accountName string) (doc, error) {
	rows, err := b.list(ctx, dtAccount, [][]any{
		{"company", "=", b.p.ERPCompany},
		{"account_name", "=", accountName},
	}, "account_name", "company", "is_group", "root_type", "account_type", "parent_account")
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
		if str(rows[0]["company"]) != b.p.ERPCompany || str(rows[0]["account_name"]) != accountName {
			return nil, fmt.Errorf("account lookup for %q in %q returned %q of %q", accountName, b.p.ERPCompany, rows[0]["account_name"], rows[0]["company"])
		}
		return rows[0], nil
	}
	return nil, fmt.Errorf("company %q has %d accounts named %q", b.p.ERPCompany, len(rows), accountName)
}

// ensureAccount resolves (and if allowed, creates) the company's ledger
// account base and returns its ERPNext name.
func (b *boot) ensureAccount(ctx context.Context, base string, spec accountSpec) (string, error) {
	acc, err := b.findAccount(ctx, base)
	if err != nil {
		return "", err
	}
	if acc == nil && spec.parent == "" {
		return "", fmt.Errorf("account %q does not exist in company %q; ERPNext's chart should have it", base, b.p.ERPCompany)
	}
	if acc != nil {
		if str(acc["is_group"]) != "0" {
			return "", fmt.Errorf("account %q of %q is a group, want a ledger", str(acc["name"]), b.p.ERPCompany)
		}
		if rt := str(acc["root_type"]); rt != spec.rootType {
			return "", fmt.Errorf("account %q of %q has root type %q, want %q", str(acc["name"]), b.p.ERPCompany, rt, spec.rootType)
		}
	}
	want := doc{"account_name": base, "company": b.p.ERPCompany, "is_group": 0}
	if acc == nil {
		parent, err := b.findAccount(ctx, spec.parent)
		if err != nil {
			return "", err
		}
		if parent == nil || str(parent["is_group"]) != "1" {
			return "", fmt.Errorf("parent group %q for account %q does not exist in company %q", spec.parent, base, b.p.ERPCompany)
		}
		want["parent_account"] = str(parent["name"])
	}
	var owned []string
	if spec.accountType != "" {
		want["account_type"] = spec.accountType
		owned = append(owned, "account_type")
	}
	name, err := b.ensure(ctx, dtAccount, acc, want, owned)
	if err != nil {
		return "", err
	}
	if wantName := ERPAccount(base, b.p.Abbr); name != wantName {
		return "", fmt.Errorf("account %q of %q is named %q, not %q; ERPAccount's naming rule doesn't hold", base, b.p.ERPCompany, name, wantName)
	}
	return name, nil
}

func (b *boot) accounts(ctx context.Context) error {
	for _, base := range Accounts {
		spec, ok := accountSpecs[base]
		if !ok {
			return fmt.Errorf("account %q has no placement rule", base)
		}
		name, err := b.ensureAccount(ctx, base, spec)
		if err != nil {
			return err
		}
		b.rep.Accounts[base] = name
	}
	if b.p.Bank.Account == "" {
		return errors.New("the profile has no bank.account")
	}
	if _, clash := accountSpecs[b.p.Bank.Account]; clash {
		return fmt.Errorf("bank account %q clashes with a ledger account name", b.p.Bank.Account)
	}
	name, err := b.ensureAccount(ctx, b.p.Bank.Account, bankAccountSpec)
	if err != nil {
		return err
	}
	b.rep.Accounts[b.p.Bank.Account] = name
	b.rep.BankAccount = name
	return nil
}

// gstSettings checks that GST Settings has an Input and an Output row for
// the company whose CGST, SGST and IGST accounts exist, and adds or
// repairs them with India Compliance's default accounts.
func (b *boot) gstSettings(ctx context.Context) error {
	gs, err := b.get(ctx, dtGSTSettings, dtGSTSettings)
	if err != nil {
		return err
	}
	if gs == nil {
		return errors.New("GST Settings not found; is India Compliance installed?")
	}
	raw, _ := gs["gst_accounts"].([]any)
	rows := make([]doc, 0, len(raw)+2)
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	dirty := false
	for _, typ := range []string{gstInput, gstOutput} {
		def := gstDefaults[typ]
		var row doc
		for _, r := range rows {
			if str(r["company"]) == b.p.ERPCompany && str(r["account_type"]) == typ {
				row = r
				break
			}
		}
		if row == nil {
			row = doc{"company": b.p.ERPCompany, "account_type": typ}
			rows = append(rows, row)
			dirty = true
		}
		var got GSTAccounts
		for _, f := range []struct {
			field string
			base  string
			out   *string
		}{
			{"cgst_account", def.CGST, &got.CGST},
			{"sgst_account", def.SGST, &got.SGST},
			{"igst_account", def.IGST, &got.IGST},
		} {
			ok, err := b.companyAccountExists(ctx, str(row[f.field]))
			if err != nil {
				return err
			}
			if !ok {
				name, err := b.ensureAccount(ctx, f.base, def.spec)
				if err != nil {
					return err
				}
				row[f.field] = name
				dirty = true
			}
			*f.out = str(row[f.field])
		}
		if typ == gstInput {
			b.rep.InputGST = got
		} else {
			b.rep.OutputGST = got
		}
	}
	cnt := b.counts(dtGSTSettings)
	if !dirty {
		cnt.Unchanged++
		return nil
	}
	if _, err := frappe.Update(ctx, b.c, dtGSTSettings, dtGSTSettings, doc{"gst_accounts": rows}); err != nil {
		return err
	}
	cnt.Updated++
	cnt.UpdatedNames = append(cnt.UpdatedNames, dtGSTSettings)
	return nil
}

// gstRatesUsed lists the non-zero GST rates p's world uses: sales rates,
// registered suppliers' rates, bank charges and the laptop.
func gstRatesUsed(p Profile) []int {
	set := map[int]bool{}
	set[bankChargeGSTRate] = true
	set[assetRate] = true
	for _, r := range p.Sales.GSTRates {
		set[r] = true
	}
	for _, s := range p.Suppliers {
		if s.IsRegistered() && s.GSTRate != nil {
			set[*s.GSTRate] = true
		}
	}
	delete(set, 0)
	out := make([]int, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Ints(out)
	return out
}

// itemTaxTemplates resolves the company's taxable Item Tax Template for
// each GST rate the world uses. India Compliance creates them ("GST 18% -
// STPL") when it sets up an Indian company; Bootstrap only checks them.
func (b *boot) itemTaxTemplates(ctx context.Context) error {
	rows, err := b.list(ctx, dtItemTaxTemplate, [][]any{
		{"company", "=", b.p.ERPCompany},
		{"gst_treatment", "=", "Taxable"},
		{"disabled", "=", 0},
	}, "gst_rate")
	if err != nil {
		return err
	}
	byRate := map[string]string{}
	for _, r := range rows {
		rate, ok := wholeNumber(str(r["gst_rate"]))
		if !ok {
			continue
		}
		if _, dup := byRate[rate]; !dup {
			byRate[rate] = str(r["name"]) // rows are in name order
		}
	}
	var missing []string
	for _, rate := range gstRatesUsed(b.p) {
		key := strconv.Itoa(rate)
		name, ok := byRate[key]
		if !ok {
			missing = append(missing, key+"%")
			continue
		}
		b.rep.ItemTaxTemplates[key] = name
	}
	if len(missing) > 0 {
		return fmt.Errorf("company %q has no taxable Item Tax Template for GST %s", b.p.ERPCompany, strings.Join(missing, ", "))
	}
	return nil
}

// wholeNumber returns the integer text of a decimal such as "18.0" or
// "18", or false if it has a non-zero fraction.
func wholeNumber(s string) (string, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || strings.Trim(frac, "0") != "" {
		return "", false
	}
	if _, err := strconv.Atoi(whole); err != nil {
		return "", false
	}
	return whole, true
}

// companyAccountExists reports whether name is a ledger account of the
// company.
func (b *boot) companyAccountExists(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	acc, err := b.get(ctx, dtAccount, name)
	if err != nil || acc == nil {
		return false, err
	}
	return str(acc["company"]) == b.p.ERPCompany && str(acc["is_group"]) == "0", nil
}

// customFields ensures the external-ID field on each DocType and checks
// the stored fieldname.
func (b *boot) customFields(ctx context.Context) error {
	for _, dt := range extIDDocTypes {
		rows, err := b.list(ctx, dtCustomField, [][]any{{"dt", "=", dt}},
			"dt", "fieldname", "label", "fieldtype", "no_copy", "read_only", "search_index")
		if err != nil {
			return err
		}
		var existing doc
		for _, r := range rows {
			fn := str(r["fieldname"])
			if fn == ExtIDField || fn == "custom_"+ExtIDField || str(r["label"]) == extIDLabel {
				if fn != ExtIDField {
					return fmt.Errorf("%s has the external-ID custom field stored as %q, want %q", dt, fn, ExtIDField)
				}
				existing = r
				break
			}
		}
		want := doc{
			"dt":           dt,
			"fieldname":    ExtIDField,
			"label":        extIDLabel,
			"fieldtype":    "Data",
			"no_copy":      1,
			"read_only":    1,
			"search_index": 1,
		}
		name, err := b.ensure(ctx, dtCustomField, existing, want,
			[]string{"label", "fieldtype", "no_copy", "read_only", "search_index"})
		if err != nil {
			return err
		}
		stored, err := b.get(ctx, dtCustomField, name)
		if err != nil {
			return err
		}
		if stored == nil {
			return fmt.Errorf("custom field %q on %s vanished after saving", name, dt)
		}
		if fn := str(stored["fieldname"]); fn != ExtIDField {
			return fmt.Errorf("the external-ID field on %s was stored as %q, want %q", dt, fn, ExtIDField)
		}
	}
	b.rep.ExtIDField = ExtIDField
	return nil
}

// pickGroup returns a non-group record of a tree DocType (Supplier Group,
// Customer Group, Item Group, Territory): the first of preferred that
// exists, otherwise the first by name.
func (b *boot) pickGroup(ctx context.Context, doctype string, preferred ...string) (string, error) {
	key := doctype + "|" + strings.Join(preferred, "|")
	if g, ok := b.groups[key]; ok {
		return g, nil
	}
	rows, err := b.list(ctx, doctype, [][]any{{"is_group", "=", 0}})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no %s exists", doctype)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = str(r["name"])
	}
	pick := names[0]
	for _, p := range preferred {
		if slices.Contains(names, p) {
			pick = p
			break
		}
	}
	b.groups[key] = pick
	return pick, nil
}

func (b *boot) suppliers(ctx context.Context) error {
	if len(b.p.Suppliers) == 0 {
		return nil
	}
	group, err := b.pickGroup(ctx, dtSupplierGroup, "Local", "Services")
	if err != nil {
		return err
	}
	names := make([]string, len(b.p.Suppliers))
	for i, s := range b.p.Suppliers {
		names[i] = s.Name
	}
	rows, err := b.list(ctx, dtSupplier, [][]any{{"supplier_name", "in", names}},
		"supplier_name", "supplier_group", "supplier_type", "country", "gstin", "gst_category", "pan")
	if err != nil {
		return err
	}
	byName := map[string]doc{}
	for _, r := range rows {
		n := str(r["supplier_name"])
		if _, dup := byName[n]; dup {
			return fmt.Errorf("more than one supplier is named %q", n)
		}
		byName[n] = r
	}
	for _, s := range b.p.Suppliers {
		want := doc{
			"supplier_name":  s.Name,
			"supplier_group": group,
			"supplier_type":  "Company",
			"country":        countryIndia,
		}
		owned := []string{"country", "gst_category", "gstin"}
		gstin := ""
		if s.IsRegistered() {
			g, err := b.p.SupplierGSTIN(s)
			if err != nil {
				return err
			}
			gstin = g
			want["gstin"] = gstin
			want["gst_category"] = gstRegisteredRegular
			want["pan"] = b.p.SupplierPAN(s)
			owned = append(owned, "pan")
		} else {
			want["gstin"] = ""
			want["gst_category"] = gstUnregistered
		}
		name, err := b.ensure(ctx, dtSupplier, byName[s.Name], want, owned)
		if err != nil {
			return fmt.Errorf("supplier %s: %w", s.ID, err)
		}
		if name != s.Name {
			return fmt.Errorf("supplier %s was named %q, want %q (the generator's party name); check Buying Settings > Supplier Naming By", s.ID, name, s.Name)
		}
		b.rep.Suppliers[s.ID] = name
		if !s.IsRegistered() {
			continue
		}
		addr, err := b.address(ctx, s.Name, dtSupplier, name, s.StateCode, gstin, false)
		if err != nil {
			return fmt.Errorf("supplier %s address: %w", s.ID, err)
		}
		b.rep.SupplierAddresses[s.ID] = addr
	}
	return nil
}

// CustomerName is the n-th customer's name (1-based): CUST-001.
func CustomerName(n int) string { return fmt.Sprintf("CUST-%03d", n) }

func (b *boot) customers(ctx context.Context) error {
	if b.p.Customers <= 0 {
		return nil
	}
	group, err := b.pickGroup(ctx, dtCustomerGroup, "Commercial")
	if err != nil {
		return err
	}
	territory, err := b.pickGroup(ctx, dtTerritory, countryIndia)
	if err != nil {
		return err
	}
	names := make([]string, b.p.Customers)
	for i := range names {
		names[i] = CustomerName(i + 1)
	}
	rows, err := b.list(ctx, dtCustomer, [][]any{{"customer_name", "in", names}},
		"customer_name", "customer_type", "customer_group", "territory", "gst_category")
	if err != nil {
		return err
	}
	byName := map[string]doc{}
	for _, r := range rows {
		n := str(r["customer_name"])
		if _, dup := byName[n]; dup {
			return fmt.Errorf("more than one customer is named %q", n)
		}
		byName[n] = r
	}
	for _, n := range names {
		want := doc{
			"customer_name":  n,
			"customer_type":  "Company",
			"customer_group": group,
			"territory":      territory,
			"gst_category":   gstUnregistered,
		}
		name, err := b.ensure(ctx, dtCustomer, byName[n], want, []string{"customer_type", "gst_category"})
		if err != nil {
			return fmt.Errorf("customer %s: %w", n, err)
		}
		if name != n {
			return fmt.Errorf("customer %s was named %q; the generator's party is the customer name (check Selling Settings > Customer Naming By)", n, name)
		}
		b.rep.Customers = append(b.rep.Customers, name)
	}
	return nil
}

func (b *boot) items(ctx context.Context) error {
	codes := make([]string, len(itemSpecs))
	for i, it := range itemSpecs {
		codes[i] = it.Code
	}
	rows, err := b.list(ctx, dtItem, [][]any{{"item_code", "in", codes}},
		"item_code", "item_name", "item_group", "is_stock_item", "stock_uom", "gst_hsn_code", "is_sales_item", "is_purchase_item", "description")
	if err != nil {
		return err
	}
	byCode := map[string]doc{}
	for _, r := range rows {
		byCode[str(r["item_code"])] = r
	}
	checked := map[string]bool{}
	for _, it := range itemSpecs {
		if !checked[it.HSN] {
			h, err := b.get(ctx, dtGSTHSNCode, it.HSN)
			if err != nil {
				return err
			}
			if h == nil {
				return fmt.Errorf("item %q: GST HSN Code %q does not exist", it.Code, it.HSN)
			}
			checked[it.HSN] = true
		}
		group, err := b.pickGroup(ctx, dtItemGroup, it.Group)
		if err != nil {
			return err
		}
		sales := 0
		if it.Sales {
			sales = 1
		}
		want := doc{
			"item_code":        it.Code,
			"item_name":        it.Code,
			"item_group":       group,
			"description":      it.Description,
			"is_stock_item":    0,
			"stock_uom":        uomNos,
			"gst_hsn_code":     it.HSN,
			"is_sales_item":    sales,
			"is_purchase_item": 1 - sales,
		}
		owned := []string{"item_name", "description", "is_stock_item", "gst_hsn_code", "is_sales_item", "is_purchase_item"}
		name, err := b.ensure(ctx, dtItem, byCode[it.Code], want, owned)
		if err != nil {
			return fmt.Errorf("item %q: %w", it.Code, err)
		}
		if name != it.Code {
			return fmt.Errorf("item %q was named %q; check Stock Settings > Item Naming By", it.Code, name)
		}
		b.rep.Items = append(b.rep.Items, name)
	}
	return nil
}
