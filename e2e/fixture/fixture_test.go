package fixture

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// server starts a fresh fixture for one test, so tests never see each
// other's writes.
func server(t *testing.T) string {
	t.Helper()
	h, err := New(true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// client is a signed-in browser stand-in.
type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func (c *client) do(method, path string, form url.Values) (int, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func signIn(t *testing.T, base string, a Account) *client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: base, http: &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	code, page := c.do(http.MethodGet, "/login", nil)
	m := csrfRe.FindStringSubmatch(page)
	if code != http.StatusOK || m == nil {
		t.Fatalf("login form: status %d, csrf found %v", code, m != nil)
	}
	c.csrf = m[1]
	code, _ = c.do(http.MethodPost, "/login", url.Values{"csrf_token": {c.csrf}, "username": {a.Name}, "password": {a.Password}})
	if code != http.StatusSeeOther {
		t.Fatalf("sign in as %s: status %d", a.Name, code)
	}
	// The page CSRF token differs from the login one: read it from home.
	code, home := c.do(http.MethodGet, "/", nil)
	m = csrfRe.FindStringSubmatch(home)
	if code != http.StatusOK {
		t.Fatalf("home as %s: %d", a.Name, code)
	}
	if m != nil {
		c.csrf = m[1]
	}
	return c
}

func accountFor(r web.Role) Account {
	for _, a := range Accounts {
		if a.Role == r {
			return a
		}
	}
	panic("no account for role " + string(r))
}

var (
	viewerOnly = []web.Role{web.RoleViewer, web.RoleMaker, web.RoleChecker}
	checker    = []web.Role{web.RoleChecker}
	maker      = []web.Role{web.RoleMaker}
)

// proposalID is a seeded pending proposal.
var proposalID = sid(0xd1, 1)

// pages mirrors the GET pages of the route table with the roles that may open
// them.
var pages = []struct {
	path  string
	roles []web.Role
}{
	{"/", viewerOnly},
	{"/runs", viewerOnly},
	{"/runs/" + RunDone.String(), viewerOnly},
	{"/runs/" + RunDone.String() + "/findings", viewerOnly},
	{"/runs/" + RunDone.String() + "/metrics", viewerOnly},
	{"/runs/" + RunDone.String() + "/progress", viewerOnly},
	{"/runs/" + RunFailed.String(), viewerOnly},
	{"/runs/" + RunQueued.String(), viewerOnly},
	{"/findings", viewerOnly},
	{"/findings/" + FindClean.String(), viewerOnly},
	{"/findings/" + FindReview.String(), viewerOnly},
	{"/findings/" + FindEntry.String(), viewerOnly},
	{"/proposals/" + proposalID.String(), viewerOnly},
	{"/ask", viewerOnly},
	{"/documents", viewerOnly},
	{"/documents/POL-001", viewerOnly},
	{"/system", viewerOnly},
	{"/approvals", checker},
	{"/audit", checker},
	{"/audit/export.csv", checker},
}

func TestPagesByRole(t *testing.T) {
	base := server(t)
	for _, a := range Accounts {
		t.Run(a.Name, func(t *testing.T) {
			c := signIn(t, base, a)
			for _, p := range pages {
				want := http.StatusForbidden
				if slices.Contains(p.roles, a.Role) {
					want = http.StatusOK
				}
				if got, body := c.do(http.MethodGet, p.path, nil); got != want {
					t.Errorf("GET %s as %s = %d, want %d: %.120s", p.path, a.Role, got, want, body)
				}
			}
		})
	}
}

func TestMakerCheckerHolds(t *testing.T) {
	base := server(t)
	mk := signIn(t, base, accountFor(web.RoleMaker))
	ck := signIn(t, base, accountFor(web.RoleChecker))

	code, _ := mk.do(http.MethodPost, "/findings/"+FindEntry.String()+"/proposals", url.Values{})
	if code != http.StatusSeeOther && code != http.StatusOK {
		t.Fatalf("propose: status %d", code)
	}
	_, list := ck.do(http.MethodGet, "/approvals", nil)
	m := regexp.MustCompile(`/proposals/([0-9a-f-]{36})/approve`).FindAllStringSubmatch(list, -1)
	if len(m) == 0 {
		t.Fatal("no approvable proposal on the approvals page")
	}

	// The proposer is refused by the backend, whatever the web layer does.
	a := Approvals{s: newStore(Clock)}
	a.s.seed(Clock)
	if _, err := a.Approve(context.Background(), "asha", proposalID); err == nil {
		t.Error("the proposer approved their own entry")
	}
	if _, err := a.Approve(context.Background(), " ASHA ", proposalID); err == nil {
		t.Error("the proposer approved under a different spelling")
	}
	if _, err := a.Approve(context.Background(), "ravi", proposalID); err != nil {
		t.Errorf("a different checker was refused: %v", err)
	}
	if err := a.Reject(context.Background(), "ravi", sid(0xd1, 2), ""); err == nil {
		t.Error("rejected without a reason")
	}
}

func TestRunExecutorFinishes(t *testing.T) {
	st := newStore(Clock)
	st.seed(Clock)
	st.stepDelay = time.Millisecond
	id := uuid.New()
	ctx := context.Background()
	_ = st.CreateCloseRun(ctx, runFor(id))
	if err := st.Execute(ctx, id); err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetCloseRun(ctx, id)
	fs, _ := st.ListFindingsByRun(ctx, id)
	if r.Status != "done" || len(fs) != 3 {
		t.Errorf("status %q with %d findings, want done with 3", r.Status, len(fs))
	}
}

// Gate G: every state-changing page route of the route table has a form in the
// UI for the roles that may call it, and for no other role.
func TestEveryPostRouteHasAForm(t *testing.T) {
	type form struct {
		route  string // pattern in routes.go
		page   string // page that holds the form
		action string // form action as rendered
		roles  []web.Role
	}
	forms := []form{
		{"/runs", "/runs", `action="/runs"`, maker},
		{"/findings/{id}/accept", "/findings/" + FindClean.String(), `/findings/` + FindClean.String() + `/accept"`, maker},
		{"/findings/{id}/dismiss", "/findings/" + FindClean.String(), `/findings/` + FindClean.String() + `/dismiss"`, maker},
		{"/findings/{id}/proposals", "/findings/" + FindEntry.String(), `/findings/` + FindEntry.String() + `/proposals"`, maker},
		{"/findings/accept-selected", "/findings", `action="/findings/accept-selected"`, maker},
		{"/proposals/{id}/approve", "/approvals", `/proposals/` + proposalID.String() + `/approve"`, checker},
		{"/proposals/{id}/reject", "/approvals", `/proposals/` + proposalID.String() + `/reject"`, checker},
		{"/ask", "/ask", `action="/ask"`, viewerOnly},
		{"/context", "/", `action="/context"`, viewerOnly},
		{"/logout", "/", `action="/logout"`, viewerOnly},
	}

	// The list must name every non-API POST route of the table.
	files, err := filepath.Glob("../../internal/web/routes*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("route files: %v %v", files, err)
	}
	re := regexp.MustCompile(`http\.MethodPost, "([^"]+)"`)
	var tableRoutes []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if p := m[1]; !strings.HasPrefix(p, "/api/") && p != "/login" {
				tableRoutes = append(tableRoutes, p)
			}
		}
	}
	for _, p := range slices.Compact(slices.Sorted(slices.Values(tableRoutes))) {
		if !slices.ContainsFunc(forms, func(f form) bool { return f.route == p }) {
			t.Errorf("POST %s is in the route table but no form is checked for it: add it to this test", p)
		}
	}

	base := server(t)
	for _, a := range Accounts {
		c := signIn(t, base, a)
		for _, f := range forms {
			code, body := c.do(http.MethodGet, f.page, nil)
			if code != http.StatusOK {
				if slices.Contains(f.roles, a.Role) && !slices.Contains(checker, a.Role) {
					t.Errorf("GET %s as %s = %d", f.page, a.Role, code)
				}
				continue
			}
			has := strings.Contains(body, f.action)
			if want := slices.Contains(f.roles, a.Role); has != want {
				t.Errorf("%s as %s: form %q present = %v, want %v", f.page, a.Role, f.action, has, want)
			}
		}
	}
}

func runFor(id uuid.UUID) store.CloseRun {
	return store.CloseRun{ID: id, CompanyID: "mehta", Month: "2026-09", Status: store.RunQueued}
}
