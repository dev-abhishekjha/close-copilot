package templates

import (
	"strconv"

	"github.com/a-h/templ"
)

// RunRow is one row of the past-runs table.
type RunRow struct {
	ID       string
	Company  string
	Month    string
	Status   string
	Findings int
	Cost     string
	Duration string
}

// RunsPage is the runs page: the start form (makers only) and past runs.
type RunsPage struct {
	User      string
	Role      string
	CSRFToken string
	CanStart  bool
	Companies []string
	Company   string // form echo after an error
	Month     string // form echo after an error
	Error     string
	Runs      []RunRow
	// ShowSummary is true when the audit backend is set; Summary is nil
	// when no run has finished.
	ShowSummary bool
	Summary     *SummaryCard
}

// TimelineStep is one state in the run's timeline.
type TimelineStep struct {
	Status  string
	Current bool
}

// TypeCount is the number of findings of one type.
type TypeCount struct {
	Type  string
	Count int
}

// RunPanel is the progress panel htmx refreshes.
type RunPanel struct {
	ID              string
	Status          string
	Polling         bool // true while the run is not terminal
	Timeline        []TimelineStep
	Counts          []TypeCount
	Total           int
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	Cost            string
	Duration        string
	Error           string
}

// RunPage is the run page.
type RunPage struct {
	User      string
	Role      string
	CSRFToken string
	CanStart  bool // maker: the Re-run button is shown on a finished run
	Company   string
	Month     string
	Panel     RunPanel
}

// FindingRow is one finding in the findings table. Nested holds the
// variance findings that link to it.
type FindingRow struct {
	ID       string
	Severity string
	Type     string
	Title    string
	Amount   string
	Status   string
	Verified bool
	Nested   []FindingRow
}

// FindingsPage is the findings list of one run.
type FindingsPage struct {
	User      string
	Role      string
	CSRFToken string
	RunID     string
	Company   string
	Month     string
	Types     []string // filter options, from the run's findings
	Statuses  []string
	SelType   string
	SelStatus string
	Rows      []FindingRow
}

func runURL(id string) string      { return "/runs/" + id }
func progressURL(id string) string { return "/runs/" + id + "/progress" }
func findingsURL(id string) string { return "/runs/" + id + "/findings" }

func itoa(n int) string  { return strconv.Itoa(n) }
func i64(n int64) string { return strconv.FormatInt(n, 10) }

// SummaryCard is the last finished run's headline on the runs page.
type SummaryCard struct {
	RunID        string
	Company      string
	Month        string
	VerifiedRate string // "N of M (P%)" or "n/a"
	OpenHigh     int
}

// AuditRow is one audit log entry. ArgsFull is the whole compact JSON,
// shown in a <details>; ArgsShort is truncated.
type AuditRow struct {
	Time        string
	Actor       string
	Server      string
	Tool        string
	ArgsShort   string
	ArgsFull    string
	Truncated   bool
	ResultCount string
	Latency     string
	Error       string
}

// AuditPage is the checker's audit log page.
type AuditPage struct {
	User      string
	Role      string
	CSRFToken string
	Actors    []string // filter options
	Servers   []string
	Tools     []string
	SelActor  string
	SelServer string
	SelTool   string
	From      string // YYYY-MM-DD echo
	To        string
	Rows      []AuditRow
	OlderURL  string // empty when there are no older rows
}

// StepRow is the usage of one step family on the metrics page.
type StepRow struct {
	Kind    string
	Input   int64
	Output  int64
	Cache   int64
	Cost    string
	Calls   int64
	Latency int64
}

// MetricsPage is the run metrics page.
type MetricsPage struct {
	User         string
	Role         string
	CSRFToken    string
	RunID        string
	Company      string
	Month        string
	Status       string
	Duration     string
	Input        int64
	Output       int64
	Cache        int64
	Cost         string
	Steps        []StepRow
	VerifiedRate string
	TraceID      string
}

func metricsURL(id string) string { return "/runs/" + id + "/metrics" }

// finished reports whether a run status is terminal.
func finished(status string) bool {
	return status == "done" || status == "partial" || status == "failed"
}

func companyOptions(cs []string) []Option {
	out := make([]Option, 0, len(cs))
	for _, c := range cs {
		out = append(out, Option{Value: c, Label: c})
	}
	return out
}

// filterOptions is the "All" option followed by the given values.
func filterOptions(vals []string) []Option {
	out := make([]Option, 0, len(vals)+1)
	out = append(out, Option{Value: "", Label: "All"})
	for _, v := range vals {
		out = append(out, Option{Value: v, Label: v})
	}
	return out
}

const monthPattern = `\d{4}-(0[1-9]|1[0-2])`

func runTitle(company, month string) string { return company + " " + month }

var runColumns = []Column{
	{Label: "Company"}, {Label: "Month"}, {Label: "Status"},
	{Label: "Findings", Align: "right"}, {Label: "Cost", Align: "right"}, {Label: "Duration", Align: "right"},
}

func runRows(runs []RunRow) []Row {
	rows := make([]Row, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, Row{
			Attrs: templ.Attributes{"data-run": r.ID, "data-status": r.Status},
			Cells: []Cell{
				{Text: r.Company, Href: runURL(r.ID)},
				{Text: r.Month},
				{Content: StatusChip(r.Status), Class: "status"},
				{Text: itoa(r.Findings), Num: true, Class: "findings"},
				{Text: r.Cost, Num: true, Class: "cost"},
				{Text: r.Duration, Num: true, Class: "duration"},
			},
		})
	}
	return rows
}

// progressSteps maps a run status onto the real lifecycle: queued, running,
// then one final step named for the terminal status.
func progressSteps(status string) []Step {
	queued, running := Step{Label: "Queued", Key: "queued"}, Step{Label: "Running", Key: "running"}
	final := Step{Label: "Done", Key: "done", State: "pending"}
	switch status {
	case "queued":
		queued.State, running.State = "active", "pending"
	case "running":
		queued.State, running.State = "done", "active"
	default:
		queued.State, running.State = "done", "done"
		if finished(status) {
			final.Key = status
			final.Label = map[string]string{"done": "Done", "partial": "Partial", "failed": "Failed"}[status]
			final.State = "done"
			if status == "failed" {
				final.State = "failed"
			}
		}
	}
	return []Step{queued, running, final}
}

func currentStepLabel(status string) string {
	for _, s := range progressSteps(status) {
		if s.State == "active" {
			return s.Label
		}
	}
	return status
}

var findingColumns = []Column{
	{Label: "Severity"}, {Label: "Type"}, {Label: "Title"}, {Label: "Amount", Align: "right"},
	{Label: "Status"}, {Label: "Verified"}, {Label: "Detail"},
}

func findingTableRows(rows []FindingRow) []Row {
	var out []Row
	add := func(r FindingRow, nested bool) {
		attrs := templ.Attributes{"data-finding": r.ID, "data-severity": r.Severity}
		title := "title"
		if nested {
			attrs["data-nested"] = "true"
			title = "title nested-title"
		}
		out = append(out, Row{Attrs: attrs, Cells: []Cell{
			{Content: SeverityBadge(r.Severity), Class: "severity"},
			{Text: r.Type, Class: "type"},
			{Text: r.Title, Class: title},
			{Text: r.Amount, Num: true, Class: "amount"},
			{Content: StatusChip(r.Status), Class: "status"},
			{Content: VerifiedBadge(r.Verified), Class: "verified"},
			{Text: "Details", Href: findingURL(r.ID), Class: "detail"},
		}})
	}
	for _, r := range rows {
		add(r, false)
		for _, n := range r.Nested {
			add(n, true)
		}
	}
	return out
}
