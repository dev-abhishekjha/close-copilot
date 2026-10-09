package seed

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/money"
)

const goldenBankCSV = "testdata/evidence/bank-sharma-2026-09-small.csv"

// testMonths are the consecutive months the invariant tests cover.
var testMonths = []string{"2026-04", "2026-05", "2026-06", "2026-07", "2026-08", "2026-09", "2026-10", "2026-11", "2026-12"}

var txnIDPattern = regexp.MustCompile(`^BNK-\d{8}-\d{3}$`)

func mustBankLines(t *testing.T, w World) []BankLine {
	t.Helper()
	lines, err := BankLines(w)
	if err != nil {
		t.Fatalf("BankLines(%s %s): %v", w.Company, w.Month, err)
	}
	return lines
}

// readCSV writes lines with WriteBankCSV and parses the file back.
func readCSV(t *testing.T, lines []BankLine) (raw []byte, rows [][]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x", BankCSVFile)
	if err := WriteBankCSV(path, lines); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return raw, rows
}

func TestBankCSVBalance(t *testing.T) {
	p := loadCompany(t, "sharma")
	for _, small := range []bool{true, false} {
		for _, month := range testMonths {
			t.Run(fmt.Sprintf("%s/small=%v", month, small), func(t *testing.T) {
				w := mustGenerate(t, p, month, Options{Small: small})
				lines := mustBankLines(t, w)

				var sum money.Paise
				cash := 0
				for _, ev := range w.Events {
					if d := ev.BankDelta(); d != 0 {
						sum += d
						cash++
					}
				}
				if len(lines) != cash {
					t.Errorf("%d lines for %d cash events", len(lines), cash)
				}
				if w.ClosingBank != w.OpeningBank+sum {
					t.Errorf("closing %s != opening %s + cash %s", w.ClosingBank.Rupees(), w.OpeningBank.Rupees(), sum.Rupees())
				}

				_, rows := readCSV(t, lines)
				if !slices.Equal(rows[0], BankCSVHeader) {
					t.Fatalf("header %v", rows[0])
				}
				balance := w.OpeningBank
				ids := map[string]bool{}
				prevDate := ""
				for i, r := range rows[1:] {
					id, date, wd, dep, bal := r[0], r[1], r[5], r[6], r[7]
					if !txnIDPattern.MatchString(id) {
						t.Errorf("row %d: txn_id %q", i, id)
					}
					if ids[id] {
						t.Errorf("row %d: duplicate txn_id %s", i, id)
					}
					ids[id] = true
					if strings.ReplaceAll(date, "-", "") != id[4:12] {
						t.Errorf("row %d: txn_id %s on %s", i, id, date)
					}
					if r[2] != date {
						t.Errorf("row %d: value_date %s, date %s", i, r[2], date)
					}
					if date < prevDate {
						t.Errorf("row %d: %s after %s", i, date, prevDate)
					}
					prevDate = date
					if (wd == "") == (dep == "") {
						t.Errorf("row %d: withdrawal %q, deposit %q", i, wd, dep)
						continue
					}
					if wd != "" {
						balance -= mustRupees(t, wd)
					} else {
						balance += mustRupees(t, dep)
					}
					if got := mustRupees(t, bal); got != balance {
						t.Errorf("row %d: balance %s, running sum %s", i, bal, balance.Rupees())
					}
					if len(r[3]) == 0 || len(r[3]) > maxNarration || r[3] != strings.ToUpper(r[3]) {
						t.Errorf("row %d: narration %q", i, r[3])
					}
				}
				if balance != w.ClosingBank {
					t.Errorf("last balance %s, closing %s", balance.Rupees(), w.ClosingBank.Rupees())
				}
			})
		}
	}
}

func mustRupees(t *testing.T, s string) money.Paise {
	t.Helper()
	if i := strings.IndexByte(s, '.'); i < 0 || len(s)-i != 3 {
		t.Errorf("amount %q doesn't have two decimals", s)
	}
	v, err := money.ParseRupees(s)
	if err != nil {
		t.Fatalf("amount %q: %v", s, err)
	}
	return v
}

func TestBankCSVGolden(t *testing.T) {
	w := mustGenerate(t, loadCompany(t, "sharma"), "2026-09", Options{Small: true})
	got, _ := readCSV(t, mustBankLines(t, w))
	if header := strings.SplitN(string(got), "\n", 2)[0]; header != "txn_id,date,value_date,narration,ref,withdrawal,deposit,balance" {
		t.Errorf("header %q", header)
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Error("CSV has carriage returns")
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenBankCSV), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenBankCSV, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenBankCSV)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("bank.csv differs from %s; run go test ./internal/seed -run TestBankCSVGolden -update and review the diff", goldenBankCSV)
	}
}

func TestBankCSVNarrations(t *testing.T) {
	w := mustGenerate(t, loadCompany(t, "sharma"), "2026-09", Options{})
	lines := mustBankLines(t, w)
	byExt := map[string]Event{}
	for _, ev := range w.Events {
		byExt[ev.ExtID] = ev
	}
	kinds := map[string]bool{}
	for _, l := range lines {
		ev := byExt[l.ExtID]
		kinds[ev.Kind] = true
		if l.Ref != ev.BankRef {
			t.Errorf("%s: ref %q, bank ref %q", l.TxnID, l.Ref, ev.BankRef)
		}
		n := l.Narration
		var ok bool
		switch ev.Kind {
		case EventVendorPayment:
			ok = strings.HasPrefix(n, "NEFT-")
			if ev.Party == "Omkar Estates" {
				ok = n == "NEFT-RENT-SEP-OMKAR ESTATES"
			}
		case EventReceipt:
			ok = n == "UPI/"+ev.BankRef+"/"+strings.ToUpper(ev.Party) || n == "NEFT-"+strings.ToUpper(ev.Party)
		case EventPayroll:
			ok = n == "SALARY BATCH SEP 2026"
		case EventBankCharge:
			ok = n == "SMS/ACCT CHARGES INCL GST" && l.Ref == ""
		case EventGatewaySettlement:
			ok = n == fmt.Sprintf("PG SETTL %s%s BATCH %s", l.Date[5:7], l.Date[8:10], ev.BankRef[3:])
		case EventInterest:
			ok = n == "INT CREDIT" && l.Ref == ""
		}
		if !ok {
			t.Errorf("%s (%s): narration %q", l.TxnID, ev.Kind, n)
		}
	}
	for _, k := range []string{EventVendorPayment, EventReceipt, EventPayroll, EventBankCharge, EventGatewaySettlement, EventInterest} {
		if !kinds[k] {
			t.Errorf("no %s line in sharma 2026-09", k)
		}
	}
	if got := withParty("NEFT-", "A Very Long Supplier Name Private Limited Of India"); len(got) > maxNarration {
		t.Errorf("narration %q is longer than %d", got, maxNarration)
	}
}

func TestBankCSVErrors(t *testing.T) {
	w := mustGenerate(t, loadCompany(t, "sharma"), "2026-09", Options{Small: true})
	bad := w
	bad.ClosingBank++
	if _, err := BankLines(bad); err == nil {
		t.Error("BankLines accepted a closing balance that doesn't add up")
	}
	path := filepath.Join(t.TempDir(), BankCSVFile)
	for _, l := range []BankLine{
		{TxnID: "BNK-20260901-001", Withdrawal: 100, Deposit: 100},
		{TxnID: "BNK-20260901-001"},
	} {
		if err := WriteBankCSV(path, []BankLine{l}); err == nil {
			t.Errorf("WriteBankCSV accepted %+v", l)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a failed write left %s: %v", path, err)
	}
}
