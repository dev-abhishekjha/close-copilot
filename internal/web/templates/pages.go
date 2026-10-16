// Package templates holds the web UI's templ components (CC-1001). The
// generated *_templ.go files are committed; regenerate with `make generate`.
package templates

import "encoding/json"

// htmx 2.0.4 is vendored as internal/web/static/htmx.min.js, served from
// this origin (the Content-Security-Policy allows only 'self' for scripts)
// and still loaded with Subresource Integrity so a changed file fails closed.
const (
	htmxSrc       = "/static/htmx.min.js"
	htmxIntegrity = "sha384-HGfztofotfshcF7+8n44JQL2oJmowVChPTg48S+jvZoztPfvwD79OC/LTtG6dMp+"
	// htmxConfig keeps htmx within the strict CSP and this origin: no inline
	// indicator style, no eval, no inline script tags in swapped content, and
	// no requests to other origins.
	htmxConfig = `{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false,"selfRequestsOnly":true}`
)

// Layout is the base page. CSRFToken, when set, is sent by htmx on every
// request through hx-headers. User, Role and CSRFToken are kept for pages
// drawn without the app shell; when the request carries a ShellData, its
// values win. Crumbs is the optional breadcrumb trail for the shell's top bar.
type Layout struct {
	Title     string
	User      string
	Role      string
	CSRFToken string
	Crumbs    []Crumb
}

// LoginPage is the login form.
type LoginPage struct {
	CSRFToken string
	Next      string
	Error     string
}

// Action is one action the home page offers the user. Href, when set, is
// the UI page for it; Method and Path name the route that decided whether it
// is shown and are not displayed.
type Action struct {
	ID     string
	Label  string
	Method string
	Path   string
	Href   string
}

// Destination is a page the home page links to as a button.
type Destination struct {
	Label, Href, Icon string
}

// HomePage is the signed-in landing page.
type HomePage struct {
	User         string
	Role         string
	CSRFToken    string
	Actions      []Action
	Destinations []Destination
}

// csrfHeaders is the hx-headers JSON carrying the CSRF token.
func csrfHeaders(token string) string {
	b, _ := json.Marshal(map[string]string{"X-CSRF-Token": token}) // a map of strings always marshals
	return string(b)
}

// ApprovalsPage is the checker's list of proposals awaiting a decision
// (CC-1004).
type ApprovalsPage struct {
	User      string
	Role      string
	CSRFToken string
	Rows      []ApprovalRow
	Error     string
	// Posted, when set, is the proposal just posted.
	Posted *PostedNotice
}

// PostedNotice names a just-posted proposal and its ERPNext entry.
type PostedNotice struct {
	ProposalID string
	ERPName    string
	ERPLink    string // empty when not linkable
	ID         string // the full proposal id, for the link to its page (UI-09)
}

// ApprovalRow is one pending proposal. Own, CanApprove and CanReject only
// decide which controls are shown; the server enforces the rules.
type ApprovalRow struct {
	ID, ShortID, Company, PostingDate string
	Remark, Total, Maker, Status      string
	StatusNote, FindingID, Created    string
	Lines                             []ProposalLine
	Own                               bool
	CanApprove                        bool
	CanReject                         bool
	// TotalCredit, Balanced and Difference are the entry's totals line and
	// balance notice (UI-09); Total is the debit total.
	TotalCredit string
	Balanced    bool
	Difference  string // empty when balanced
}

// ProposalPage is one journal proposal and who did what to it (UI-09).
// Row carries the decision controls exactly as approvalRow computes them;
// ShowControls is false unless the viewer is a checker and the proposal is
// still awaiting a decision. The server enforces every rule.
type ProposalPage struct {
	User         string
	Role         string
	CSRFToken    string
	Row          ApprovalRow
	Entry        ProposalView
	Checker      string // empty until decided
	Timeline     []TimelineEntry
	ShowControls bool
	BackHref     string // empty for no back link
	BackLabel    string
}

func approveURL(id string) string { return "/proposals/" + id + "/approve" }

func rejectURL(id string) string { return "/proposals/" + id + "/reject" }

func proposalsURL(findingID string) string { return "/findings/" + findingID + "/proposals" }
