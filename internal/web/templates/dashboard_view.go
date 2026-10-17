package templates

import (
	"net/url"

	"github.com/a-h/templ"
)

// View types of the dashboard (UI-04). The handler fills them from the
// interfaces the web layer already has; each section carries its own Off
// (backend not configured) and Err (generic text, the cause is logged) so a
// failure in one never breaks the others.

// DashboardView is the signed-in landing page.
type DashboardView struct {
	User, Role, CSRFToken string
	Company, Month        string
	StartHref             string // maker only; empty hides the Start button
	AskHref               string // empty hides the Ask button
	Context               DashContext
	Latest                DashLatest
	Waiting               DashWaiting
	Actions               []Action
	Pages                 []Destination // the pages the role can open, for the quick-actions card
}

// DashBar is one severity line: a badge, a count and a meter.
type DashBar struct {
	Severity string
	Count    int
	Max      int
}

// DashCount is one status line.
type DashCount struct {
	Status string
	Count  int
}

// DashContext summarises the run for the selected company and month.
type DashContext struct {
	Off, NoCompany bool
	Err            string // the run list failed
	HasRun         bool
	RunID, Status  string
	RunHref        string // empty when the user may not open the run
	FindingsErr    string // the findings of the run failed
	Total, High    int
	NeedsReview    int
	Unverified     int
	Severity       []DashBar
	Statuses       []DashCount
}

// DashCompanyRow is one company's latest run.
type DashCompanyRow struct {
	Company, RunID, Month, Status, Started string
	Findings                               int
	HasRun                                 bool
	Link                                   bool
}

// DashLatest is the latest run per company.
type DashLatest struct {
	Off  bool
	Err  string
	Rows []DashCompanyRow
	More int
}

// DashItem is one linked entry of the waiting list.
type DashItem struct {
	Label, Href string
}

// DashWaiting is the "waiting on you" card. Kind is checker, maker or viewer.
type DashWaiting struct {
	Kind         string
	Off          bool
	Err          string
	Count        int
	Items        []DashItem
	ViewAll      string
	ViewAllLabel string
}

func dashSubtitle(v DashboardView) string {
	if v.Company == "" {
		return v.Month
	}
	return v.Company + " " + v.Month
}

// DashStartHref is the Start link: the runs page with the context filled in.
func DashStartHref(company, month string) string {
	return "/runs?" + url.Values{"company": {company}, "month": {month}}.Encode()
}

func dashHeaderActions(v DashboardView) templ.Component {
	if v.StartHref == "" && v.AskHref == "" {
		return nil
	}
	return dashActionButtons(v)
}

func dashStatusRows(cs []DashCount) []Row {
	rows := make([]Row, 0, len(cs))
	for _, c := range cs {
		rows = append(rows, Row{
			Attrs: templ.Attributes{"data-status": c.Status},
			Cells: []Cell{{Content: StatusChip(dashStatusLabel(c.Status)), Class: "status"}, {Text: itoa(c.Count), Num: true}},
		})
	}
	return rows
}

var dashStatusColumns = []Column{{Label: "Status"}, {Label: "Findings", Align: "right"}}

var dashLatestColumns = []Column{
	{Label: "Company"}, {Label: "Month"}, {Label: "Status"},
	{Label: "Findings", Align: "right"}, {Label: "Started"},
}

func dashLatestRows(rs []DashCompanyRow) []Row {
	rows := make([]Row, 0, len(rs))
	for _, r := range rs {
		name := Cell{Text: r.Company}
		if r.HasRun && r.Link {
			name.Href = runURL(r.RunID)
		}
		if !r.HasRun {
			rows = append(rows, Row{
				Attrs: templ.Attributes{"data-company": r.Company, "data-status": "none"},
				Cells: []Cell{name, {Text: "-", Class: "month"}, {Text: "No runs yet", Class: "status"}, {Text: "-", Num: true}, {Text: "-", Class: "started"}},
			})
			continue
		}
		rows = append(rows, Row{
			Attrs: templ.Attributes{"data-company": r.Company, "data-run": r.RunID, "data-status": r.Status},
			Cells: []Cell{
				name, {Text: r.Month, Class: "month"}, {Content: StatusChip(r.Status), Class: "status"},
				{Text: itoa(r.Findings), Num: true}, {Text: r.Started, Class: "started"},
			},
		})
	}
	return rows
}

func dashStatTone(n int, tone string) string {
	if n > 0 {
		return tone
	}
	return ""
}

func dashFindingsHref(c DashContext) string {
	if c.RunHref == "" {
		return ""
	}
	return c.RunHref + "/findings"
}

func dashWaitingNoun(w DashWaiting) string {
	if w.Kind == "checker" {
		if w.Count == 1 {
			return "proposal awaits your decision"
		}
		return "proposals await your decision"
	}
	if w.Count == 1 {
		return "finding is still open or needs review"
	}
	return "findings are still open or need review"
}

// dashStatusLabel gives the finding statuses the chip has no entry for a
// readable label; the others keep their own chip.
func dashStatusLabel(s string) string {
	if s == "needs_review" {
		return "Needs review"
	}
	return s
}
