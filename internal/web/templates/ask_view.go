package templates

import (
	"net/url"

	"github.com/a-h/templ"
)

// AskViolationView is one verification failure of an answer. Amount is
// already formatted, empty when the violation has no value.
type AskViolationView struct {
	Code, Field, Detail, Amount string
}

// AskCitationView is one cited document section (plain text; UI-11 links it).
type AskCitationView struct {
	DocID, Section string
}

// AskResult is what the Ask page shows below the form. Action is answer,
// refuse or clarify; any other value renders a generic line.
type AskResult struct {
	Action      string
	Text        string
	Verified    bool
	NeedsReview bool
	Violations  []AskViolationView
	Citations   []AskCitationView
	EvidenceIDs []string
	StepRefs    []string
}

// AskPage is the Ask page. Question, Company and Month echo the form after
// an error; templ escapes them. Error is an alert above the form.
type AskPage struct {
	User, Role, CSRFToken string
	Companies             []Option
	Company, Month        string
	Question              string
	Error                 string
	Result                *AskResult
	Suggestions           []string
	// NotConfigured shows the "Ask is not set up" empty state instead.
	NotConfigured bool
}

// askSuggestionURL links a suggested question to the prefilled page.
func askSuggestionURL(q string) templ.SafeURL {
	return templ.SafeURL("/ask?q=" + url.QueryEscape(q))
}

func askMonthPattern() string { return `\d{4}-(0[1-9]|1[0-2])` }
