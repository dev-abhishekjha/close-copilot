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
