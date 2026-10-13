package templates

import "strconv"

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
func rowClass(nested bool) string {
	if nested {
		return "finding nested"
	}
	return "finding"
}
func stepClass(s TimelineStep) string {
	if s.Current {
		return "step current"
	}
	return "step"
}
