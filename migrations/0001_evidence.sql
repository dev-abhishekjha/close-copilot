-- +goose Up
CREATE TABLE companies (
    id text PRIMARY KEY,
    erp_company text NOT NULL,
    gstin text NOT NULL
);

CREATE TABLE bank_lines (
    company_id text NOT NULL REFERENCES companies(id),
    txn_id text NOT NULL,
    txn_date date NOT NULL,
    value_date date,
    narration text NOT NULL,
    ref text,
    amount_paise bigint NOT NULL,
    balance_paise bigint,
    source_file text NOT NULL,
    PRIMARY KEY (company_id, txn_id)
);

CREATE TABLE gstr2b_entries (
    company_id text NOT NULL REFERENCES companies(id),
    period text NOT NULL,
    supplier_gstin text NOT NULL,
    supplier_name text,
    invoice_no text NOT NULL,
    invoice_no_norm text NOT NULL,
    invoice_date date NOT NULL,
    taxable_paise bigint NOT NULL,
    igst_paise bigint NOT NULL DEFAULT 0,
    cgst_paise bigint NOT NULL DEFAULT 0,
    sgst_paise bigint NOT NULL DEFAULT 0,
    itc_available boolean NOT NULL,
    PRIMARY KEY (company_id, period, supplier_gstin, invoice_no_norm)
);

-- +goose Down
DROP TABLE IF EXISTS gstr2b_entries;
DROP TABLE IF EXISTS bank_lines;
DROP TABLE IF EXISTS companies;
