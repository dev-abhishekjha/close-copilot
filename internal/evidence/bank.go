package evidence

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// BankCSVHeader is the required bank statement CSV header row from the shared specs.
var BankCSVHeader = []string{"txn_id", "date", "value_date", "narration", "ref", "withdrawal", "deposit", "balance"}

// BankLoadResult summarizes the outcome of loading a bank statement file.
type BankLoadResult struct {
	Company    string `json:"company"`
	Month      string `json:"month"`
	SourceFile string `json:"source_file"`
	Total      int    `json:"total"`
	Inserted   int    `json:"inserted"`
	Updated    int    `json:"updated"`
	Rejected   int    `json:"rejected"`
	Warning    string `json:"warning,omitempty"`
}

// ParseBankCSV reads a bank statement CSV and validates its header and data rows.
// Any malformed row is rejected with its line number.
func ParseBankCSV(r io.Reader, companyID, sourceFile string) ([]store.BankLine, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // validate length explicitly per row

	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("bank csv: file is empty")
		}
		return nil, fmt.Errorf("bank csv: read header: %w", err)
	}

	if len(header) != len(BankCSVHeader) || !slices.Equal(header, BankCSVHeader) {
		return nil, fmt.Errorf("bank csv: invalid header %v, want %v", header, BankCSVHeader)
	}

	var lines []store.BankLine
	var rowErrs []error
	seqPerDate := make(map[string]int)

	lineNum := 1 // header was line 1
	for {
		lineNum++
		rec, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			rowErrs = append(rowErrs, fmt.Errorf("line %d: %w", lineNum, err))
			continue
		}

		if len(rec) != len(BankCSVHeader) {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: expected %d columns, got %d", lineNum, len(BankCSVHeader), len(rec)))
			continue
		}

		txnID := strings.TrimSpace(rec[0])
		dateStr := strings.TrimSpace(rec[1])
		valDateStr := strings.TrimSpace(rec[2])
		narration := strings.TrimSpace(rec[3])
		refStr := strings.TrimSpace(rec[4])
		wdStr := strings.TrimSpace(rec[5])
		depStr := strings.TrimSpace(rec[6])
		balStr := strings.TrimSpace(rec[7])

		txnDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: invalid date %q (want YYYY-MM-DD): %w", lineNum, dateStr, err))
			continue
		}

		var valDate *time.Time
		if valDateStr != "" {
			v, err := time.Parse("2006-01-02", valDateStr)
			if err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("line %d: invalid value_date %q: %w", lineNum, valDateStr, err))
				continue
			}
			valDate = &v
		}

		if narration == "" {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: narration is empty", lineNum))
			continue
		}

		var ref *string
		if refStr != "" {
			ref = &refStr
		}

		// Exactly one of withdrawal and deposit must be set
		hasWd := wdStr != ""
		hasDep := depStr != ""
		if (!hasWd && !hasDep) || (hasWd && hasDep) {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: exactly one of withdrawal and deposit must be set (got withdrawal=%q, deposit=%q)", lineNum, wdStr, depStr))
			continue
		}

		var amtPaise money.Paise
		if hasWd {
			wd, err := money.ParseRupees(wdStr)
			if err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("line %d: parse withdrawal %q: %w", lineNum, wdStr, err))
				continue
			}
			if wd <= 0 {
				rowErrs = append(rowErrs, fmt.Errorf("line %d: withdrawal amount %s must be positive", lineNum, wd.Rupees()))
				continue
			}
			amtPaise = -wd // withdrawals are stored as negative paise
		} else {
			dep, err := money.ParseRupees(depStr)
			if err != nil {
				rowErrs = append(rowErrs, fmt.Errorf("line %d: parse deposit %q: %w", lineNum, depStr, err))
				continue
			}
			if dep <= 0 {
				rowErrs = append(rowErrs, fmt.Errorf("line %d: deposit amount %s must be positive", lineNum, dep.Rupees()))
				continue
			}
			amtPaise = dep
		}

		if balStr == "" {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: balance is required", lineNum))
			continue
		}
		bal, err := money.ParseRupees(balStr)
		if err != nil {
			rowErrs = append(rowErrs, fmt.Errorf("line %d: parse balance %q: %w", lineNum, balStr, err))
			continue
		}

		// Derive txn_id if empty: TXN-<YYYYMMDD>-<seq>
		if txnID == "" {
			dateKey := strings.ReplaceAll(dateStr, "-", "")
			seqPerDate[dateKey]++
			txnID = fmt.Sprintf("TXN-%s-%03d", dateKey, seqPerDate[dateKey])
		}

		lines = append(lines, store.BankLine{
			CompanyID:    companyID,
			TxnID:        txnID,
			TxnDate:      txnDate,
			ValueDate:    valDate,
			Narration:    narration,
			Ref:          ref,
			AmountPaise:  amtPaise,
			BalancePaise: &bal,
			SourceFile:   sourceFile,
		})
	}

	if len(rowErrs) > 0 {
		return nil, fmt.Errorf("bank csv validation failed with %d error(s):\n%w", len(rowErrs), errors.Join(rowErrs...))
	}

	return lines, nil
}

// LoadBankStatement parses and loads bank lines into the store for a given company and month.
func LoadBankStatement(ctx context.Context, st *store.Store, companyID, month string, r io.Reader, sourceFile string) (BankLoadResult, error) {
	lines, err := ParseBankCSV(r, companyID, sourceFile)
	if err != nil {
		return BankLoadResult{
			Company:    companyID,
			Month:      month,
			SourceFile: sourceFile,
		}, err
	}

	res := BankLoadResult{
		Company:    companyID,
		Month:      month,
		SourceFile: sourceFile,
		Total:      len(lines),
	}

	if len(lines) == 0 {
		return res, nil
	}

	// Balance continuity check against previous month if available
	res.Warning = checkBalanceContinuity(ctx, st, companyID, month, lines)

	// Determine how many lines are new vs updates
	var inserted, updated int
	for _, l := range lines {
		_, err := st.GetBankLine(ctx, companyID, l.TxnID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				inserted++
			} else {
				return res, fmt.Errorf("check existing bank line %s: %w", l.TxnID, err)
			}
		} else {
			updated++
		}
	}

	// Upsert all rows in one transaction
	if err := st.UpsertBankLines(ctx, lines); err != nil {
		return res, fmt.Errorf("upsert bank lines: %w", err)
	}

	res.Inserted = inserted
	res.Updated = updated
	return res, nil
}

// LoadBankFile reads a bank CSV file from disk and loads it into the database.
func LoadBankFile(ctx context.Context, st *store.Store, companyID, month, filePath string) (BankLoadResult, error) {
	f, err := os.Open(filePath) //nolint:gosec // filePath is the user or caller supplied path
	if err != nil {
		return BankLoadResult{
			Company:    companyID,
			Month:      month,
			SourceFile: filepath.Base(filePath),
		}, fmt.Errorf("open bank csv %s: %w", filePath, err)
	}
	defer f.Close()

	return LoadBankStatement(ctx, st, companyID, month, f, filepath.Base(filePath))
}

// checkBalanceContinuity checks if opening balance of first line matches
// the closing balance of the previous month.
func checkBalanceContinuity(ctx context.Context, st *store.Store, companyID, month string, lines []store.BankLine) string {
	if len(lines) == 0 || month == "" {
		return ""
	}

	first := lines[0]
	if first.BalancePaise == nil {
		return ""
	}

	// Opening balance of this month = first balance - first amount
	calculatedOpening := *first.BalancePaise - first.AmountPaise

	// Determine previous month
	mDate, err := time.Parse("2006-01", month)
	if err != nil {
		return ""
	}
	prevMonthDate := mDate.AddDate(0, -1, 0)
	prevMonth := prevMonthDate.Format("2006-01")

	// Query last day of previous month
	prevStart := prevMonthDate
	prevEnd := mDate.AddDate(0, 0, -1)

	prevLines, err := st.ListBankLines(ctx, store.BankLineFilter{
		CompanyID: companyID,
		FromDate:  &prevStart,
		ToDate:    &prevEnd,
	})
	if err != nil || len(prevLines) == 0 {
		return ""
	}

	// Last line of previous month
	lastPrev := prevLines[len(prevLines)-1]
	if lastPrev.BalancePaise == nil {
		return ""
	}

	prevClosing := *lastPrev.BalancePaise
	if prevClosing != calculatedOpening {
		return fmt.Sprintf("opening balance %s does not match previous month %s closing balance %s",
			calculatedOpening.Format(), prevMonth, prevClosing.Format())
	}

	return ""
}
