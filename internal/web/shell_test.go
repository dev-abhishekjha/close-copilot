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
	ts.clock.Set(start.Add(70 * time.Second))
	view()
	if n := cs.count(); n != 3 {
		t.Errorf("a failed refresh is retried at once: %d calls, want 3", n)
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
