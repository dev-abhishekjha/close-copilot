package templates

import (
	"context"
	"io"
	"unicode/utf8"

	"github.com/a-h/templ"
)

// EvidenceCell is one table cell; Link is set only on a Source cell that
// links to ERPNext.
type EvidenceCell struct {
	Text string
	Link string
}

// EvidenceRow is one selected record of a snapshot.
type EvidenceRow struct {
	Cells  []string
	Source EvidenceCell // Text empty when the row is not an ERPNext document
}

// EvidenceBlock is one evidence reference with its rows.
type EvidenceBlock struct {
	Heading   string // server.tool
	Args      string // compact JSON text
	IDs       []string
	Note      string // why there are no rows, if so
	Columns   []string
	HasSource bool
	Rows      []EvidenceRow
	More      int // rows beyond the cap
}

// TrailLine is one investigator tool call.
type TrailLine struct {
	Call string
	Args string
	Rows string
}

// CitationView is one citation.
type CitationView struct {
	DocID   string
	Section string
}

// ReviewBanner explains why a finding needs review.
type ReviewBanner struct {
	Reasons  []string // empty: show Fallback
	Fallback string
}

// ProposalLine is one debit and credit line.
type ProposalLine struct {
	Account string
	Debit   string
	Credit  string
}

// ProposalView is the proposal preview.
type ProposalView struct {
	Status      string
	Maker       string
	PostingDate string
	Remark      string
	Lines       []ProposalLine
	TotalDebit  string
	TotalCredit string
	Balanced    bool
	Difference  string
	ERPName     string
	ERPLink     string // empty when not linkable
}

// FindingActions says which action controls the page shows.
type FindingActions struct {
	CanDecide    bool // accept and dismiss forms
	CreateButton bool
	Reason       string // dismiss form echo
	Error        string
}

// FindingPage is the finding detail page.
type FindingPage struct {
	User      string
	Role      string
	CSRFToken string

	ID, RunID, Company, Month string
	Type, Severity, Title     string
	Amount, Status            string
	Verified                  bool

	Review         *ReviewBanner
	Explanation    string
	Action         string
	HasExplanation bool
	Citations      []CitationView
	Evidence       []EvidenceBlock
	HasTrail       bool
	Trail          []TrailLine
	Proposal       *ProposalView
	Actions        FindingActions
}

func findingURL(id string) string { return "/findings/" + id }

func joinIDs(ids []string) string {
	out := ""
	for i, s := range ids {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// findingFacts builds the header facts list.
func findingFacts(p FindingPage) []Fact {
	return []Fact{
		{Label: "Type", Content: spanText("type", p.Type)},
		{Label: "Severity", Content: SeverityBadge(p.Severity)},
		{Label: "Amount", Content: spanText("amount ui-num", p.Amount)},
		{Label: "Status", Content: StatusChip(p.Status)},
		{Label: "Verified", Content: VerifiedBadge(p.Verified)},
	}
}

// spanText renders text in a span with a fixed class (never user input).
func spanText(class, text string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, `<span class="`+class+`">`); err != nil {
			return err
		}
		if _, err := io.WriteString(w, templ.EscapeString(text)); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</span>`)
		return err
	})
}

// evidenceTable turns an evidence block's rows into DataTable props.
func evidenceTable(b EvidenceBlock) TableProps {
	cols := make([]Column, 0, len(b.Columns)+1)
	for _, c := range b.Columns {
		cols = append(cols, Column{Label: c})
	}
	if b.HasSource {
		cols = append(cols, Column{Label: "Source"})
	}
	rows := make([]Row, 0, len(b.Rows))
	for _, r := range b.Rows {
		cells := make([]Cell, 0, len(cols))
		for _, c := range r.Cells {
			cells = append(cells, Cell{Text: c, Class: shortClass(c)})
		}
		if b.HasSource {
			cells = append(cells, Cell{Content: sourceCell(r.Source), Class: "source"})
		}
		rows = append(rows, Row{Cells: cells})
	}
	return TableProps{Caption: b.Heading, Columns: cols, Rows: rows, Dense: true}
}

// shortClass keeps short ids and numbers on one line; long text wraps.
func shortClass(s string) string {
	if utf8.RuneCountInString(s) <= 24 {
		return "nowrap"
	}
	return ""
}

func sourceCell(c EvidenceCell) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if c.Link == "" {
			_, err := io.WriteString(w, templ.EscapeString(c.Text))
			return err
		}
		return sourceLink(c).Render(ctx, w)
	})
}

// trailEntries maps the investigation trail onto Timeline entries.
func trailEntries(ts []TrailLine) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(ts))
	for _, t := range ts {
		out = append(out, TimelineEntry{Title: t.Call, Detail: t.Args + " · rows: " + t.Rows})
	}
	return out
}

func dismissBody(p FindingPage) string {
	s := "Dismiss \"" + p.Title + "\""
	if p.Amount != "" {
		s += " (" + p.Amount + ")"
	}
	return s + "? A reason is required and is recorded in the audit log."
}

func dismissFieldAttrs() templ.Attributes { return templ.Attributes{"maxlength": "500"} }
