package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// BankLine represents a single transaction line from a company bank statement.
type BankLine struct {
	CompanyID    string       `json:"company_id"`
	TxnID        string       `json:"txn_id"`
	TxnDate      time.Time    `json:"txn_date"`
	ValueDate    *time.Time   `json:"value_date,omitempty"`
	Narration    string       `json:"narration"`
	Ref          *string      `json:"ref,omitempty"`
	AmountPaise  money.Paise  `json:"amount_paise"`
	BalancePaise *money.Paise `json:"balance_paise,omitempty"`
	SourceFile   string       `json:"source_file"`
}

// BankLineFilter specifies criteria for filtering bank statement lines.
type BankLineFilter struct {
	CompanyID string
	FromDate  *time.Time
	ToDate    *time.Time
	MinAmount *money.Paise // minimum absolute amount
}

// GSTR2BEntry represents one invoice line from an inward GST return (GSTR-2B).
type GSTR2BEntry struct {
	CompanyID     string      `json:"company_id"`
	Period        string      `json:"period"` // YYYY-MM
	SupplierGSTIN string      `json:"supplier_gstin"`
	SupplierName  *string     `json:"supplier_name,omitempty"`
	InvoiceNo     string      `json:"invoice_no"`
	InvoiceNoNorm string      `json:"invoice_no_norm"`
	InvoiceDate   time.Time   `json:"invoice_date"`
	TaxablePaise  money.Paise `json:"taxable_paise"`
	IGSTPaise     money.Paise `json:"igst_paise"`
	CGSTPaise     money.Paise `json:"cgst_paise"`
	SGSTPaise     money.Paise `json:"sgst_paise"`
	ITCAvailable  bool        `json:"itc_available"`
}

// UpsertBankLines inserts or updates bank statement lines in a single transaction.
func (s *Store) UpsertBankLines(ctx context.Context, lines []BankLine) error {
	if len(lines) == 0 {
		return nil
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		const query = `
			INSERT INTO bank_lines (
				company_id, txn_id, txn_date, value_date,
				narration, ref, amount_paise, balance_paise, source_file
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (company_id, txn_id) DO UPDATE SET
				txn_date = EXCLUDED.txn_date,
				value_date = EXCLUDED.value_date,
				narration = EXCLUDED.narration,
				ref = EXCLUDED.ref,
				amount_paise = EXCLUDED.amount_paise,
				balance_paise = EXCLUDED.balance_paise,
				source_file = EXCLUDED.source_file;
		`
		for _, line := range lines {
			var bal *int64
			if line.BalancePaise != nil {
				v := int64(*line.BalancePaise)
				bal = &v
			}
			_, err := tx.Exec(ctx, query,
				line.CompanyID,
				line.TxnID,
				line.TxnDate,
				line.ValueDate,
				line.Narration,
				line.Ref,
				int64(line.AmountPaise),
				bal,
				line.SourceFile,
			)
			if err != nil {
				return fmt.Errorf("store: upsert bank line %s/%s: %w", line.CompanyID, line.TxnID, err)
			}
		}
		return nil
	})
}

// GetBankLine retrieves a single bank line by company ID and transaction ID.
func (s *Store) GetBankLine(ctx context.Context, companyID, txnID string) (BankLine, error) {
	const query = `
		SELECT company_id, txn_id, txn_date, value_date,
		       narration, ref, amount_paise, balance_paise, source_file
		FROM bank_lines
		WHERE company_id = $1 AND txn_id = $2;
	`
	var line BankLine
	var amt int64
	var bal *int64
	err := s.pool.QueryRow(ctx, query, companyID, txnID).Scan(
		&line.CompanyID,
		&line.TxnID,
		&line.TxnDate,
		&line.ValueDate,
		&line.Narration,
		&line.Ref,
		&amt,
		&bal,
		&line.SourceFile,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BankLine{}, fmt.Errorf("%w: bank line %s/%s", ErrNotFound, companyID, txnID)
		}
		return BankLine{}, fmt.Errorf("store: get bank line: %w", err)
	}
	line.AmountPaise = money.Paise(amt)
	if bal != nil {
		p := money.Paise(*bal)
		line.BalancePaise = &p
	}
	return line, nil
}

// ListBankLines retrieves bank lines matching the given filter, ordered by date and txn_id.
func (s *Store) ListBankLines(ctx context.Context, filter BankLineFilter) ([]BankLine, error) {
	query := `
		SELECT company_id, txn_id, txn_date, value_date,
		       narration, ref, amount_paise, balance_paise, source_file
		FROM bank_lines
		WHERE company_id = $1
	`
	args := []any{filter.CompanyID}
	idx := 2

	if filter.FromDate != nil {
		query += fmt.Sprintf(" AND txn_date >= $%d", idx)
		args = append(args, *filter.FromDate)
		idx++
	}
	if filter.ToDate != nil {
		query += fmt.Sprintf(" AND txn_date <= $%d", idx)
		args = append(args, *filter.ToDate)
		idx++
	}
	if filter.MinAmount != nil {
		query += fmt.Sprintf(" AND abs(amount_paise) >= $%d", idx)
		args = append(args, int64(*filter.MinAmount))
	}

	query += " ORDER BY txn_date ASC, txn_id ASC;"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list bank lines: %w", err)
	}
	defer rows.Close()

	var out []BankLine
	for rows.Next() {
		var line BankLine
		var amt int64
		var bal *int64
		if err := rows.Scan(
			&line.CompanyID,
			&line.TxnID,
			&line.TxnDate,
			&line.ValueDate,
			&line.Narration,
			&line.Ref,
			&amt,
			&bal,
			&line.SourceFile,
		); err != nil {
			return nil, fmt.Errorf("store: scan bank line: %w", err)
		}
		line.AmountPaise = money.Paise(amt)
		if bal != nil {
			p := money.Paise(*bal)
			line.BalancePaise = &p
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate bank lines: %w", err)
	}
	return out, nil
}

// UpsertGSTR2BEntries inserts or updates GSTR-2B entries in a single transaction.
func (s *Store) UpsertGSTR2BEntries(ctx context.Context, entries []GSTR2BEntry) error {
	if len(entries) == 0 {
		return nil
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		const query = `
			INSERT INTO gstr2b_entries (
				company_id, period, supplier_gstin, supplier_name,
				invoice_no, invoice_no_norm, invoice_date,
				taxable_paise, igst_paise, cgst_paise, sgst_paise, itc_available
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (company_id, period, supplier_gstin, invoice_no_norm) DO UPDATE SET
				supplier_name = EXCLUDED.supplier_name,
				invoice_no = EXCLUDED.invoice_no,
				invoice_date = EXCLUDED.invoice_date,
				taxable_paise = EXCLUDED.taxable_paise,
				igst_paise = EXCLUDED.igst_paise,
				cgst_paise = EXCLUDED.cgst_paise,
				sgst_paise = EXCLUDED.sgst_paise,
				itc_available = EXCLUDED.itc_available;
		`
		for _, e := range entries {
			_, err := tx.Exec(ctx, query,
				e.CompanyID,
				e.Period,
				e.SupplierGSTIN,
				e.SupplierName,
				e.InvoiceNo,
				e.InvoiceNoNorm,
				e.InvoiceDate,
				int64(e.TaxablePaise),
				int64(e.IGSTPaise),
				int64(e.CGSTPaise),
				int64(e.SGSTPaise),
				e.ITCAvailable,
			)
			if err != nil {
				return fmt.Errorf("store: upsert gstr2b entry %s/%s/%s: %w", e.CompanyID, e.Period, e.InvoiceNoNorm, err)
			}
		}
		return nil
	})
}

// GetGSTR2BEntry retrieves a single GSTR-2B entry by primary key.
func (s *Store) GetGSTR2BEntry(ctx context.Context, companyID, period, supplierGSTIN, invoiceNoNorm string) (GSTR2BEntry, error) {
	const query = `
		SELECT company_id, period, supplier_gstin, supplier_name,
		       invoice_no, invoice_no_norm, invoice_date,
		       taxable_paise, igst_paise, cgst_paise, sgst_paise, itc_available
		FROM gstr2b_entries
		WHERE company_id = $1 AND period = $2 AND supplier_gstin = $3 AND invoice_no_norm = $4;
	`
	var e GSTR2BEntry
	var taxable, igst, cgst, sgst int64
	err := s.pool.QueryRow(ctx, query, companyID, period, supplierGSTIN, invoiceNoNorm).Scan(
		&e.CompanyID,
		&e.Period,
		&e.SupplierGSTIN,
		&e.SupplierName,
		&e.InvoiceNo,
		&e.InvoiceNoNorm,
		&e.InvoiceDate,
		&taxable,
		&igst,
		&cgst,
		&sgst,
		&e.ITCAvailable,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GSTR2BEntry{}, fmt.Errorf("%w: gstr2b entry %s/%s/%s", ErrNotFound, companyID, period, invoiceNoNorm)
		}
		return GSTR2BEntry{}, fmt.Errorf("store: get gstr2b entry: %w", err)
	}
	e.TaxablePaise = money.Paise(taxable)
	e.IGSTPaise = money.Paise(igst)
	e.CGSTPaise = money.Paise(cgst)
	e.SGSTPaise = money.Paise(sgst)
	return e, nil
}

// ListGSTR2BEntries retrieves all GSTR-2B entries for a given company and period.
func (s *Store) ListGSTR2BEntries(ctx context.Context, companyID, period string) ([]GSTR2BEntry, error) {
	const query = `
		SELECT company_id, period, supplier_gstin, supplier_name,
		       invoice_no, invoice_no_norm, invoice_date,
		       taxable_paise, igst_paise, cgst_paise, sgst_paise, itc_available
		FROM gstr2b_entries
		WHERE company_id = $1 AND period = $2
		ORDER BY invoice_date ASC, supplier_gstin ASC, invoice_no_norm ASC;
	`
	rows, err := s.pool.Query(ctx, query, companyID, period)
	if err != nil {
		return nil, fmt.Errorf("store: list gstr2b entries: %w", err)
	}
	defer rows.Close()

	var out []GSTR2BEntry
	for rows.Next() {
		var e GSTR2BEntry
		var taxable, igst, cgst, sgst int64
		if err := rows.Scan(
			&e.CompanyID,
			&e.Period,
			&e.SupplierGSTIN,
			&e.SupplierName,
			&e.InvoiceNo,
			&e.InvoiceNoNorm,
			&e.InvoiceDate,
			&taxable,
			&igst,
			&cgst,
			&sgst,
			&e.ITCAvailable,
		); err != nil {
			return nil, fmt.Errorf("store: scan gstr2b entry: %w", err)
		}
		e.TaxablePaise = money.Paise(taxable)
		e.IGSTPaise = money.Paise(igst)
		e.CGSTPaise = money.Paise(cgst)
		e.SGSTPaise = money.Paise(sgst)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate gstr2b entries: %w", err)
	}
	return out, nil
}

// monthBounds returns the first day of month (YYYY-MM) and of the next month, at UTC midnight.
func monthBounds(month string) (time.Time, time.Time, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("store: invalid month %q (want YYYY-MM): %w", month, err)
	}
	return start.UTC(), start.UTC().AddDate(0, 1, 0), nil
}

// ReplaceBankLines makes the company's bank lines for month (YYYY-MM) exactly
// lines: in one transaction it deletes that company's lines dated in the
// month, then inserts lines. A line of another company or dated outside the
// month is an error before anything is deleted. An empty list clears the month.
func (s *Store) ReplaceBankLines(ctx context.Context, companyID, month string, lines []BankLine) error {
	start, end, err := monthBounds(month)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if l.CompanyID != companyID {
			return fmt.Errorf("store: replace bank lines: line %s belongs to company %q, not %q", l.TxnID, l.CompanyID, companyID)
		}
		if l.TxnDate.Before(start) || !l.TxnDate.Before(end) {
			return fmt.Errorf("store: replace bank lines %s/%s: line %s is dated %s, outside the month",
				companyID, month, l.TxnID, l.TxnDate.Format("2006-01-02"))
		}
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM bank_lines WHERE company_id = $1 AND txn_date >= $2 AND txn_date < $3`,
			companyID, start, end); err != nil {
			return fmt.Errorf("store: clear bank lines %s/%s: %w", companyID, month, err)
		}
		const query = `
			INSERT INTO bank_lines (
				company_id, txn_id, txn_date, value_date,
				narration, ref, amount_paise, balance_paise, source_file
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
		`
		for _, line := range lines {
			var bal *int64
			if line.BalancePaise != nil {
				v := int64(*line.BalancePaise)
				bal = &v
			}
			if _, err := tx.Exec(ctx, query,
				line.CompanyID, line.TxnID, line.TxnDate, line.ValueDate,
				line.Narration, line.Ref, int64(line.AmountPaise), bal, line.SourceFile,
			); err != nil {
				return fmt.Errorf("store: insert bank line %s/%s: %w", line.CompanyID, line.TxnID, err)
			}
		}
		return nil
	})
}

// ReplaceGSTR2BEntries makes the company's GSTR-2B entries for period
// (YYYY-MM) exactly entries: in one transaction it deletes that company's
// entries of the period, then inserts entries. An entry of another company or
// period is an error before anything is deleted. An empty list clears the period.
func (s *Store) ReplaceGSTR2BEntries(ctx context.Context, companyID, period string, entries []GSTR2BEntry) error {
	if _, _, err := monthBounds(period); err != nil {
		return err
	}
	for _, e := range entries {
		if e.CompanyID != companyID || e.Period != period {
			return fmt.Errorf("store: replace gstr2b entries: entry %s is for %s/%s, not %s/%s",
				e.InvoiceNoNorm, e.CompanyID, e.Period, companyID, period)
		}
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM gstr2b_entries WHERE company_id = $1 AND period = $2`,
			companyID, period); err != nil {
			return fmt.Errorf("store: clear gstr2b entries %s/%s: %w", companyID, period, err)
		}
		const query = `
			INSERT INTO gstr2b_entries (
				company_id, period, supplier_gstin, supplier_name,
				invoice_no, invoice_no_norm, invoice_date,
				taxable_paise, igst_paise, cgst_paise, sgst_paise, itc_available
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
		`
		for _, e := range entries {
			if _, err := tx.Exec(ctx, query,
				e.CompanyID, e.Period, e.SupplierGSTIN, e.SupplierName,
				e.InvoiceNo, e.InvoiceNoNorm, e.InvoiceDate,
				int64(e.TaxablePaise), int64(e.IGSTPaise), int64(e.CGSTPaise), int64(e.SGSTPaise), e.ITCAvailable,
			); err != nil {
				return fmt.Errorf("store: insert gstr2b entry %s/%s/%s: %w", e.CompanyID, e.Period, e.InvoiceNoNorm, err)
			}
		}
		return nil
	})
}
