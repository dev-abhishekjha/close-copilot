package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// TestRetrieveBuildQuery: one query per planted type of the citations
// golden set (evals/golden/citations.yaml). Amounts, GSTINs and invoice
// numbers never reach a query.
func TestRetrieveBuildQuery(t *testing.T) {
	amt := money.Paise(1180000)
	cases := []struct {
		typ  string
		keys map[string]string
		want string
	}{
		{checks.TypePrepaidNotSpread, map[string]string{"invoice": "PINV-2026-00042"}, "prepaid not spread"},
		{checks.TypeMisclassifiedExpense, map[string]string{"invoice": "PINV-2026-00043"}, "misclassified expense"},
		{checks.TypeGSTR2BMissingIn2B, map[string]string{"supplier_gstin": synthGSTIN, "invoice_no_norm": "VT0917"}, "gstr2b missing in 2b"},
		{checks.TypeUnrecordedBankCharge, map[string]string{"bank_txn_id": "HDFC-20260915-C1"}, "unrecorded bank charge"},
		{checks.TypeMissingAccrual, map[string]string{"supplier": "Vaani  Telecom\nPvt Ltd", "month": "2026-09"}, "missing accrual Vaani Telecom Pvt Ltd"},
		{checks.TypeWrongPeriodPosting, map[string]string{"invoice": "PINV-2026-00044"}, "wrong period posting"},
		{checks.TypeDuplicateVendorPayment, map[string]string{"invoice": "PINV-2026-00045"}, "duplicate vendor payment"},
		{checks.TypeVariance, map[string]string{"account": "Telephone Expenses - STPL", "month": "2026-09"}, "variance Telephone Expenses - STPL"},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			f := store.Finding{Type: c.typ, Keys: c.keys, AmountPaise: &amt, Title: "Title with ₹11,800.00 and " + synthGSTIN}
			got := buildQuery(f)
			if got != c.want {
				t.Errorf("buildQuery = %q, want %q", got, c.want)
			}
			for _, bad := range []string{synthGSTIN, "PINV-", "11800", "11,800", "HDFC-"} {
				if strings.Contains(got, bad) {
					t.Errorf("query %q holds %q", got, bad)
				}
			}
		})
	}
	long := buildQuery(store.Finding{Type: checks.TypeMissingAccrual, Keys: map[string]string{"supplier": strings.Repeat("Vaani ", 100)}})
	if utf8.RuneCountInString(long) > maxQueryRunes {
		t.Errorf("query of %d runes, want at most %d", utf8.RuneCountInString(long), maxQueryRunes)
	}
}

// searchIn and searchRow are the in-test search tool's input and output
// (score as an integer: the agent reads it as a json.Number and drops it).
type searchIn struct {
	Query    string   `json:"query"`
	Company  string   `json:"company,omitempty"`
	DocTypes []string `json:"doc_types,omitempty"`
	K        int      `json:"k,omitempty"`
}

type searchRow struct {
	DocID   string `json:"doc_id"`
	DocType string `json:"doc_type"`
	Section string `json:"section"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Score   int    `json:"score"`
}

// searchServer adds search_documents to the fake evidence server; it
// records each input and returns rows (five, more than asked for) or err.
func searchServer(mu *sync.Mutex, got *[]searchIn, rows []searchRow, err error) func(*mcp.Server) {
	return func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "search_documents", Description: "documents", Annotations: ro()},
			func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, []searchRow, error) {
				mu.Lock()
				*got = append(*got, in)
				mu.Unlock()
				if err != nil {
					return nil, nil, err
				}
				return nil, rows, nil
			})
	}
}

func policyRows() []searchRow {
	return []searchRow{
		{DocID: "POL-001-accounting-policy", DocType: "policy", Section: "§4.2 Spread annual costs monthly", Title: "Accounting policy", Snippet: "Annual subscriptions are prepaid and spread monthly.", Score: 9},
		{DocID: "POL-001-accounting-policy", DocType: "policy", Section: "§4.1 Prepayments", Title: "Accounting policy", Snippet: "A payment for a future period is a prepayment.", Score: 7},
		{DocID: "NOTE-sharma-2026-09", DocType: "close_note", Section: "§2 Open items", Title: "Close note", Snippet: "The annual software renewal was paid in September.", Score: 5},
		{DocID: "POL-001-accounting-policy", DocType: "policy", Section: "§7.1 Bank charges", Title: "Accounting policy", Snippet: "Bank charges are booked monthly.", Score: 3},
		{DocID: "CON-vaani-telecom", DocType: "contract", Section: "§3 Payment terms", Title: "Contract", Snippet: "Invoices are payable within 30 days.", Score: 1},
	}
}

func TestRetrieveMCPDocuments(t *testing.T) {
	var mu sync.Mutex
	var got []searchIn
	e := newEnv(t, fakeBooks(), fakeEvidence(searchServer(&mu, &got, policyRows(), nil)))
	d := &MCPDocuments{Registry: e.registry(t)}
	f := store.Finding{ID: uuid.New(), Type: checks.TypePrepaidNotSpread, Keys: map[string]string{"invoice": "PINV-1"}}
	ps, err := d.Retrieve(t.Context(), RetrieveRequest{Company: "sharma", Month: "2026-09", Finding: f, Query: buildQuery(f)})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 || got[0].Company != "sharma" || got[0].K != maxPassages || got[0].Query != "prepaid not spread" || got[0].DocTypes != nil {
		t.Errorf("search input %+v, want company sharma, k 3 and the built query", got)
	}
	// The tool returned five rows: only the top three are kept, in order.
	if len(ps) != maxPassages {
		t.Fatalf("%d passages, want %d", len(ps), maxPassages)
	}
	rows := policyRows()
	for i, p := range ps {
		r := rows[i]
		sum := sha256.Sum256([]byte(r.Snippet))
		if p.DocID != r.DocID || p.DocType != r.DocType || p.Section != r.Section || p.Title != r.Title || p.Text != r.Snippet ||
			p.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("passage %d = %+v, want row %+v", i, p, r)
		}
	}
	if strings.Contains(e.logText(), "Annual subscriptions") {
		t.Error("passage text reached the log")
	}
}

func TestRetrieveMCPDocumentsErrors(t *testing.T) {
	var mu sync.Mutex
	var got []searchIn
	e := newEnv(t, fakeBooks(), fakeEvidence(searchServer(&mu, &got, nil, errors.New("index down: "+resultMarker))))
	d := &MCPDocuments{Registry: e.registry(t)}
	req := RetrieveRequest{Company: "sharma", Month: "2026-09", Query: "prepaid not spread"}
	_, err := d.Retrieve(t.Context(), req)
	if !errors.Is(err, ErrToolError) {
		t.Errorf("err = %v, want ErrToolError", err)
	}
	if err != nil && strings.Contains(err.Error(), resultMarker) {
		t.Error("the tool's error text is in the error")
	}
	if _, err := d.Retrieve(t.Context(), RetrieveRequest{Query: "x"}); err == nil {
		t.Error("empty company accepted")
	}
	if _, err := d.Retrieve(t.Context(), RetrieveRequest{Company: "sharma", Query: " "}); err == nil {
		t.Error("empty query accepted")
	}
	if _, err := (&MCPDocuments{}).Retrieve(t.Context(), req); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil registry: %v, want ErrUnavailable", err)
	}

	// Zero hits is an empty list, not an error.
	var got2 []searchIn
	e2 := newEnv(t, fakeBooks(), fakeEvidence(searchServer(&mu, &got2, nil, nil)))
	ps, err := (&MCPDocuments{Registry: e2.registry(t)}).Retrieve(t.Context(), req)
	if err != nil || len(ps) != 0 {
		t.Errorf("zero hits: %v, %v", ps, err)
	}
}
