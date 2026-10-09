package seed

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Evidence file names under <out>/<company>/<YYYY-MM>/ (shared specs "Bank
// statement" and "GSTR-2B").
const (
	BankCSVFile = "bank.csv"
	GSTR2BFile  = "gstr2b.json"
)

// BankCSVHeader is the bank statement's header row, exactly as the shared
// spec gives it.
var BankCSVHeader = []string{"txn_id", "date", "value_date", "narration", "ref", "withdrawal", "deposit", "balance"}

// maxNarration is the length a narration is cut to, as a bank statement
// would: the party name is truncated, never the prefix.
const maxNarration = 40

// maxTxnsPerDay keeps the 3-digit txn_id sequence from overflowing.
const maxTxnsPerDay = 999

// BankLine is one row of bank.csv. Amounts are paise; exactly one of
// Withdrawal and Deposit is positive and the other is zero (written empty).
// ExtID is the event the line came from, for ground truth (CC-306); it is
// not written to the CSV.
type BankLine struct {
	TxnID      string
	Date       string
	ValueDate  string
	Narration  string
	Ref        string
	Withdrawal money.Paise
	Deposit    money.Paise
	Balance    money.Paise
	ExtID      string
}

// BankLines returns the bank statement of a world: one line per event that
// moves the bank (Event.BankDelta non-zero), in date order and otherwise in
// the world's event order, with a running balance from w.OpeningBank. The
// last balance must equal w.ClosingBank. It reads only the world, never the
// books, so the bank shows what the books may have missed.
func BankLines(w World) ([]BankLine, error) {
	byExt := make(map[string]*Event, len(w.Events))
	type cash struct {
		ev   *Event
		date time.Time
	}
	var cs []cash
	for i := range w.Events {
		ev := &w.Events[i]
		byExt[ev.ExtID] = ev
		if ev.BankDelta() == 0 {
			continue
		}
		d, err := time.Parse(dateLayout, ev.Date)
		if err != nil {
			return nil, fmt.Errorf("bank lines %s %s: event %s: date %q is not YYYY-MM-DD", w.Company, w.Month, ev.ExtID, ev.Date)
		}
		cs = append(cs, cash{ev: ev, date: d})
	}
	// The generator's order is already by date; a stable sort keeps it and
	// still places an event a planter appended on its own date.
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].date.Before(cs[j].date) })

	lines := make([]BankLine, 0, len(cs))
	balance := w.OpeningBank
	seq := map[string]int{}
	for _, c := range cs {
		ev := c.ev
		delta := ev.BankDelta()
		balance += delta
		day := c.date.Format("20060102")
		seq[day]++
		if seq[day] > maxTxnsPerDay {
			return nil, fmt.Errorf("bank lines %s %s: more than %d transactions on %s", w.Company, w.Month, maxTxnsPerDay, ev.Date)
		}
		l := BankLine{
			TxnID:     fmt.Sprintf("BNK-%s-%03d", day, seq[day]),
			Date:      ev.Date,
			ValueDate: ev.Date,
			Narration: bankNarration(*ev, c.date, byExt),
			Ref:       ev.BankRef,
			Balance:   balance,
			ExtID:     ev.ExtID,
		}
		if delta > 0 {
			l.Deposit = delta
		} else {
			l.Withdrawal = -delta
		}
		lines = append(lines, l)
	}
	if balance != w.ClosingBank {
		return nil, fmt.Errorf("bank lines %s %s: closing balance %s, but the world closes at %s",
			w.Company, w.Month, balance.Rupees(), w.ClosingBank.Rupees())
	}
	return lines, nil
}

// bankNarration is the bank's description of a cash event: upper case and
// fixed per kind, with no randomness.
func bankNarration(ev Event, date time.Time, byExt map[string]*Event) string {
	switch ev.Kind {
	case EventVendorPayment:
		if p := settledPurchase(ev, byExt); p != nil && p.Account == AccountRent {
			mon := date
			if from, err := time.Parse(dateLayout, p.Meta.ServiceFrom); err == nil {
				mon = from
			} else if d, err := time.Parse(dateLayout, p.Date); err == nil {
				mon = d
			}
			return withParty("NEFT-RENT-"+monAbbr(mon)+"-", ev.Party)
		}
		return withParty("NEFT-", ev.Party)
	case EventReceipt:
		if receiptIsUPI(ev.BankRef) {
			return withParty("UPI/"+strings.ToUpper(ev.BankRef)+"/", ev.Party)
		}
		return withParty("NEFT-", ev.Party)
	case EventPayroll:
		return fmt.Sprintf("SALARY BATCH %s %04d", monAbbr(date), date.Year())
	case EventBankCharge:
		return "SMS/ACCT CHARGES INCL GST"
	case EventGatewaySettlement:
		return fmt.Sprintf("PG SETTL %s BATCH %s", date.Format("0102"), batchNumber(ev.BankRef))
	case EventInterest:
		return "INT CREDIT"
	}
	return withParty(strings.ToUpper(ev.Kind)+"-", ev.Party)
}

// settledPurchase returns the purchase a vendor payment settles, when it is
// in the same world.
func settledPurchase(ev Event, byExt map[string]*Event) *Event {
	for _, ref := range ev.Refs {
		if p, ok := byExt[ref]; ok && p.Kind == EventPurchase {
			return p
		}
	}
	return nil
}

// receiptIsUPI chooses a direct receipt's channel from its reference: a
// "UPI" prefix is UPI and an "N" or "NEFT" prefix is NEFT. The generator
// issues every direct receipt a UTR (UTR and nine digits); a UTR whose
// number is even arrived by UPI and an odd one by NEFT, so a statement
// shows both channels and the same reference always reads the same way.
func receiptIsUPI(ref string) bool {
	r := strings.ToUpper(ref)
	switch {
	case strings.HasPrefix(r, "UPI"):
		return true
	case strings.HasPrefix(r, "NEFT"), strings.HasPrefix(r, "N"):
		return false
	case r == "":
		return false
	}
	last := r[len(r)-1]
	return isDigit(last) && (last-'0')%2 == 0
}

// batchNumber is a settlement reference's number: PGS7781 -> 7781.
func batchNumber(ref string) string {
	n := strings.TrimLeftFunc(ref, func(r rune) bool { return r < '0' || r > '9' })
	if n == "" {
		return strings.ToUpper(ref)
	}
	return n
}

// monAbbr is the upper-case three-letter month: SEP.
func monAbbr(t time.Time) string { return strings.ToUpper(t.Format("Jan")) }

// withParty appends the upper-cased party to prefix, truncating the party
// so the narration fits maxNarration characters.
func withParty(prefix, party string) string {
	p := []rune(strings.ToUpper(strings.TrimSpace(party)))
	if room := maxNarration - len([]rune(prefix)); len(p) > room {
		p = p[:max(room, 0)]
	}
	name := strings.TrimRight(string(p), " ")
	if name == "" {
		return strings.TrimRight(prefix, "-/")
	}
	return prefix + name
}

// WriteBankCSV writes lines to path as bank.csv: the shared-spec header,
// two-decimal rupees, exactly one of withdrawal and deposit per row, "\n"
// line endings and encoding/csv quoting. It creates the parent directory
// and writes atomically (a temporary file, then a rename).
func WriteBankCSV(path string, lines []BankLine) error {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	if err := cw.Write(BankCSVHeader); err != nil {
		return fmt.Errorf("write bank csv: %w", err)
	}
	var errs []error
	for _, l := range lines {
		var wd, dep string
		switch {
		case l.Withdrawal > 0 && l.Deposit == 0:
			wd = l.Withdrawal.Rupees()
		case l.Deposit > 0 && l.Withdrawal == 0:
			dep = l.Deposit.Rupees()
		default:
			errs = append(errs, fmt.Errorf("line %s: withdrawal %s and deposit %s; exactly one must be positive",
				l.TxnID, l.Withdrawal.Rupees(), l.Deposit.Rupees()))
			continue
		}
		rec := []string{l.TxnID, l.Date, l.ValueDate, l.Narration, l.Ref, wd, dep, l.Balance.Rupees()}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("write bank csv: %w", err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("write bank csv: %w", err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("write bank csv: %w", err)
	}
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		return fmt.Errorf("write bank csv: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory and a rename, creating the directory, so a crash never leaves
// a partial file under the final name.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
