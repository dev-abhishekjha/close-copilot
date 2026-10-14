package web

import (
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// apiSpec is the HTTP API table of the shared spec, written out by hand so
// the tests don't trust the server's own route table. nil roles = public.
var apiSpec = []struct {
	method, pattern, path string
	roles                 []Role
}{
	{http.MethodPost, "/api/close-runs", "/api/close-runs", []Role{RoleMaker}},
	{http.MethodGet, "/api/close-runs/{id}", "/api/close-runs/run-1", []Role{RoleViewer, RoleMaker, RoleChecker}},
	{http.MethodGet, "/api/close-runs/{id}/findings", "/api/close-runs/run-1/findings", []Role{RoleViewer, RoleMaker, RoleChecker}},
	{http.MethodPost, "/api/findings/{id}/proposals", "/api/findings/f-1/proposals", []Role{RoleMaker}},
	{http.MethodPost, "/api/proposals/{id}/approve", "/api/proposals/p-1/approve", []Role{RoleChecker}},
	{http.MethodPost, "/api/proposals/{id}/reject", "/api/proposals/p-1/reject", []Role{RoleChecker}},
	{http.MethodPost, "/api/ask", "/api/ask", []Role{RoleViewer, RoleMaker, RoleChecker}},
	{http.MethodGet, "/healthz", "/healthz", nil},
}

func TestRouteTableMatchesSpec(t *testing.T) {
	ts := newTestServer(t)
	for _, row := range apiSpec {
		found := false
		for _, rt := range ts.routes {
			if rt.method == row.method && rt.path == row.pattern {
				found = true
				if !slices.Equal(rt.roles, row.roles) {
					t.Errorf("%s %s roles = %v, spec says %v", row.method, row.pattern, rt.roles, row.roles)
				}
			}
		}
		if !found {
			t.Errorf("%s %s missing from the route table", row.method, row.pattern)
		}
	}
	// Every non-public route outside the spec table must be a page route
	// open to all roles; nothing else may be public.
	public := map[string]bool{"GET /healthz": true, "GET /login": true, "POST /login": true, "GET /static/": true}
	for _, rt := range ts.routes {
		key := rt.method + " " + rt.path
		if rt.roles == nil && !public[key] {
			t.Errorf("%s is public but not in the public list", key)
		}
	}
}

// caller is one way of calling a route in the role matrix.
type caller struct {
	name   string
	role   Role // "" = anonymous
	bearer bool
}

var callers = []caller{
	{name: "anonymous"},
	{name: "viewer", role: RoleViewer},
	{name: "maker", role: RoleMaker},
	{name: "checker", role: RoleChecker},
	{name: "bearer viewer", role: RoleViewer, bearer: true},
	{name: "bearer maker", role: RoleMaker, bearer: true},
	{name: "bearer checker", role: RoleChecker, bearer: true},
}

// call requests method path as c, sending the CSRF token with cookie
// sessions so only the role check decides.
func (ts *testServer) call(t *testing.T, c caller, method, path string) int {
	t.Helper()
	rq := req{method: method, path: path, header: map[string]string{}}
	switch {
	case c.role == "":
	case c.bearer:
		rq.header["Authorization"] = "Bearer " + ts.fix.tokens[ts.fix.userFor(c.role).Name]
	default:
		ck, csrf := ts.session(c.role)
		rq.cookies = []*http.Cookie{ck}
		rq.header[csrfHeaderName] = csrf
	}
	return ts.do(t, rq).Code
}

func TestRoleMatrix(t *testing.T) {
	ts := newTestServer(t)
	for _, row := range apiSpec {
		for _, c := range callers {
			want := http.StatusNotImplemented
			switch {
			case row.roles == nil:
				want = http.StatusOK
			case c.role == "":
				want = http.StatusUnauthorized
			case !slices.Contains(row.roles, c.role):
				want = http.StatusForbidden
			}
			t.Run(row.method+" "+row.pattern+" as "+c.name, func(t *testing.T) {
				if got := ts.call(t, c, row.method, row.path); got != want {
					t.Errorf("status = %d, want %d", got, want)
				}
			})
		}
	}
}

func TestViewerForbiddenOnMakerRoutes(t *testing.T) {
	ts := newTestServer(t)
	type ep struct{ method, path string }
	var (
		makerRoutes   = []ep{{http.MethodPost, "/api/close-runs"}, {http.MethodPost, "/api/findings/f-1/proposals"}}
		checkerRoutes = []ep{{http.MethodPost, "/api/proposals/p-1/approve"}, {http.MethodPost, "/api/proposals/p-1/reject"}}
		viewerRoutes  = []ep{{http.MethodGet, "/api/close-runs/run-1"}, {http.MethodGet, "/api/close-runs/run-1/findings"}, {http.MethodPost, "/api/ask"}}
	)
	tests := []struct {
		name   string
		role   Role
		routes []ep
		want   int
	}{
		{"viewer on maker routes", RoleViewer, makerRoutes, http.StatusForbidden},
		{"viewer on checker routes", RoleViewer, checkerRoutes, http.StatusForbidden},
		{"maker on checker routes", RoleMaker, checkerRoutes, http.StatusForbidden},
		{"checker on maker routes", RoleChecker, makerRoutes, http.StatusForbidden},
		{"maker on maker routes", RoleMaker, makerRoutes, http.StatusNotImplemented},
		{"checker on checker routes", RoleChecker, checkerRoutes, http.StatusNotImplemented},
		{"viewer on viewer routes", RoleViewer, viewerRoutes, http.StatusNotImplemented},
		{"maker on viewer routes", RoleMaker, viewerRoutes, http.StatusNotImplemented},
		{"checker on viewer routes", RoleChecker, viewerRoutes, http.StatusNotImplemented},
		{"viewer on home", RoleViewer, []ep{{http.MethodGet, "/"}}, http.StatusOK},
		{"maker on home", RoleMaker, []ep{{http.MethodGet, "/"}}, http.StatusOK},
		{"checker on home", RoleChecker, []ep{{http.MethodGet, "/"}}, http.StatusOK},
	}
	for _, tt := range tests {
		for _, bearer := range []bool{false, true} {
			for _, e := range tt.routes {
				if bearer && !strings.HasPrefix(e.path, "/api/") {
					continue
				}
				c := caller{name: string(tt.role), role: tt.role, bearer: bearer}
				t.Run(tt.name+" "+e.method+" "+e.path, func(t *testing.T) {
					if got := ts.call(t, c, e.method, e.path); got != tt.want {
						t.Errorf("bearer=%v: status = %d, want %d", bearer, got, tt.want)
					}
				})
			}
		}
	}
	// A 403 on the API is JSON.
	ck, csrf := ts.session(RoleViewer)
	w := ts.do(t, req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{ck}, header: map[string]string{csrfHeaderName: csrf}})
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Errorf("API 403 = %q %q, want JSON forbidden", ct, w.Body.String())
	}
}

func TestHomeActions(t *testing.T) {
	viewerActions := []string{"view-run", "view-findings", "ask"}
	makerActions := []string{"start-run", "propose-entry"}
	checkerActions := []string{"approve-proposal", "reject-proposal"}
	tests := []struct {
		role       Role
		user       string
		want, deny []string
	}{
		{RoleViewer, "demo", viewerActions, append(slices.Clone(makerActions), checkerActions...)},
		{RoleMaker, "asha", append(slices.Clone(viewerActions), makerActions...), checkerActions},
		{RoleChecker, "ravi", append(slices.Clone(viewerActions), checkerActions...), makerActions},
	}
	ts := newTestServer(t)
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			ck, csrf := ts.session(tt.role)
			w := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
			if w.Code != http.StatusOK {
				t.Fatalf("GET / = %d", w.Code)
			}
			body := w.Body.String()
			for _, id := range tt.want {
				if !strings.Contains(body, `data-action="`+id+`"`) {
					t.Errorf("home for %s lacks action %s", tt.role, id)
				}
			}
			for _, id := range tt.deny {
				if strings.Contains(body, `data-action="`+id+`"`) {
					t.Errorf("home for %s shows forbidden action %s", tt.role, id)
				}
			}
			if !strings.Contains(body, tt.user) || !strings.Contains(body, `data-role="`+string(tt.role)+`"`) {
				t.Errorf("home doesn't show user %s and role %s", tt.user, tt.role)
			}
			if !strings.Contains(body, "hx-headers") || !strings.Contains(body, csrf) {
				t.Error("home doesn't carry the session CSRF token for htmx")
			}
		})
	}
}

func TestAnonymous(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		name     string
		method   string
		path     string
		want     int
		location string
	}{
		{"home redirects", http.MethodGet, "/", http.StatusSeeOther, "/login"},
		{"deep page keeps next", http.MethodGet, "/runs/abc?tab=1", http.StatusSeeOther, "/login?next=%2Fruns%2Fabc%3Ftab%3D1"},
		{"logout post redirects without next", http.MethodPost, "/logout", http.StatusSeeOther, "/login"},
		{"api get 401", http.MethodGet, "/api/close-runs/run-1", http.StatusUnauthorized, ""},
		{"api findings 401", http.MethodGet, "/api/close-runs/run-1/findings", http.StatusUnauthorized, ""},
		{"api post 401", http.MethodPost, "/api/close-runs", http.StatusUnauthorized, ""},
		{"api approve 401", http.MethodPost, "/api/proposals/p-1/approve", http.StatusUnauthorized, ""},
		{"healthz public", http.MethodGet, "/healthz", http.StatusOK, ""},
		{"login public", http.MethodGet, "/login", http.StatusOK, ""},
		{"static public", http.MethodGet, "/static/app.css", http.StatusOK, ""},
		{"static dir not listed", http.MethodGet, "/static/", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.do(t, req{method: tt.method, path: tt.path})
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if tt.location != "" && w.Header().Get("Location") != tt.location {
				t.Errorf("Location = %q, want %q", w.Header().Get("Location"), tt.location)
			}
			if tt.want == http.StatusUnauthorized {
				if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("Content-Type = %q, want JSON", ct)
				}
				if w.Header().Get("Location") != "" {
					t.Error("API 401 must not redirect")
				}
			}
		})
	}
	// The deep page's next is accepted by the login page.
	w := ts.do(t, req{method: http.MethodGet, path: "/login?next=%2Fruns%2Fabc%3Ftab%3D1"})
	if !strings.Contains(w.Body.String(), `name="next" value="/runs/abc?tab=1"`) {
		t.Error("login form doesn't carry the next path")
	}
}

// TestRoutesNoERPClientDependency checks that the web package does not
// reach the ERPNext client, directly or transitively.
func TestRoutesNoERPClientDependency(t *testing.T) {
	const module = "github.com/abhishekjha/close-copilot"
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", module+"/internal/web").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("go list -deps listed only %d packages", len(deps))
	}
	for _, dep := range deps {
		if dep == module+"/internal/frappe" || strings.HasPrefix(dep, module+"/internal/frappe/") {
			t.Errorf("internal/web depends on %s", dep)
		}
	}
}
