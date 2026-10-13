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
// request through hx-headers.
type Layout struct {
	Title     string
	User      string
	Role      string
	CSRFToken string
}

// LoginPage is the login form.
type LoginPage struct {
	CSRFToken string
	Next      string
	Error     string
}

// Action is one action the home page offers the user.
type Action struct {
	ID     string
	Label  string
	Method string
	Path   string
}

// HomePage is the signed-in landing page.
type HomePage struct {
	User      string
	Role      string
	CSRFToken string
	Actions   []Action
}

// csrfHeaders is the hx-headers JSON carrying the CSRF token.
func csrfHeaders(token string) string {
	b, _ := json.Marshal(map[string]string{"X-CSRF-Token": token}) // a map of strings always marshals
	return string(b)
}
