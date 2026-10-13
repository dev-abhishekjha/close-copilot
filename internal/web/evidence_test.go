package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func mustSnap(t testing.TB, js string) *snapshot {
	t.Helper()
	s, err := decodeSnapshot([]byte(js))
	if err != nil {
		t.Fatalf("decodeSnapshot: %v", err)
	}
	return &s
}

func TestEvidenceRows(t *testing.T) {
	list := `{"server":"evidence","tool":"list_bank_lines","args":{},"result":[
		{"txn_id":"T1","amount":-35400,"narration":"fee"},
		{"txn_id":"T2","amount":9007199254740993,"narration":"big"},
		{"txn_id":"T3","amount":100,"narration":"x","nested":{"a":[1,2]}}]}`
	obj := `{"server":"evidence","tool":"list_bank_lines","args":{},"result":{"count":2,"lines":[{"txn_id":"T1"}],"more":[{"txn_id":"T9"}]}}`
	var many strings.Builder
	many.WriteString(`{"server":"evidence","tool":"list_bank_lines","args":{},"result":[`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `{"txn_id":"X%d"}`, i)
	}
	many.WriteString("]}")

	tests := []struct {
		name      string
		snap      string
		ref       store.EvidenceRef
		loadErr   error
		wantRows  int
		wantMore  int
		wantNote  string
		wantCells []string // must all appear among the first row's cells
	}{
		{"by id", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"T1"}, Artifact: "a"}, nil, 1, 0, "", []string{"T1", "-₹354.00", "fee"}},
		{"no ids shows all", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", Artifact: "a"}, nil, 3, 0, "", nil},
		{"no match", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"nope"}, Artifact: "a"}, nil, 0, 0, "no matching rows in the snapshot", nil},
		{"array in object", obj, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", Artifact: "a"}, nil, 2, 0, "", []string{"T1"}},
		{"cap", many.String(), store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", Artifact: "a"}, nil, 50, 10, "", nil},
		{"wrong tool", list, store.EvidenceRef{Server: "evidence", Tool: "list_gstr2b_entries", Artifact: "a"}, nil, 0, 0, "snapshot does not match the reference", nil},
		{"wrong server", list, store.EvidenceRef{Server: "books", Tool: "list_bank_lines", Artifact: "a"}, nil, 0, 0, "snapshot does not match the reference", nil},
		{"no artifact", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"T1"}}, nil, 0, 0, "snapshot not stored", nil},
		{"unreadable", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", Artifact: "a"}, store.ErrNotFound, 0, 0, "snapshot unavailable", nil},
		{"large integer keeps precision", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"T2"}, Artifact: "a"}, nil, 1, 0, "", []string{"₹9,00,71,99,25,47,409.93"}},
		{"nested as json", list, store.EvidenceRef{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"T3"}, Artifact: "a"}, nil, 1, 0, "", []string{`{"a":[1,2]}`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var snap *snapshot
			if tc.loadErr == nil {
				snap = mustSnap(t, tc.snap)
			}
			b := evidenceBlock("http://erp", tc.ref, snap, tc.loadErr)
			if len(b.Rows) != tc.wantRows || b.More != tc.wantMore || b.Note != tc.wantNote {
				t.Fatalf("rows=%d more=%d note=%q, want %d %d %q", len(b.Rows), b.More, b.Note, tc.wantRows, tc.wantMore, tc.wantNote)
			}
			for _, want := range tc.wantCells {
				if !contains(b.Rows[0].Cells, want) {
					t.Errorf("first row %v lacks %q", b.Rows[0].Cells, want)
				}
			}
		})
	}

	t.Run("columns sorted with name first", func(t *testing.T) {
		s := mustSnap(t, `{"server":"books","tool":"list_gl_entries","args":{},"result":[{"zeta":"1","debit":5,"voucher_no":"V1","name":"G1","alpha":"2"}]}`)
		b := evidenceBlock("http://erp", store.EvidenceRef{Server: "books", Tool: "list_gl_entries", Artifact: "a"}, s, nil)
		if got := strings.Join(b.Columns, ","); got != "name,voucher_no,alpha,debit,zeta" {
			t.Errorf("columns = %s", got)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		if _, err := decodeSnapshot([]byte(`{`)); err == nil {
			t.Error("want error")
		}
		if _, err := decodeSnapshot([]byte(`{} {}`)); err == nil {
			t.Error("want error for trailing data")
		}
	})
	t.Run("non-object record", func(t *testing.T) {
		s := mustSnap(t, `{"server":"books","tool":"get_trial_balance","args":{},"result":"hello"}`)
		b := evidenceBlock("", store.EvidenceRef{Server: "books", Tool: "get_trial_balance", Artifact: "a"}, s, nil)
		if len(b.Rows) != 1 || b.Rows[0].Cells[0] != "hello" {
			t.Errorf("rows = %+v", b.Rows)
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestErpLinkFunc(t *testing.T) {
	tests := []struct {
		base, tool string
		rec        map[string]any
		want       string
	}{
		{"http://h:8080", "list_purchase_invoices", map[string]any{"name": "PINV-1"}, "http://h:8080/app/purchase-invoice/PINV-1"},
		{"http://h:8080/", "list_sales_invoices", map[string]any{"name": "SINV-1"}, "http://h:8080/app/sales-invoice/SINV-1"},
		{"https://h", "list_payments", map[string]any{"name": "PE-1"}, "https://h/app/payment-entry/PE-1"},
		{"http://h", "list_gl_entries", map[string]any{"name": "GL-9", "voucher_type": "Journal Entry", "voucher_no": "JV-2"}, "http://h/app/journal-entry/JV-2"},
		{"http://h", "list_gl_entries", map[string]any{"voucher_type": "Journal/Entry", "voucher_no": "JV-2"}, ""},
		{"http://h", "list_purchase_invoices", map[string]any{"name": "a b/c?d"}, "http://h/app/purchase-invoice/a%20b%2Fc%3Fd"},
		{"http://h", "list_bank_lines", map[string]any{"txn_id": "T1"}, ""},
		{"http://h", "list_gstr2b_entries", map[string]any{"name": "x"}, ""},
		{"http://h", "list_purchase_invoices", map[string]any{"name": ""}, ""},
		{"", "list_purchase_invoices", map[string]any{"name": "P"}, ""},
		{"ftp://h", "list_purchase_invoices", map[string]any{"name": "P"}, ""},
		{"http://", "list_purchase_invoices", map[string]any{"name": "P"}, ""},
		{"javascript:alert(1)", "list_purchase_invoices", map[string]any{"name": "P"}, ""},
	}
	for i, tc := range tests {
		got, ok := erpLink(tc.base, tc.tool, tc.rec)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%d: erpLink(%q,%q) = %q,%v want %q", i, tc.base, tc.tool, got, ok, tc.want)
		}
	}
}
