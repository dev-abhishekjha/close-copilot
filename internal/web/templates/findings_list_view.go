package templates

import "github.com/a-h/templ"

// ListRow is one row of the findings list. Nested rows are variance
// findings shown under their parent; CanSelect is true only for a maker and
// a finding that is open or needs review.
type ListRow struct {
	FindingRow
	IsNested  bool
	CanSelect bool
}

// Refusal is one finding a bulk accept did not accept, and why.
type Refusal struct {
	Title  string
	Reason string
}

// BulkResult is the outcome of one bulk accept, rendered on the result page.
type BulkResult struct {
	Accepted int
	Refused  []Refusal
}

// Hidden is one hidden form field.
type Hidden struct{ Name, Value string }

// FindingsListPage is the findings list of the selected company and month.
type FindingsListPage struct {
	User      string
	Role      string
	CSRFToken string
	Company   string
	Month     string
	HasRun    bool
	CanBulk   bool // maker: checkboxes and the bulk button
	CanStart  bool // maker: the empty state links to /runs

	Tabs    []Tab
	Hidden  []Hidden // carried by the filter form
	Types   []string
	SelSev  string
	SelType string
	SelVer  string
	Query   string // the q filter

	Columns []Column
	Rows    []ListRow
	Total   int    // top-level findings after filters
	Showing string // "Showing x-y of N", empty when there are none
	PrevURL string
	NextURL string

	ReturnURL string // /findings?... carried by the bulk form
	Error     string // a refused bulk request
	Result    *BulkResult
}

func (p FindingsListPage) emptyTitle() string {
	return "No close run for " + runTitle(p.Company, p.Month) + " yet"
}

func (p FindingsListPage) severityOptions() []Option {
	return []Option{{Value: "", Label: "All"}, {Value: "high", Label: "High"}, {Value: "medium", Label: "Medium"}, {Value: "low", Label: "Low"}}
}

func (p FindingsListPage) verifiedOptions() []Option {
	return []Option{{Value: "", Label: "All"}, {Value: "yes", Label: "Verified"}, {Value: "no", Label: "Not verified"}}
}

func (p FindingsListPage) columns() []Column {
	if !p.CanBulk {
		return p.Columns
	}
	return append([]Column{{Label: "Select"}}, p.Columns...)
}

func listTableRows(p FindingsListPage) []Row {
	out := make([]Row, 0, len(p.Rows))
	for _, r := range p.Rows {
		attrs := templ.Attributes{"data-finding": r.ID, "data-severity": r.Severity}
		title := "title"
		if r.IsNested {
			attrs["data-nested"] = "true"
			title = "title nested-title"
		}
		var cells []Cell
		if p.CanBulk {
			if r.CanSelect {
				cells = append(cells, Cell{Content: selectBox(r.ID, r.Title), Class: "select"})
			} else {
				cells = append(cells, Cell{Class: "select"})
			}
		}
		cells = append(cells,
			Cell{Content: SeverityBadge(r.Severity), Class: "severity"},
			Cell{Text: r.Type, Class: "type"},
			Cell{Text: r.Title, Class: title},
			Cell{Text: r.Amount, Num: true, Class: "amount"},
			Cell{Content: StatusChip(r.Status), Class: "status"},
			Cell{Content: VerifiedBadge(r.Verified), Class: "verified"},
			Cell{Text: "Details", Href: findingURL(r.ID), Class: "detail"},
		)
		out = append(out, Row{Attrs: attrs, Cells: cells})
	}
	return out
}

func qAttrs() templ.Attributes { return templ.Attributes{"maxlength": "100"} }

// startAction is the empty state's link to the runs page, for makers.
func startAction(p FindingsListPage) *ButtonProps {
	if !p.CanStart {
		return nil
	}
	return &ButtonProps{Label: "Start a close", Href: "/runs", Variant: "secondary", Icon: "play"}
}

func resultTone(r BulkResult) string {
	if len(r.Refused) > 0 {
		return "warn"
	}
	return "info"
}
