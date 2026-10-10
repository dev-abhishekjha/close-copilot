# ERPNext schema dumps

One JSON file per DocType that Close Copilot reads or writes. Later tickets code against these field names instead of guessing them (CC-201).

## How they were made

```sh
deploy/erpnext/api-users.sh        # creates the bot user and both key pairs in tmp/erp-keys.env
(set -a; . tmp/erp-keys.env; set +a; go run ./cmd/probe schema --out docs/erpnext-schema)
```

The site was `erp.localhost` from CC-102, running frappe 15.122.0, erpnext 15.122.0 and india_compliance 15.32.0 (`bench version`).

For each DocType, `probe schema` reads two endpoints:

- `GET /api/resource/DocType/<name>` for the standard fields (DocField rows);
- `GET /api/resource/Custom Field?filters=[["dt","=",<name>]]` for the custom fields, paged 500 at a time. Most of these come from India Compliance, such as `supplier_gstin`.

It keeps only stable keys: `name`, `module`, `istable` and `is_submittable`, and for each field `fieldname`, `fieldtype`, `label`, `options`, `reqd`, `read_only`, `hidden` and `custom` (`true` for a Custom Field). Fields are sorted by `fieldname`, and files use a 2-space indent and a trailing newline. Two runs give identical bytes, so `diff -r -x README.md <new dir> docs/erpnext-schema` shows any drift after an upgrade.

**`schema` uses the seeder key.** The bot `copilot-bot@example.com` holds only Accounts User, and Frappe refuses it on DocType and Custom Field meta (HTTP 403 `PermissionError`). So `probe schema` switches to the Administrator seeder key (`ERP_SEED_API_KEY`, `ERP_SEED_API_SECRET`) for this subcommand only, and logs that it did. `probe auth` and `probe perms` always use the bot key.

Caveats:

- These are the raw DocType definitions. Property Setters, which change things like `hidden` or `options` per site, are not applied. `frappe.get_meta` at run time can differ.
- A `reqd` of 0 doesn't mean optional in practice. Python controllers and India Compliance hooks validate more than the DocType says.
- The CC-303 external-ID custom field is not here yet, because it is created by the seeder. Regenerate the dumps after CC-303 lands.

## Credentials (tmp/erp-keys.env)

`deploy/erpnext/api-users.sh` writes `tmp/erp-keys.env` with mode 600. It holds `ERP_BASE_URL`, `ERP_SITE`, `ERP_API_KEY`/`ERP_API_SECRET` (the bot) and `ERP_SEED_API_KEY`/`ERP_SEED_API_SECRET` (Administrator, local development only). Copy only the bot lines (`ERP_BASE_URL`, `ERP_SITE`, `ERP_API_KEY`, `ERP_API_SECRET`) into your `.env`; the seed keys stay in `tmp/erp-keys.env`, and commands that need them load it in a subshell (`(set -a; . tmp/erp-keys.env; set +a; <command>)`). Never commit or print them. `deploy/erpnext/api-users.sh --rotate` issues new pairs and rewrites the file; copy the bot lines again afterwards.

## Fields the project relies on

`[]` marks a child-table row, for example `taxes[].account_head`. Amounts (Currency fields) are decoded as `json.Number` and converted to paise by `internal/money` (CC-203). "Custom" means the field comes from India Compliance.

| DocType | Field | Type | Used by |
| --- | --- | --- | --- |
| Company | `abbr`, `country`, `default_currency` | Data, Link, Link | CC-303 (second company), CC-204 (account names end in ` - <abbr>`) |
| Company | `gstin`, `gst_category`, `pan` | custom | CC-303 |
| Account | `account_name`, `company`, `parent_account`, `is_group`, `account_type`, `account_currency` | | CC-303 (create accounts) |
| Account | `root_type` | Select, read-only | CC-204 (opening resets at fiscal-year start for Income and Expense) |
| Address | `address_title`, `address_type`, `address_line1`, `city`, `state`, `pincode`, `country`, `links[]` | | CC-303 (company and supplier addresses) |
| Address | `gstin`, `gst_state`, `gst_category`, `is_your_company_address` | custom | CC-303 |
| Supplier | `supplier_name`, `supplier_group`, `supplier_type` | | CC-303 |
| Supplier | `gstin`, `gst_category`, `pan` | custom | CC-303 |
| Customer | `customer_name`, `customer_group`, `customer_type`, `territory` | | CC-303 |
| Customer | `gstin`, `gst_category` | custom | CC-303 |
| Item | `item_code`, `item_name`, `item_group`, `is_stock_item`, `stock_uom` | | CC-303 (non-stock items) |
| Item | `gst_hsn_code` | custom | CC-303 |
| Purchase Invoice | `supplier`, `supplier_name`, `bill_no`, `bill_date`, `posting_date`, `company`, `remarks`, `credit_to` | | CC-304 (write), CC-502 `list_purchase_invoices`, CC-604 (`bill_date` in month), CC-605 |
| Purchase Invoice | `net_total`, `grand_total`, `outstanding_amount`, `is_return` | Currency, Check | CC-502 (taxable), CC-603 (fully paid) |
| Purchase Invoice | `supplier_gstin`, `company_gstin`, `place_of_supply` | custom | CC-502, CC-604 (key with `bill_no`) |
| Purchase Invoice | `items[]`, `taxes[]` | Table | CC-304, CC-502 (fetched per invoice with `Get`, CC-202) |
| Purchase Invoice Item (not dumped) | `items[].item_code`, `items[].description`, `items[].expense_account`, `items[].amount` | | CC-304, CC-502 `lines (account, description, amount)`. Checked live but not in the CC-201 list; add a dump if CC-502 needs more |
| Purchase Taxes and Charges | `taxes[].account_head`, `taxes[].tax_amount`, `taxes[].charge_type`, `taxes[].rate`, `taxes[].add_deduct_tax`, `taxes[].category`, `taxes[].description` | | CC-304 (explicit GST rows), CC-502 (igst/cgst/sgst), CC-604 (via GST Settings account heads) |
| Purchase Taxes and Charges | `taxes[].gst_tax_type` | custom, read-only | CC-604 (cross-check of the account-head mapping) |
| Sales Invoice | `customer`, `customer_name`, `posting_date`, `company`, `net_total`, `grand_total`, `outstanding_amount`, `debit_to`, `remarks`, `items[]`, `taxes[]` | | CC-304 (write), CC-502 `list_sales_invoices` |
| Payment Entry | `payment_type`, `party_type`, `party`, `party_name`, `paid_amount`, `received_amount`, `paid_from`, `paid_to`, `reference_no`, `reference_date`, `posting_date`, `company`, `remarks`, `unallocated_amount`, `references[]` | | CC-304 (write), CC-502 `list_payments`, CC-602 (pass 1 on `reference_no`), CC-603 (unallocated advances, remarks) |
| Payment Entry Reference | `references[].reference_doctype`, `references[].reference_name`, `references[].allocated_amount`, `references[].total_amount`, `references[].outstanding_amount` | | CC-304, CC-502 (`invoices`), CC-603 |
| Journal Entry | `voucher_type`, `posting_date`, `company`, `user_remark`, `cheque_no`, `cheque_date`, `total_debit`, `total_credit`, `accounts[]` | | CC-304 (write), CC-504b (post approved entry), CC-602 (pass 1 on `cheque_no`), CC-605 |
| Journal Entry Account | `accounts[].account`, `accounts[].debit_in_account_currency`, `accounts[].credit_in_account_currency`, `accounts[].party_type`, `accounts[].party`, `accounts[].reference_type`, `accounts[].reference_name`, `accounts[].cost_center`, `accounts[].user_remark` | | CC-304, CC-504b (lines from the approved proposal) |
| Journal Entry Account | `accounts[].debit`, `accounts[].credit` | Currency, read-only | CC-502 (company-currency amounts; ERPNext computes them on save) |
| GL Entry | `account`, `debit`, `credit`, `posting_date`, `voucher_type`, `voucher_no`, `party_type`, `party`, `company` | | CC-204 (trial balance, account history), CC-502 `list_gl_entries`, CC-602 (bank account entries grouped by voucher) |
| GL Entry | `is_cancelled`, `is_opening`, `fiscal_year`, `against`, `remarks` | | CC-204 (skip `is_cancelled = 1`), CC-502 |

`docstatus` and `name` are standard columns on every DocType and do not appear as fields. Filter submitted documents with `["docstatus","=",1]`.
