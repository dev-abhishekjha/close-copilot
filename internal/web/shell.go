package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// The app shell (UI-03): the sidebar links, the company and month context
// and the POST /context route. Hiding a link is a convenience: the route
// table and its middleware stay the only access control, and the nav asks
// them through s.allows and nothing else.

// navEntry is one sidebar link. path is a GET route pattern; the entry is
// shown only when the route table lets the role GET it, so a page whose
// route does not exist yet stays hidden until its ticket adds the route.
type navEntry struct {
	group, label, icon, path string
}

var navTable = []navEntry{
	// Dashboard: allows(role, "GET", "/") matches the catch-all "GET /" row,
	// not "GET /{$}". Both are anyRole today; UI-04 must keep them in step.
	{"Work", "Dashboard", "home", "/"},
	{"Work", "Close runs", "play", "/runs"},
	{"Work", "Findings", "alert", "/findings"},
	{"Work", "Ask", "message", "/ask"},
	{"Review", "Approvals", "check-circle", "/approvals"},
	{"Review", "Proposals", "file-text", "/proposals"},
	{"Reference", "Documents", "book", "/documents"},
	{"Reference", "Audit log", "shield", "/audit"},
	{"Reference", "System", "server", "/system"},
}

// Context cookie and company cache limits.
const (
	ctxCookieName      = "cc_ctx"
	ctxCookieMaxAge    = 365 * 24 * 3600
	maxCtxCookieLen    = 512
	companyCacheMaxAge = 30 * time.Second
)

// navFor returns the sidebar groups role may follow, in table order, with
// the item for path marked current. Groups with no visible item are left out.
func (s *Server) navFor(role Role, path string) []templates.NavGroup {
	var visible []navEntry
	for _, e := range navTable {
		if s.allows(role, http.MethodGet, e.path) {
			visible = append(visible, e)
		}
	}
	current := currentNav(visible, path)
	var groups []templates.NavGroup
	for _, e := range visible {
		if len(groups) == 0 || groups[len(groups)-1].Title != e.group {
			groups = append(groups, templates.NavGroup{Title: e.group})
		}
		g := &groups[len(groups)-1]
		g.Items = append(g.Items, templates.NavItem{Label: e.label, Href: e.path, Icon: e.icon, Current: e.path == current})
	}
	return groups
}

// currentNav returns the path of the entry for the request path: an exact
// match, else the longest entry that is a prefix at a "/" boundary. "/"
// matches only "/".
func currentNav(entries []navEntry, path string) string {
	best := ""
	for _, e := range entries {
		switch {
		case e.path == path:
			return e.path
		case e.path != "/" && strings.HasPrefix(path, e.path+"/") && len(e.path) > len(best):
			best = e.path
		}
	}
	return best
}

// shellData builds the frame's data for a signed-in request. Pages rendered
// from a non-GET request get "/" as the context form's next, so it never
// points at a POST-only URL.
func (s *Server) shellData(r *http.Request, p *principal) templates.ShellData {
	companies := s.shellCompanies(r.Context())
	company, month := s.resolveContext(r, companies)
	path := "/"
	if r.Method == http.MethodGet {
		path = r.URL.EscapedPath()
	}
	return templates.ShellData{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		Nav:       s.navFor(p.user.Role, r.URL.Path),
		Companies: companies, Company: company, Month: month,
		Path: path,
	}
}

// companyCache holds the company ids the shell offers, for
// companyCacheMaxAge. It only ever holds ids read from the store.
type companyCache struct {
	mu      sync.Mutex
	ids     []string
	fetched time.Time
	tried   bool
}

// shellCompanies returns the company ids, from the cache when it is fresh.
// A failed refresh keeps the last list (none if there never was one), logs
// once, and is retried after companyCacheMaxAge. Nil when the runs backend is
// not configured.
func (s *Server) shellCompanies(ctx context.Context) []string {
	if s.runs == nil {
		return nil
	}
	c := &s.companies
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.tried && !now.Before(c.fetched) && now.Sub(c.fetched) < companyCacheMaxAge {
		return slices.Clone(c.ids)
	}
	c.tried, c.fetched = true, now
	cs, err := s.runs.b.Store.ListCompanies(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "web: list companies for the shell", "err", err, "stale", len(c.ids))
		return slices.Clone(c.ids)
	}
	ids := make([]string, 0, len(cs))
	for _, x := range cs {
		ids = append(ids, x.ID)
	}
	c.ids = ids
	return slices.Clone(ids)
}

// contextFor returns the validated company and month for r: the cc_ctx
// cookie's values when they check out, else the defaults.
func (s *Server) contextFor(r *http.Request) (company, month string) {
	return s.resolveContext(r, s.shellCompanies(r.Context()))
}

// resolveContext reads the cc_ctx cookie. It is a preference, not a
// credential: it is not signed and every read revalidates it. The company
// is used only when it is one of companies and the month only when it is a
// YYYY-MM; each falls back on its own to the first company (empty when
// there is none) and the previous calendar month in UTC.
func (s *Server) resolveContext(r *http.Request, companies []string) (company, month string) {
	var q url.Values
	if ck, err := r.Cookie(ctxCookieName); err == nil && len(ck.Value) <= maxCtxCookieLen {
		if v, err := url.ParseQuery(ck.Value); err == nil {
			q = v
		}
	}
	company, month = q.Get("company"), q.Get("month")
	if !validCompany(company, companies) {
		company = ""
		if len(companies) > 0 {
			company = companies[0]
		}
	}
	if !validMonth(month) {
		month = previousMonth(s.now())
	}
	return company, month
}

func validCompany(c string, companies []string) bool {
	return c != "" && len(c) <= maxCompanyLen && slices.Contains(companies, c)
}

func validMonth(m string) bool { return monthRE.MatchString(m) }

// previousMonth is the calendar month before now's, in UTC: a close runs
// after its month ends.
func previousMonth(now time.Time) string {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")
}

// handleContext is POST /context: it sets the company and month context
// cookie and sends the browser back to next. Invalid values are ignored, so
// the cookie keeps the last valid (or default) value; nothing is reflected.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	if principalFrom(r.Context()) == nil {
		unauthorized(w, r)
		return
	}
	companies := s.shellCompanies(r.Context())
	company, month := s.resolveContext(r, companies)
	if c := r.PostFormValue("company"); validCompany(c, companies) {
		company = c
	}
	if m := r.PostFormValue("month"); validMonth(m) {
		month = m
	}
	v := url.Values{"company": {company}, "month": {month}}.Encode()
	http.SetCookie(w, s.cookie(ctxCookieName, v, "/", ctxCookieMaxAge, time.Time{}))
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// homeDestinations are the pages the home page links to as buttons: the
// role's sidebar links other than the dashboard itself.
func (s *Server) homeDestinations(role Role) []templates.Destination {
	var out []templates.Destination
	for _, g := range s.navFor(role, "") {
		for _, it := range g.Items {
			if it.Href != "/" {
				out = append(out, templates.Destination{Label: it.Label, Href: it.Href, Icon: it.Icon})
			}
		}
	}
	return out
}
