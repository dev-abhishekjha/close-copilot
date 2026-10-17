package fixture

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/retrieval"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// Ask answers a question that mentions the bank with a cited answer and
// refuses everything else. The refusal is the product's fixed wording for a
// question that is not about the books.
type Ask struct{}

var _ web.AskBackend = Ask{}

// RefusalText is the refusal the fixture gives.
const RefusalText = "I can only answer questions about this company's books for the selected month."

// Ask implements web.AskBackend.
func (Ask) Ask(_ context.Context, in web.AskInput) (web.AskOutput, error) {
	if strings.Contains(strings.ToLower(in.Question), "bank") {
		return web.AskOutput{
			RunID: RunDone, Action: "answer", Verified: true,
			Answer:      "There are three unbooked bank charges in September 2026, totalling Rs 2,012.00.",
			Citations:   []store.Citation{{DocID: "POL-001", Section: "4.2"}},
			EvidenceIDs: []string{"BT-0001", "BT-0002", "BT-0003"}, StepRefs: []string{"step-1"},
		}, nil
	}
	return web.AskOutput{RunID: RunDone, Action: "refuse", Refusal: RefusalText}, nil
}

// Documents is the in-memory web.DocumentsBackend.
type Documents struct{}

var _ web.DocumentsBackend = Documents{}

func company(id string) *string { return &id }

var docs = []retrieval.Document{
	{DocID: "POL-001", DocType: "policy", Title: "Bank charges policy", Sections: []retrieval.DocSection{
		{Section: "4.1", Content: "Bank charges are booked in the month the bank debits them."},
		{Section: "4.2", Content: "A charge with no matching entry is booked to Bank Charges after a checker approves the entry."},
	}},
	{DocID: "CON-101", DocType: "contract", Title: "Current account agreement", CompanyID: company("mehta"), Sections: []retrieval.DocSection{
		{Section: "1", Content: "The bank may debit maintenance and SMS alert charges monthly."},
	}},
	{DocID: "CN-2026-09", DocType: "close_note", Title: "September close note", CompanyID: company("mehta"), Sections: []retrieval.DocSection{
		{Section: "1", Content: "Three bank charges were still unbooked at cut-off."},
		{Section: "2", Content: "Accruals are unchanged from August."},
	}},
	{DocID: "CON-201", DocType: "contract", Title: "Warehouse lease", CompanyID: company("sharma"), Sections: []retrieval.DocSection{
		{Section: "1", Content: "Rent is payable on the first of each month."},
	}},
}

func visible(d retrieval.Document, companyID string) bool {
	return d.CompanyID == nil || *d.CompanyID == companyID
}

// ListDocuments implements web.DocumentsBackend.
func (Documents) ListDocuments(_ context.Context, companyID string, docTypes []string) ([]retrieval.DocSummary, error) {
	var out []retrieval.DocSummary
	for _, d := range docs {
		if visible(d, companyID) && (len(docTypes) == 0 || slices.Contains(docTypes, d.DocType)) {
			out = append(out, retrieval.DocSummary{DocID: d.DocID, DocType: d.DocType, Title: d.Title, CompanyID: d.CompanyID, Sections: len(d.Sections)})
		}
	}
	return out, nil
}

// GetDocument implements web.DocumentsBackend.
func (Documents) GetDocument(_ context.Context, companyID, docID string) (retrieval.Document, error) {
	for _, d := range docs {
		if d.DocID == docID && visible(d, companyID) {
			return d, nil
		}
	}
	return retrieval.Document{}, fmt.Errorf("fixture: document %s: %w", docID, retrieval.ErrDocNotFound)
}

// System reports every check as healthy.
type System struct{}

var _ web.SystemBackend = System{}

// Postgres implements web.SystemBackend.
func (System) Postgres(context.Context) error { return nil }

// ERPNext implements web.SystemBackend.
func (System) ERPNext(context.Context) error { return nil }

// SpendToday implements web.SystemBackend.
func (System) SpendToday(context.Context) (web.SpendToday, error) {
	return web.SpendToday{USD: "0.42", Fraction: 0.08}, nil
}
