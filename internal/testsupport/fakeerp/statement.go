package fakeerp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Charge is one bank charge on the statement that the books never
// recorded.
type Charge struct {
	TxnID, Date, Narration, Amount string // Amount in rupees text, negative
}

// PlantedCharges are the three bank charges planted on September's
// statement.
var PlantedCharges = []Charge{
	{"HDFC-20260915-C1", "2026-09-15", "NEFT CHARGES INCL GST", "-5.90"},
	{"HDFC-20260920-C2", "2026-09-20", "DEBIT CARD ANNUAL FEE", "-590.00"},
	{"HDFC-20260930-C3", "2026-09-30", "SMS CHGS JUL-SEP 2026", "-17.70"},
}

// StatementOptions varies September's statement.
type StatementOptions struct {
	// OmitLastCashSales leaves the last n cash sales off the statement, so
	// their GL Entries stay unmatched.
	OmitLastCashSales int
	// UnknownDeposit adds one deposit the books don't explain.
	UnknownDeposit bool
}

// SourceFile is the statement file name the lines carry.
const SourceFile = "fakeerp-hdfc-2026-09.csv"

// Statement is September's bank statement for the synthetic month: the
// rent payment (by UTR), the gateway receipt, the cash sales and the three
// planted charges, varied by opts.
func Statement(opts StatementOptions) ([]store.BankLine, error) {
	if opts.OmitLastCashSales < 0 || opts.OmitLastCashSales > CashSales {
		return nil, fmt.Errorf("fakeerp: omit %d cash sales: out of range", opts.OmitLastCashSales)
	}
	var errs []error
	line := func(txn, date, narration, amount string, ref *string) store.BankLine {
		d, err := time.Parse(time.DateOnly, date)
		if err != nil {
			errs = append(errs, err)
		}
		p, err := money.ParseRupees(amount)
		if err != nil {
			errs = append(errs, err)
		}
		return store.BankLine{CompanyID: CompanyID, TxnID: txn, TxnDate: d, Narration: narration, Ref: ref,
			AmountPaise: p, SourceFile: SourceFile}
	}
	utr := "UTR2026091000123"
	out := []store.BankLine{
		line("HDFC-20260910-R1", "2026-09-10", "NEFT VARDHAN ESTATES RENT", "-59000.00", &utr),
		line("HDFC-20260913-P1", "2026-09-13", "PG SETTL TATVA RETAIL", "1180.00", nil),
	}
	if opts.UnknownDeposit {
		out = append(out, line("HDFC-20260918-X1", "2026-09-18", "IMPS UNKNOWN REMITTER", "2500.00", nil))
	}
	for i := range CashSales - opts.OmitLastCashSales {
		out = append(out, line("HDFC-CASH-"+CashSaleDate(i)+"-"+CashSale(i), CashSaleDate(i), "CASH DEPOSIT BRANCH", CashSale(i), nil))
	}
	for _, c := range PlantedCharges {
		out = append(out, line(c.TxnID, c.Date, c.Narration, c.Amount, nil))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("fakeerp: statement: %w", err)
	}
	return out, nil
}

// SkeletonStatement is the statement whose only findings are the three
// planted bank charges: every other line matches the books.
func SkeletonStatement() ([]store.BankLine, error) {
	return Statement(StatementOptions{})
}

// ParityStatement is the statement of the CC-702 parity test: all but the
// last ten cash sales, the planted charges and one unexplained deposit.
func ParityStatement() ([]store.BankLine, error) {
	return Statement(StatementOptions{OmitLastCashSales: 10, UnknownDeposit: true})
}

// Seed stores the synthetic company (with its ERPNext name) and the given
// bank lines in st.
func Seed(ctx context.Context, st *store.Store, lines []store.BankLine) error {
	if err := st.UpsertCompany(ctx, store.Company{ID: CompanyID, ERPCompany: ERPCompany, GSTIN: CompanyGSTIN}); err != nil {
		return fmt.Errorf("fakeerp: seed company: %w", err)
	}
	if len(lines) == 0 {
		return nil
	}
	if err := st.UpsertBankLines(ctx, lines); err != nil {
		return fmt.Errorf("fakeerp: seed bank lines: %w", err)
	}
	return nil
}
