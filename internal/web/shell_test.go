package web

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// uiJSTag is the one script tag the shell adds (UI-02's ui.js).
const uiJSTag = `<script src="/static/js/ui.js" defer></script>`

// htmxTagRE is the layout's pinned htmx tag.
var htmxTagRE = regexp.MustCompile(`<script src="/static/htmx\.min\.js" integrity="sha384-[A-Za-z0-9+/=]+" crossorigin="anonymous" defer></script>`)

// unexpectedScripts counts the <script tags in body besides at most one
// layout htmx tag and one ui.js tag.
func unexpectedScripts(body string) int {
	body = strings.Replace(body, uiJSTag, "", 1)
	if loc := htmxTagRE.FindStringIndex(body); loc != nil {
		body = body[:loc[0]] + body[loc[1]:]
	}
	return strings.Count(body, "<script")
}

// hrefRE matches link targets (not the icon sprite <use href>).
var hrefRE = regexp.MustCompile(`<a [^>]*href="([^"]*)"`)

func hrefsIn(s string) []string {
	var out []string
	for _, m := range hrefRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// between returns body from the first start through the first end after it.
func between(t testing.TB, body, start, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("%q not found in body", start)
	}
	j := strings.Index(body[i:], end)
	if j < 0 {
		t.Fatalf("%q not closed by %q", start, end)
	}
	return body[i : i+j+len(end)]
}

// mainNav is the <nav aria-label="Main"> element only.
func mainNav(t testing.TB, body string) string {
	t.Helper()
	return between(t, body, `<nav class="shell-nav" aria-label="Main">`, "</nav>")
}

// mainContent is the <main id="main"> element only.
func mainContent(t testing.TB, body string) string {
	t.Helper()
	return between(t, body, `<main id="main"`, "</main>")
}

// shellForm is the top bar's context form only.
func shellForm(t testing.TB, body string) string {
	t.Helper()
	return between(t, body, `<form method="post" action="/context"`, "</form>")
}

func okBody(t testing.TB, ts *testServer, rq req) string {
	t.Helper()
	w := ts.do(t, rq)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s = %d", rq.method, rq.path, w.Code)
	}
	return w.Body.String()
}

// countingRunsStore counts ListCompanies calls and can fail them.
type countingRunsStore struct {
	*fakeRunsStore
	mu    sync.Mutex
	calls int
	err   error
}

func (c *countingRunsStore) ListCompanies(ctx context.Context) ([]store.Company, error) {
	c.mu.Lock()
	c.calls++
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.fakeRunsStore.ListCompanies(ctx)
}

func (c *countingRunsStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingRunsStore) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func noopExec(context.Context, uuid.UUID) error { return nil }

func TestShellNavByRole(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		role Role
		want []string
	}{
		{RoleViewer, []string{"/", "/runs"}},
		{RoleMaker, []string{"/", "/runs"}},
		{RoleChecker, []string{"/", "/runs", "/approvals", "/audit"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			var derived []string
			for _, e := range navTable {
				if ts.allows(tt.role, http.MethodGet, e.path) {
					derived = append(derived, e.path)
				}
			}
			if !slices.Equal(derived, tt.want) {
				t.Errorf("allows over the nav table = %v, want %v", derived, tt.want)
			}
			ck, _ := ts.session(tt.role)
			nav := mainNav(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
			if got := hrefsIn(nav); !slices.Equal(got, tt.want) {
				t.Errorf("nav hrefs = %v, want %v", got, tt.want)
			}
			if tt.role != RoleChecker {
				for _, deny := range []string{`href="/approvals"`, `href="/audit"`} {
					if strings.Contains(nav, deny) {
						t.Errorf("%s nav shows %s", tt.role, deny)
					}
				}
			}
			// No link the role can see answers 403.
			for _, h := range tt.want {
				if w := ts.do(t, req{method: http.MethodGet, path: h, cookies: []*http.Cookie{ck}}); w.Code == http.StatusForbidden {
					t.Errorf("%s sees %s, which answers 403", tt.role, h)
				}
			}
		})
	}
}

func TestShellNavOmitsUnroutedPages(t *testing.T) {
	unrouted := []string{"/findings", "/ask", "/proposals", "/documents", "/system"}
	ts := newTestServer(t)
	ck, _ := ts.session(RoleChecker)
	nav := mainNav(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
	for _, p := range unrouted {
		if strings.Contains(nav, `href="`+p+`"`) {
			t.Errorf("nav shows unrouted %s", p)
		}
	}
	// A route added to a copy of the table makes its item appear, for the
	// roles it allows only; the group appears with it.
	ts.routes = append(slices.Clone(ts.routes),
		route{http.MethodGet, "/findings", anyRole, http.NotFoundHandler()},
		route{http.MethodGet, "/system", checkerOnly, http.NotFoundHandler()},
	)
	for _, tt := range []struct {
		role       Role
		want, deny []string
	}{
		{RoleViewer, []string{"/findings"}, []string{"/system", "/ask", "/proposals", "/documents"}},
		{RoleChecker, []string{"/findings", "/system"}, []string{"/ask", "/proposals", "/documents"}},
	} {
		ck, _ := ts.session(tt.role)
		nav := mainNav(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
		for _, p := range tt.want {
			if !strings.Contains(nav, `href="`+p+`"`) {
				t.Errorf("%s: routed %s not in nav", tt.role, p)
			}
		}
		for _, p := range tt.deny {
			if strings.Contains(nav, `href="`+p+`"`) {
				t.Errorf("%s: nav shows %s", tt.role, p)
			}
		}
	}
	// A group with no visible item is not drawn.
	if groups := ts.navFor(RoleViewer, "/"); slices.ContainsFunc(groups, func(g templates.NavGroup) bool { return g.Title == "Review" }) {
		t.Errorf("viewer nav has the empty Review group: %+v", groups)
	}
}

func TestShellActivePage(t *testing.T) {
	fs := newFakeRunsStore()
	rid := uuid.New()
	fs.runs[rid] = store.CloseRun{ID: rid, CompanyID: "acme", Month: "2026-09", Status: store.RunDone}
	ts := runsServer(t, fs, nil)
	tests := []struct {
		role       Role
		path, want string
	}{
		{RoleViewer, "/", "/"},
		{RoleViewer, "/runs", "/runs"},
		{RoleMaker, "/runs/" + rid.String(), "/runs"},
		{RoleViewer, "/runs/" + rid.String() + "/findings", "/runs"},
		{RoleChecker, "/runs", "/runs"},
		{RoleChecker, "/", "/"},
	}
	reCurrent := regexp.MustCompile(`<a class="shell-link" href="([^"]*)" aria-current="page">`)
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			ck, _ := ts.session(tt.role)
			w := ts.do(t, req{method: http.MethodGet, path: tt.path, cookies: []*http.Cookie{ck}})
			body := w.Body.String()
			if n := strings.Count(body, `aria-current="page"`); n != 1 {
				t.Fatalf("aria-current appears %d times, want 1 (status %d)", n, w.Code)
			}
			m := reCurrent.FindStringSubmatch(mainNav(t, body))
			if m == nil || m[1] != tt.want {
				t.Errorf("current item = %v, want %s", m, tt.want)
			}
		})
	}
	// The matcher itself: "/" only for "/", prefixes at a "/" boundary.
	for path, want := range map[string]string{
		"/": "/", "/runs": "/runs", "/runs/abc": "/runs", "/runs/abc/findings": "/runs",
		"/runsx": "", "/approvals/x": "/approvals", "/other": "", "": "",
	} {
		if got := currentNav(navTable, path); got != want {
			t.Errorf("currentNav(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestShellLandmarks(t *testing.T) {
	ts := newTestServer(t)
	ck, csrf := ts.session(RoleViewer)
	body := okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})

	i := strings.Index(body, "<body")
	if i < 0 {
		t.Fatal("no <body")
	}
	afterBody := body[i+strings.Index(body[i:], ">")+1:]
	if !strings.HasPrefix(strings.TrimSpace(afterBody), `<a class="skip-link" href="#main">Skip to content</a>`) {
		t.Errorf("skip link is not first in the body: %.120s", afterBody)
	}
	esc := html.EscapeString(csrf)
	for _, want := range []string{
		`<nav class="shell-nav" aria-label="Main">`,
		`<main id="main" tabindex="-1">`,
		`<span class="user">demo</span>`,
		`<span class="role" data-role="viewer">viewer</span>`,
		`class="brand"`,
		`<form method="post" action="/logout" class="inline"><input type="hidden" name="csrf_token" value="` + esc + `">`,
		uiJSTag,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
	if form := shellForm(t, body); !strings.Contains(form, `name="csrf_token" value="`+esc+`"`) || !strings.Contains(form, `name="next" value="/"`) {
		t.Errorf("context form lacks the CSRF token or next: %s", form)
	}
	if n := strings.Count(body, `aria-label="Main"`); n != 1 {
		t.Errorf("%d Main nav landmarks, want 1", n)
	}
	if n := strings.Count(body, "<main"); n != 1 {
		t.Errorf("%d <main elements, want 1", n)
	}
	if unexpectedScripts(body) != 0 {
		t.Error("unexpected <script tags")
	}
	for _, bad := range []string{"style=", "<style", " onclick="} {
		if strings.Contains(body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
	// The skip link comes before the nav, and the nav before <main>.
	if a, b, c := strings.Index(body, "skip-link"), strings.Index(body, `aria-label="Main"`), strings.Index(body, `<main id="main"`); a >= b || b >= c {
		t.Errorf("focus order: skip %d, nav %d, main %d", a, b, c)
	}
}

func TestShellLoginHasNoShell(t *testing.T) {
	ts := runsServer(t, newFakeRunsStore(), nil)
	body := okBody(t, ts, req{method: http.MethodGet, path: "/login"})
	for _, bad := range []string{"shell-nav", `aria-label="Main"`, "ui.js", `action="/context"`, "skip-link", "cc_ctx"} {
		if strings.Contains(body, bad) {
			t.Errorf("login page contains %q", bad)
		}
	}
	if !htmxTagRE.MatchString(body) || !strings.Contains(body, `<header class="topbar">`) || !strings.Contains(body, "<main>") {
		t.Error("login page lost the plain frame")
	}
	if strings.Count(body, "<script") != 1 {
		t.Errorf("login page has %d <script tags, want 1", strings.Count(body, "<script"))
	}
}

func TestShellContextDefault(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{"october", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), "2026-09"},
		{"january", time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC), "2026-12"},
		{"month end", time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC), "2026-02"},
		// 1 March 01:00 IST is still 28 February in UTC.
		{"utc not local", time.Date(2026, 3, 1, 1, 0, 0, 0, ist), "2026-01"},
	}
	ts := runsServer(t, newFakeRunsStore(), nil)
	bare := newTestServer(t) // no runs backend
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts.clock.Set(tt.now)
			bare.clock.Set(tt.now)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c, m := ts.contextFor(r); c != "acme" || m != tt.want {
				t.Errorf("contextFor = %q %q, want acme %s", c, m, tt.want)
			}
			if c, m := bare.contextFor(r); c != "" || m != tt.want {
				t.Errorf("no backend: contextFor = %q %q, want \"\" %s", c, m, tt.want)
			}
			ck, _ := ts.session(RoleViewer)
			form := shellForm(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
			for _, want := range []string{`<option value="acme" selected>`, `<option value="globex">`, `type="month" name="month" value="` + tt.want + `"`} {
				if !strings.Contains(form, want) {
					t.Errorf("context form lacks %s: %s", want, form)
				}
			}
			ck, _ = bare.session(RoleViewer)
			form = shellForm(t, okBody(t, bare, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
			if strings.Contains(form, "<select") || !strings.Contains(form, `value="`+tt.want+`"`) {
				t.Errorf("no backend: context form = %s", form)
			}
		})
	}
}

func TestShellContextCookieForged(t *testing.T) {
	long := strings.Repeat("c", maxCompanyLen+1)
	fs := newFakeRunsStore()
	fs.companies = append(fs.companies, store.Company{ID: long}, store.Company{ID: "a&b"})
	ts := runsServer(t, fs, nil)
	enc := func(c, m string) string { return url.Values{"company": {c}, "month": {m}}.Encode() }
	tests := []struct {
		name, value    string
		company, month string
		noReflect      string
	}{
		{"valid", enc("globex", "2026-08"), "globex", "2026-08", ""},
		{"escaped company id", enc("a&b", "2026-08"), "a&b", "2026-08", ""},
		{"unknown company", enc("evilcorp", "2026-08"), "acme", "2026-08", "evilcorp"},
		{"bad month", enc("globex", "2026-13"), "globex", "2026-09", "2026-13"},
		{"month not padded", enc("globex", "2026-8"), "globex", "2026-09", ""},
		{"script company", enc("<script>alert(1)</script>", "2026-08"), "acme", "2026-08", "alert(1)"},
		{"script month raw", "company=globex&month=<script>alert(1)</script>", "globex", "2026-09", "alert(1)"},
		{"company too long", enc(long, "2026-08"), "acme", "2026-08", ""},
		{"oversized cookie", enc("globex", "2026-08") + "&pad=" + strings.Repeat("x", maxCtxCookieLen), "acme", "2026-09", ""},
		{"malformed escape", "company=globex%zz&month=2026-08", "acme", "2026-09", ""},
		{"empty", "", "acme", "2026-09", ""},
		{"garbage", "!!!", "acme", "2026-09", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.AddCookie(&http.Cookie{Name: ctxCookieName, Value: tt.value})
			if c, m := ts.contextFor(r); c != tt.company || m != tt.month {
				t.Errorf("contextFor = %q %q, want %q %q", c, m, tt.company, tt.month)
			}
			ck, _ := ts.session(RoleMaker)
			for _, p := range []string{"/", "/runs"} {
				body := okBody(t, ts, req{method: http.MethodGet, path: p, cookies: []*http.Cookie{ck, {Name: ctxCookieName, Value: tt.value}}})
				form := shellForm(t, body)
				if !strings.Contains(form, `<option value="`+html.EscapeString(tt.company)+`" selected>`) ||
					!strings.Contains(form, `name="month" value="`+tt.month+`"`) {
					t.Errorf("%s: context form = %s", p, form)
				}
				if strings.Contains(body, "<script>") || unexpectedScripts(body) != 0 {
					t.Errorf("%s: raw <script> in page", p)
				}
				if tt.noReflect != "" && strings.Contains(body, tt.noReflect) {
					t.Errorf("%s: forged value %q reflected", p, tt.noReflect)
				}
			}
		})
	}
}

func TestShellContextSwitch(t *testing.T) {
	post := func(ts *testServer, fields url.Values, extra ...*http.Cookie) *httptest.ResponseRecorder {
		rq := formPost(ts, RoleViewer, "/context", fields)
		rq.cookies = append(rq.cookies, extra...)
		return ts.do(t, rq)
	}
	ctxValue := func(t *testing.T, w *httptest.ResponseRecorder) url.Values {
		t.Helper()
		c := responseCookie(t, w, ctxCookieName)
		if c == nil {
			t.Fatal("no cc_ctx cookie set")
		}
		v, err := url.ParseQuery(c.Value)
		if err != nil {
			t.Fatalf("cookie value %q: %v", c.Value, err)
		}
		return v
	}

	t.Run("valid", func(t *testing.T) {
		for _, insecure := range []bool{false, true} {
			ts := newTestServer(t, func(o *Options) {
				o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: noopExec}
				o.InsecureCookies = insecure
			})
			w := post(ts, url.Values{"company": {"globex"}, "month": {"2026-07"}, "next": {"/runs"}})
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/runs" {
				t.Fatalf("POST /context = %d %q, want 303 /runs", w.Code, w.Header().Get("Location"))
			}
			c := responseCookie(t, w, ctxCookieName)
			if c == nil {
				t.Fatal("no cookie")
			}
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure == insecure || c.MaxAge != 365*24*3600 {
				t.Errorf("cookie flags = HttpOnly %v SameSite %v Path %q Secure %v MaxAge %d (insecure %v)",
					c.HttpOnly, c.SameSite, c.Path, c.Secure, c.MaxAge, insecure)
			}
			if v := ctxValue(t, w); v.Get("company") != "globex" || v.Get("month") != "2026-07" {
				t.Errorf("cookie = %v", v)
			}
			// The next page shows the new selection.
			ck, _ := ts.session(RoleViewer)
			form := shellForm(t, okBody(t, ts, req{method: http.MethodGet, path: "/runs", cookies: []*http.Cookie{ck, {Name: c.Name, Value: c.Value}}}))
			for _, want := range []string{`<option value="globex" selected>`, `name="month" value="2026-07"`, `name="next" value="/runs"`} {
				if !strings.Contains(form, want) {
					t.Errorf("after switch, form lacks %s: %s", want, form)
				}
			}
		}
	})

	ts := runsServer(t, newFakeRunsStore(), nil)

	t.Run("external next", func(t *testing.T) {
		for _, next := range []string{"//evil.example", "https://evil.example", `/\evil`, "/%2F/evil.example", "javascript:alert(1)", ""} {
			w := post(ts, url.Values{"company": {"globex"}, "month": {"2026-07"}, "next": {next}})
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
				t.Errorf("next %q: %d %q, want 303 /", next, w.Code, w.Header().Get("Location"))
			}
		}
	})

	t.Run("invalid values keep the last valid", func(t *testing.T) {
		prev := &http.Cookie{Name: ctxCookieName, Value: url.Values{"company": {"globex"}, "month": {"2026-07"}}.Encode()}
		for _, f := range []url.Values{
			{"company": {"evilcorp"}, "month": {"<script>"}, "next": {"/"}},
			{"company": {strings.Repeat("g", maxCompanyLen+1)}, "month": {"2026-13"}},
			{},
		} {
			w := post(ts, f, prev)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("POST /context = %d", w.Code)
			}
			if v := ctxValue(t, w); v.Get("company") != "globex" || v.Get("month") != "2026-07" {
				t.Errorf("fields %v: cookie = %v, want the previous globex 2026-07", f, v)
			}
			if b := w.Body.String() + w.Header().Get("Location"); strings.Contains(b, "evilcorp") || strings.Contains(b, "<script>") {
				t.Errorf("invalid value reflected: %q", b)
			}
		}
		// Without a previous cookie the defaults are kept.
		w := post(ts, url.Values{"company": {"evilcorp"}, "month": {"nope"}})
		if v := ctxValue(t, w); v.Get("company") != "acme" || v.Get("month") != "2026-09" {
			t.Errorf("cookie = %v, want the defaults acme 2026-09", v)
		}
		// One valid field is taken on its own.
		w = post(ts, url.Values{"company": {"evilcorp"}, "month": {"2026-05"}}, prev)
		if v := ctxValue(t, w); v.Get("company") != "globex" || v.Get("month") != "2026-05" {
			t.Errorf("cookie = %v, want globex 2026-05", v)
		}
	})

	t.Run("no csrf", func(t *testing.T) {
		ck, _ := ts.session(RoleViewer)
		for _, f := range []url.Values{
			{"company": {"globex"}, "month": {"2026-07"}},
			{"company": {"globex"}, "month": {"2026-07"}, "csrf_token": {"wrong"}},
		} {
			w := ts.do(t, req{method: http.MethodPost, path: "/context", body: f.Encode(),
				contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}})
			if w.Code != http.StatusForbidden {
				t.Errorf("POST /context without a valid CSRF token = %d, want 403", w.Code)
			}
			if responseCookie(t, w, ctxCookieName) != nil {
				t.Error("cookie set without a CSRF token")
			}
		}
		// Cross-site, even with the token.
		rq := formPost(ts, RoleViewer, "/context", url.Values{"company": {"globex"}})
		rq.header = map[string]string{"Sec-Fetch-Site": "cross-site"}
		if w := ts.do(t, rq); w.Code != http.StatusForbidden || responseCookie(t, w, ctxCookieName) != nil {
			t.Errorf("cross-site POST /context = %d", w.Code)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		f := url.Values{"company": {"globex"}, "month": {"2026-07"}}.Encode()
		w := ts.do(t, req{method: http.MethodPost, path: "/context", body: f, contentType: "application/x-www-form-urlencoded"})
		want := ts.do(t, req{method: http.MethodPost, path: "/logout", body: f, contentType: "application/x-www-form-urlencoded"})
		if w.Code != want.Code || w.Header().Get("Location") != want.Header().Get("Location") {
			t.Errorf("anonymous POST /context = %d %q, want %d %q like other POSTs",
				w.Code, w.Header().Get("Location"), want.Code, want.Header().Get("Location"))
		}
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
			t.Errorf("anonymous POST /context = %d %q, want 303 to /login", w.Code, w.Header().Get("Location"))
		}
		if responseCookie(t, w, ctxCookieName) != nil {
			t.Error("cookie set for an anonymous request")
		}
	})

	t.Run("next is never a POST url", func(t *testing.T) {
		w := ts.do(t, formPost(ts, RoleMaker, "/runs", url.Values{"company": {"acme"}, "month": {"bad"}}))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST /runs = %d", w.Code)
		}
		if form := shellForm(t, w.Body.String()); !strings.Contains(form, `name="next" value="/"`) {
			t.Errorf("POST-rendered page: next is not /: %s", form)
		}
	})
}

func TestShellCompaniesCached(t *testing.T) {
	cs := &countingRunsStore{fakeRunsStore: newFakeRunsStore()}
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: cs, Execute: noopExec} })
	ck, _ := ts.session(RoleViewer)
	view := func() string {
		return okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	}
	start := ts.clock.Now()

	view()
	ts.clock.Set(start.Add(29 * time.Second))
	view()
	if n := cs.count(); n != 1 {
		t.Fatalf("two views within 30s: %d ListCompanies calls, want 1", n)
	}
	ts.clock.Set(start.Add(31 * time.Second))
	view()
	if n := cs.count(); n != 2 {
		t.Fatalf("after 30s: %d calls, want 2", n)
	}

	cs.fail(errors.New("db down"))
	ts.clock.Set(start.Add(62 * time.Second))
	body := view()
	if n := cs.count(); n != 3 {
		t.Fatalf("after the next 30s: %d calls, want 3", n)
	}
	if form := shellForm(t, body); !strings.Contains(form, `<option value="acme" selected>`) || !strings.Contains(form, `<option value="globex">`) {
		t.Errorf("error after a load: stale list not served: %s", form)
	}
	ts.clock.Set(start.Add(66 * time.Second))
	view()
	if n := cs.count(); n != 3 {
		t.Errorf("a failed refresh is retried at once: %d calls, want 3", n)
	}
	ts.clock.Set(start.Add(67 * time.Second))
	view()
	if n := cs.count(); n != 4 {
		t.Errorf("a failed refresh is not retried after 5s: %d calls, want 4", n)
	}
	if n := strings.Count(ts.logs.String(), "list companies for the shell"); n != 1 {
		t.Errorf("failure logged %d times, want 1", n)
	}
	if strings.Contains(ts.logs.String(), "acme") {
		t.Error("log carries company ids")
	}

	// An error with nothing cached gives no list and still renders.
	cs2 := &countingRunsStore{fakeRunsStore: newFakeRunsStore(), err: errors.New("db down")}
	ts2 := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: cs2, Execute: noopExec} })
	ck2, _ := ts2.session(RoleViewer)
	form := shellForm(t, okBody(t, ts2, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck2}}))
	if strings.Contains(form, "<select") {
		t.Errorf("no list: form still has a company select: %s", form)
	}

	// The cache hands out copies.
	ids := ts.shellCompanies(context.Background())
	if len(ids) > 0 {
		ids[0] = "mutated"
	}
	if got := ts.shellCompanies(context.Background()); len(got) == 0 || got[0] != "acme" {
		t.Errorf("cache mutated through a returned slice: %v", got)
	}
}

// blockingRunsStore's ListCompanies waits until released (or its context
// ends) and records the context it was given.
type blockingRunsStore struct {
	*fakeRunsStore
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
	ctxErrs []error         // ctx.Err() seen after each release
	budgets []time.Duration // time left to each call's deadline (-1: none)
}

func newBlockingRunsStore() *blockingRunsStore {
	return &blockingRunsStore{fakeRunsStore: newFakeRunsStore(), entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingRunsStore) ListCompanies(ctx context.Context) ([]store.Company, error) {
	budget := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}
	b.mu.Lock()
	b.calls++
	b.budgets = append(b.budgets, budget)
	b.mu.Unlock()
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	b.mu.Lock()
	b.ctxErrs = append(b.ctxErrs, ctx.Err())
	b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.fakeRunsStore.ListCompanies(ctx)
}

func (b *blockingRunsStore) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// releaseStore lets one blocked ListCompanies return; it fails instead of
// hanging if nothing is waiting.
func releaseStore(t *testing.T, b *blockingRunsStore) {
	t.Helper()
	select {
	case b.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no ListCompanies call waiting to be released")
	}
}

func waitEntered(t *testing.T, b *blockingRunsStore) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ListCompanies was not called")
	}
}

// within runs f and fails if it does not return within d.
func within[T any](t *testing.T, d time.Duration, what string, f func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- f() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s blocked for more than %s", what, d)
		var zero T
		return zero
	}
}

func TestShellCompaniesRefreshDoesNotBlock(t *testing.T) {
	bs := newBlockingRunsStore()
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: bs, Execute: noopExec} })
	ts.companies.timeout = 30 * time.Second // a slow runner must not time the fetch out
	ck, _ := ts.session(RoleViewer)
	view := func() *httptest.ResponseRecorder {
		return ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	}

	for _, phase := range []struct {
		name      string
		advance   time.Duration
		wantStale bool
	}{
		{"cold", 0, false},
		{"stale", 31 * time.Second, true},
	} {
		t.Run(phase.name, func(t *testing.T) {
			ts.clock.Set(ts.clock.Now().Add(phase.advance))
			first := make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- view() }()
			waitEntered(t, bs)

			// A second render while the refresh hangs answers at once.
			w := within(t, time.Second, "a render during a refresh", view)
			if w.Code != http.StatusOK {
				t.Fatalf("second render = %d", w.Code)
			}
			form := shellForm(t, w.Body.String())
			if got := strings.Contains(form, `<option value="acme"`); got != phase.wantStale {
				t.Errorf("second render shows the list = %v, want %v: %s", got, phase.wantStale, form)
			}
			if n, want := bs.count(), map[bool]int{false: 1, true: 2}[phase.wantStale]; n != want {
				t.Errorf("%d ListCompanies calls; a waiting render started another", n)
			}

			// The refresh completes once the store answers, and is used.
			releaseStore(t, bs)
			fw := <-first
			if !strings.Contains(shellForm(t, fw.Body.String()), `<option value="acme" selected>`) {
				t.Errorf("refreshing render lacks the list: %s", shellForm(t, fw.Body.String()))
			}
			after := within(t, time.Second, "a render after the refresh", view)
			if !strings.Contains(shellForm(t, after.Body.String()), `<option value="acme" selected>`) {
				t.Errorf("render after the refresh lacks the list")
			}
		})
	}
	if n := bs.count(); n != 2 {
		t.Errorf("%d ListCompanies calls, want 2", n)
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for i, b := range bs.budgets {
		if b <= 0 || b > ts.companies.fetchTimeout() {
			t.Errorf("call %d: deadline in %s, want within %s", i, b, ts.companies.fetchTimeout())
		}
	}
}

func TestShellCompaniesCancelledRequest(t *testing.T) {
	bs := newBlockingRunsStore()
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: bs, Execute: noopExec} })
	ts.companies.timeout = 30 * time.Second // a slow runner must not time the fetch out

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan []string, 1)
	go func() { got <- ts.shellCompanies(ctx) }()
	waitEntered(t, bs)
	cancel() // the user leaves mid-render
	releaseStore(t, bs)
	if ids := <-got; !slices.Equal(ids, []string{"acme", "globex"}) {
		t.Errorf("refresh for a cancelled request = %v, want the list", ids)
	}
	bs.mu.Lock()
	if bs.ctxErrs[0] != nil {
		t.Errorf("the store saw the request's cancellation: %v", bs.ctxErrs[0])
	}
	bs.mu.Unlock()

	// A request already cancelled when the refresh is due still refreshes,
	// and the next due refresh runs too.
	ts.clock.Set(ts.clock.Now().Add(31 * time.Second))
	got2 := make(chan []string, 1)
	go func() { got2 <- ts.shellCompanies(ctx) }()
	waitEntered(t, bs)
	releaseStore(t, bs)
	if ids := <-got2; !slices.Equal(ids, []string{"acme", "globex"}) {
		t.Errorf("refresh from a cancelled ctx = %v", ids)
	}
	ts.clock.Set(ts.clock.Now().Add(31 * time.Second))
	go func() { got2 <- ts.shellCompanies(context.Background()) }()
	waitEntered(t, bs)
	releaseStore(t, bs)
	<-got2
	if n := bs.count(); n != 3 {
		t.Errorf("%d refreshes, want 3: a cancelled request stopped refreshing", n)
	}
	if strings.Contains(ts.logs.String(), "list companies for the shell") {
		t.Errorf("a cancelled request logged a failure: %s", ts.logs.String())
	}
}

func TestShellCompaniesRetryAfterError(t *testing.T) {
	cs := &countingRunsStore{fakeRunsStore: newFakeRunsStore(), err: errors.New("db down")}
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: cs, Execute: noopExec} })
	start := ts.clock.Now()
	at := func(d time.Duration) []string {
		ts.clock.Set(start.Add(d))
		return ts.shellCompanies(context.Background())
	}
	logs := func() int { return strings.Count(ts.logs.String(), "list companies for the shell") }

	if ids := at(0); len(ids) != 0 || cs.count() != 1 {
		t.Fatalf("cold failure: %v, %d calls", ids, cs.count())
	}
	if at(4 * time.Second); cs.count() != 1 {
		t.Errorf("retried before 5s: %d calls", cs.count())
	}
	if at(5 * time.Second); cs.count() != 2 {
		t.Errorf("not retried after 5s: %d calls, want 2", cs.count())
	}
	if n := logs(); n != 1 {
		t.Errorf("one failure episode logged %d times, want 1", n)
	}
	cs.fail(nil)
	if ids := at(10 * time.Second); !slices.Equal(ids, []string{"acme", "globex"}) || cs.count() != 3 {
		t.Errorf("recovery: %v, %d calls", ids, cs.count())
	}
	// After a success the interval is 30s again.
	if at(39 * time.Second); cs.count() != 3 {
		t.Errorf("refreshed before 30s after a success: %d calls", cs.count())
	}
	// A new failure is a new episode: logged again, stale list served.
	cs.fail(errors.New("db down again"))
	if ids := at(40 * time.Second); !slices.Equal(ids, []string{"acme", "globex"}) || cs.count() != 4 {
		t.Errorf("new failure: %v, %d calls", ids, cs.count())
	}
	if n := logs(); n != 2 {
		t.Errorf("two failure episodes logged %d times, want 2", n)
	}
	// A clock that goes backwards makes the cache due at once.
	cs.fail(nil)
	if at(41 * time.Second); cs.count() != 4 {
		t.Errorf("retried before 5s: %d calls", cs.count())
	}
	if at(20 * time.Second); cs.count() != 5 {
		t.Errorf("backwards clock: %d calls, want 5", cs.count())
	}
	if strings.Contains(ts.logs.String(), "acme") || strings.Contains(ts.logs.String(), "globex") {
		t.Error("log carries company ids")
	}
}

// deadlineRunsStore records the time left to each call's deadline.
type deadlineRunsStore struct {
	*fakeRunsStore
	mu      sync.Mutex
	budgets []time.Duration // -1: no deadline
}

func (d *deadlineRunsStore) ListCompanies(ctx context.Context) ([]store.Company, error) {
	budget := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}
	d.mu.Lock()
	d.budgets = append(d.budgets, budget)
	d.mu.Unlock()
	return d.fakeRunsStore.ListCompanies(ctx)
}

func TestShellCompaniesFetchDeadline(t *testing.T) {
	if companyFetchTimeout != 2*time.Second {
		t.Errorf("companyFetchTimeout = %s, want 2s", companyFetchTimeout)
	}
	ds := &deadlineRunsStore{fakeRunsStore: newFakeRunsStore()}
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: ds, Execute: noopExec} })
	if got := ts.companies.fetchTimeout(); got != companyFetchTimeout {
		t.Errorf("default fetch timeout = %s, want %s", got, companyFetchTimeout)
	}
	ck, _ := ts.session(RoleViewer)
	start := ts.clock.Now()
	for i := range 3 {
		ts.clock.Set(start.Add(time.Duration(i) * 31 * time.Second))
		okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.budgets) != 3 {
		t.Fatalf("%d store calls, want 3", len(ds.budgets))
	}
	for i, b := range ds.budgets {
		if b <= 0 || b > companyFetchTimeout {
			t.Errorf("call %d: deadline in %s, want within %s", i, b, companyFetchTimeout)
		}
	}
}

// panickingRunsStore panics on its first ListCompanies call only.
type panickingRunsStore struct {
	*fakeRunsStore
	mu    sync.Mutex
	calls int
}

func (p *panickingRunsStore) ListCompanies(ctx context.Context) ([]store.Company, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		panic("store exploded")
	}
	return p.fakeRunsStore.ListCompanies(ctx)
}

func TestShellCompaniesPanic(t *testing.T) {
	ps := &panickingRunsStore{fakeRunsStore: newFakeRunsStore()}
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: ps, Execute: noopExec} })
	call := func() (ids []string, panicked any) {
		defer func() { panicked = recover() }()
		return ts.shellCompanies(context.Background()), nil
	}
	if _, p := call(); p != "store exploded" {
		t.Fatalf("panic not propagated: recovered %v", p)
	}
	// The in-flight flag was cleared and the panic counts as a failure: no
	// retry before 5s, a refresh once it is due.
	start := ts.clock.Now()
	ts.clock.Set(start.Add(4 * time.Second))
	if ids, p := call(); p != nil || len(ids) != 0 {
		t.Errorf("before the retry: %v %v", ids, p)
	}
	ts.clock.Set(start.Add(5 * time.Second))
	if ids, p := call(); p != nil || !slices.Equal(ids, []string{"acme", "globex"}) {
		t.Errorf("after the panic the next due call = %v %v, want the list", ids, p)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.calls != 2 {
		t.Errorf("%d store calls, want 2", ps.calls)
	}
}

func TestShellContextKeepsCompanyWhenListEmpty(t *testing.T) {
	cs := &countingRunsStore{fakeRunsStore: newFakeRunsStore(), err: errors.New("db down")}
	ts := newTestServer(t, func(o *Options) { o.Runs = &RunsBackend{Store: cs, Execute: noopExec} })
	cookie := func(c, m string) *http.Cookie {
		return &http.Cookie{Name: ctxCookieName, Value: url.Values{"company": {c}, "month": {m}}.Encode()}
	}
	post := func(fields url.Values, extra ...*http.Cookie) url.Values {
		t.Helper()
		rq := formPost(ts, RoleViewer, "/context", fields)
		rq.cookies = append(rq.cookies, extra...)
		w := ts.do(t, rq)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("POST /context = %d", w.Code)
		}
		c := responseCookie(t, w, ctxCookieName)
		if c == nil {
			t.Fatal("no cookie")
		}
		v, err := url.ParseQuery(c.Value)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// Cold cache, store failing: the stored company survives a month change,
	// and the posted company is not taken without a list to check it.
	v := post(url.Values{"company": {"evilcorp"}, "month": {"2026-03"}}, cookie("globex", "2026-07"))
	if v.Get("company") != "globex" || v.Get("month") != "2026-03" {
		t.Errorf("cold cache: cookie = %v, want globex 2026-03", v)
	}
	if v := post(url.Values{"company": {"evilcorp"}, "month": {"2026-03"}}); v.Get("company") != "" {
		t.Errorf("cold cache, no cookie: company = %q, want empty", v.Get("company"))
	}
	long := strings.Repeat("g", maxCompanyLen+1)
	if v := post(url.Values{"month": {"2026-03"}}, cookie(long, "2026-07")); v.Get("company") != "" {
		t.Errorf("cold cache: an over-long stored company was kept")
	}

	// With a list again, the posted unknown company is still dropped and the
	// stored one checked against the list.
	cs.fail(nil)
	ts.clock.Set(ts.clock.Now().Add(5 * time.Second))
	if v := post(url.Values{"company": {"evilcorp"}, "month": {"2026-04"}}, cookie("globex", "2026-07")); v.Get("company") != "globex" || v.Get("month") != "2026-04" {
		t.Errorf("with a list: cookie = %v, want globex 2026-04", v)
	}
	if v := post(url.Values{"company": {"evilcorp"}}, cookie("unknownco", "2026-07")); v.Get("company") != "acme" {
		t.Errorf("with a list: an unknown stored company = %q, want the default acme", v.Get("company"))
	}
}

func TestShellEscaping(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	const escaped = `&lt;script&gt;alert(1)&lt;/script&gt;`

	// Through the server: a company id from the store.
	fs := newFakeRunsStore()
	fs.companies = []store.Company{{ID: evil}, {ID: "acme"}}
	ts := runsServer(t, fs, nil)
	ck, _ := ts.session(RoleViewer)
	body := okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	if strings.Contains(body, evil) || !strings.Contains(shellForm(t, body), `<option value="`+escaped+`" selected>`+escaped+`</option>`) {
		t.Errorf("company id not escaped in the selector: %s", shellForm(t, body))
	}

	// Directly: every ShellData text in the sidebar, selector and top bar.
	sd := templates.ShellData{
		User: evil, Role: evil, CSRFToken: evil,
		Nav:       []templates.NavGroup{{Title: evil, Items: []templates.NavItem{{Label: evil, Href: "/runs", Icon: "play", Current: true}}}},
		Companies: []string{evil}, Company: evil, Month: evil, Path: evil,
	}
	var b strings.Builder
	l := templates.Layout{Title: evil, Crumbs: []templates.Crumb{{Label: evil, Href: "javascript:alert(1)"}, {Label: evil}}}
	if err := templates.Base(l).Render(templates.WithShell(context.Background(), sd), &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "<script>") || strings.Contains(out, `href="javascript:`) {
		t.Errorf("unescaped data in the frame: %s", out)
	}
	nav := mainNav(t, out)
	for where, s := range map[string]string{"sidebar": nav, "selector": shellForm(t, out), "top bar": between(t, out, `<span class="who">`, "</span>")} {
		if !strings.Contains(s, escaped) {
			t.Errorf("%s does not show the value escaped: %s", where, s)
		}
	}
}

func TestShellHomeLinks(t *testing.T) {
	ts := newTestServer(t)
	allActions := []string{"view-run", "view-findings", "ask", "start-run", "propose-entry", "approve-proposal", "reject-proposal", "approvals", "audit"}
	tests := []struct {
		role       Role
		want, deny []string
	}{
		{RoleViewer, []string{"/runs"}, []string{"/approvals", "/audit"}},
		{RoleMaker, []string{"/runs"}, []string{"/approvals", "/audit"}},
		{RoleChecker, []string{"/runs", "/approvals", "/audit"}, nil},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			ck, _ := ts.session(tt.role)
			main := mainContent(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
			links := hrefsIn(main)
			for _, h := range tt.want {
				if !slices.Contains(links, h) {
					t.Errorf("home lacks a link to %s: %v", h, links)
				}
			}
			for _, h := range tt.deny {
				if slices.Contains(links, h) {
					t.Errorf("home links to %s", h)
				}
			}
			for _, h := range links {
				if !ts.allows(tt.role, http.MethodGet, h) {
					t.Errorf("home links to %s, which %s may not GET", h, tt.role)
				}
			}
			if strings.Contains(main, "/api/") || strings.Contains(main, "<code>") {
				t.Errorf("home still shows API paths: %s", main)
			}
			// Destinations are UI-02 buttons.
			if !strings.Contains(main, `href="/runs"`) || !strings.Contains(main, `class="ui-btn`) {
				t.Errorf("home has no button links: %s", main)
			}
			for _, id := range allActions {
				if strings.Contains(main, `data-action="`+id+`"`) {
					seen[id] = true
				}
			}
		})
	}
	for _, id := range allActions {
		if !seen[id] {
			t.Errorf("data-action %q no longer shown to any role", id)
		}
	}
}

func TestShellRunsDefaultsFromContext(t *testing.T) {
	ts := runsServer(t, newFakeRunsStore(), nil)
	startForm := func(body string) string {
		return between(t, body, `<form method="post" action="/runs" class="start-run">`, "</form>")
	}
	prev := &http.Cookie{Name: ctxCookieName, Value: url.Values{"company": {"globex"}, "month": {"2026-07"}}.Encode()}
	tests := []struct {
		name           string
		query          string
		cookie         *http.Cookie
		company, month string
	}{
		{"defaults", "", nil, "acme", "2026-09"},
		{"cookie", "", prev, "globex", "2026-07"},
		{"query wins", "?company=acme&month=2026-03", prev, "acme", "2026-03"},
		{"query company only", "?company=acme", prev, "acme", "2026-07"},
		{"query month only", "?month=2026-04", prev, "globex", "2026-04"},
		{"empty query values", "?company=&month=", prev, "globex", "2026-07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ck, _ := ts.session(RoleMaker)
			cookies := []*http.Cookie{ck}
			if tt.cookie != nil {
				cookies = append(cookies, tt.cookie)
			}
			form := startForm(okBody(t, ts, req{method: http.MethodGet, path: "/runs" + tt.query, cookies: cookies}))
			if !strings.Contains(form, `<option value="`+tt.company+`" selected>`) || strings.Count(form, " selected>") != 1 {
				t.Errorf("start form company: want %s selected: %s", tt.company, form)
			}
			if !strings.Contains(form, `name="month" value="`+tt.month+`"`) {
				t.Errorf("start form month: want %s: %s", tt.month, form)
			}
		})
	}
	// A query value is escaped, not validated, like the POST error path.
	ck, _ := ts.session(RoleMaker)
	body := okBody(t, ts, req{method: http.MethodGet, path: "/runs?month=" + url.QueryEscape(`"><script>x</script>`), cookies: []*http.Cookie{ck}})
	if strings.Contains(body, "<script>x") || !strings.Contains(startForm(body), `value="&#34;&gt;&lt;script&gt;x&lt;/script&gt;"`) {
		t.Errorf("query month not escaped: %s", startForm(body))
	}
}
