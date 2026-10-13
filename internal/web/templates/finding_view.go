package templates

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
