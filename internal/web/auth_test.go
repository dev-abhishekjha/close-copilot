package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestSessionCookieFlags(t *testing.T) {
	tests := []struct {
		name       string
		insecure   bool
		wantSecure bool
	}{
		{"secure by default", false, true},
		{"explicit test option drops Secure", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, func(o *Options) { o.InsecureCookies = tt.insecure })
			w := ts.login(t, "asha", ts.fix.passwords["asha"], "")
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
				t.Fatalf("login = %d to %q, want 303 to /", w.Code, w.Header().Get("Location"))
			}
			c := responseCookie(t, w, sessionCookieName)
			if c == nil {
				t.Fatal("no session cookie")
			}
			if !c.HttpOnly {
				t.Error("HttpOnly not set")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v, want Lax", c.SameSite)
			}
			if c.Secure != tt.wantSecure {
				t.Errorf("Secure = %v, want %v", c.Secure, tt.wantSecure)
			}
			if c.Path != "/" {
				t.Errorf("Path = %q, want /", c.Path)
			}
			if c.MaxAge != int(sessionTTL/time.Second) || c.Expires.IsZero() {
				t.Errorf("MaxAge = %d, Expires = %v; want 8h expiry", c.MaxAge, c.Expires)
			}
			// The login CSRF cookie is cleared after a successful login.
			if lc := responseCookie(t, w, loginCSRFCookieName); lc == nil || lc.MaxAge >= 0 {
				t.Error("login CSRF cookie not cleared")
			}
			// The cookie works.
			if got := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{c}}).Code; got != http.StatusOK {
				t.Errorf("GET / with fresh session = %d", got)
			}
		})
	}
}

// expectAnonymous asserts the cookie gets the anonymous treatment on an
// HTML page and on the API.
func expectAnonymous(t *testing.T, ts *testServer, ck *http.Cookie) {
	t.Helper()
	w := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("GET / = %d to %q, want 303 to /login", w.Code, w.Header().Get("Location"))
	}
	if got := ts.do(t, req{method: http.MethodGet, path: "/api/close-runs/run-1", cookies: []*http.Cookie{ck}}).Code; got != http.StatusUnauthorized {
		t.Errorf("GET API = %d, want 401", got)
	}
}

func TestSessionTamper(t *testing.T) {
	ts := newTestServer(t)
	good, _ := ts.session(RoleViewer)
	payload, sig, _ := strings.Cut(good.Value, ".")

	other := newTestServer(t, func(o *Options) { o.SessionKey = config.NewSecret("ffffffffffffffffffffffffffffffff") })
	foreign, _ := other.session(RoleChecker)

	// Same signature, payload rewritten to another user.
	raw, err := b64.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var c sessionClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Sub = "ravi"
	forged, _ := json.Marshal(c)
	c.Sub, c.Exp = "demo", c.Exp+3600
	extended, _ := json.Marshal(c)

	flip := []byte(sig)
	if flip[0] == 'A' {
		flip[0] = 'B'
	} else {
		flip[0] = 'A'
	}

	tests := []struct {
		name  string
		value string
	}{
		{"flipped signature byte", payload + "." + string(flip)},
		{"payload changed to another user", b64.EncodeToString(forged) + "." + sig},
		{"expiry extended", b64.EncodeToString(extended) + "." + sig},
		{"signed with another key", foreign.Value},
		{"no signature", payload},
		{"empty signature", payload + "."},
		{"three parts", good.Value + ".x"},
		{"garbage", "not-a-session"},
		{"oversized", strings.Repeat("A", maxCookieLen+1) + "." + sig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectAnonymous(t, ts, &http.Cookie{Name: sessionCookieName, Value: tt.value})
		})
	}
	t.Run("untampered still works", func(t *testing.T) {
		if got := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{good}}).Code; got != http.StatusOK {
			t.Errorf("GET / = %d, want 200", got)
		}
	})
}

func TestSessionExpiry(t *testing.T) {
	ts := newTestServer(t)
	t0 := ts.clock.Now()
	ck, _ := ts.session(RoleMaker)
	tests := []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"just issued", t0, true},
		{"7h59m later", t0.Add(7*time.Hour + 59*time.Minute), true},
		{"exactly 8h later", t0.Add(sessionTTL), false},
		{"a day later", t0.Add(24 * time.Hour), false},
		{"issued in the future (clock skew beyond 1m)", t0.Add(-2 * time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts.clock.Set(tt.at)
			if tt.valid {
				if got := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}).Code; got != http.StatusOK {
					t.Errorf("GET / = %d, want 200", got)
				}
				return
			}
			expectAnonymous(t, ts, ck)
		})
	}
}

func TestCredentialRotationKillsSession(t *testing.T) {
	f := testFixture(t)
	asha := f.byName["asha"]
	nora := f.byName["nora"]

	newHash := asha
	newHash.PasswordHash = hashPassword(t, f.passwords["asha"]) // same password, new salt
	newToken := asha
	newToken.TokenSHA256 = tokenHash("tok-asha-rotated")
	tokenAdded := nora
	tokenAdded.TokenSHA256 = tokenHash("tok-nora-new")
	tokenRemoved := asha
	tokenRemoved.TokenSHA256 = ""

	tests := []struct {
		name string
		user User
	}{
		{"password hash rotated", newHash},
		{"token hash rotated", newToken},
		{"token hash removed", tokenRemoved},
		{"token hash added", tokenAdded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			ck, _ := ts.newSession(f.byName[tt.user.Name])
			if got := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}).Code; got != http.StatusOK {
				t.Fatalf("before rotation GET / = %d", got)
			}
			u := tt.user
			if err := ts.SetUsers(f.with(t, &u)); err != nil {
				t.Fatal(err)
			}
			expectAnonymous(t, ts, ck)
		})
	}

	t.Run("old bearer token stops working", func(t *testing.T) {
		ts := newTestServer(t)
		u := newToken
		if err := ts.SetUsers(f.with(t, &u)); err != nil {
			t.Fatal(err)
		}
		w := ts.do(t, req{method: http.MethodGet, path: "/api/close-runs/run-1", header: map[string]string{"Authorization": "Bearer " + f.tokens["asha"]}})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("old token = %d, want 401", w.Code)
		}
		w = ts.do(t, req{method: http.MethodGet, path: "/api/close-runs/run-1", header: map[string]string{"Authorization": "Bearer tok-asha-rotated"}})
		if w.Code != http.StatusNotImplemented {
			t.Errorf("new token = %d, want 501", w.Code)
		}
	})

	t.Run("role change applies to the live session", func(t *testing.T) {
		ts := newTestServer(t)
		ck, csrf := ts.newSession(asha)
		promoted := asha
		promoted.Role = RoleChecker
		if err := ts.SetUsers(f.with(t, &promoted, "ravi")); err != nil {
			t.Fatal(err)
		}
		h := map[string]string{csrfHeaderName: csrf}
		if got := ts.do(t, req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{ck}, header: h}).Code; got != http.StatusForbidden {
			t.Errorf("maker route after role change = %d, want 403", got)
		}
		if got := ts.do(t, req{method: http.MethodPost, path: "/api/proposals/p-1/approve", cookies: []*http.Cookie{ck}, header: h}).Code; got != http.StatusNotImplemented {
			t.Errorf("checker route after role change = %d, want 501", got)
		}
	})
}

func TestRemovedUser(t *testing.T) {
	f := testFixture(t)
	ts := newTestServer(t)
	ck, _ := ts.session(RoleMaker)
	if err := ts.SetUsers(f.with(t, nil, "asha")); err != nil {
		t.Fatal(err)
	}
	expectAnonymous(t, ts, ck)
	w := ts.do(t, req{method: http.MethodGet, path: "/api/close-runs/run-1", header: map[string]string{"Authorization": "Bearer " + f.tokens["asha"]}})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("removed user's bearer = %d, want 401", w.Code)
	}
	w = ts.login(t, "asha", f.passwords["asha"], "")
	if w.Code != http.StatusUnauthorized || responseCookie(t, w, sessionCookieName) != nil {
		t.Errorf("removed user's login = %d, want 401 without a session", w.Code)
	}
}

func TestCSRF(t *testing.T) {
	ts := newTestServer(t)
	makerCk, makerCSRF := ts.session(RoleMaker)
	checkerCk, checkerCSRF := ts.session(RoleChecker)
	_, otherCSRF := ts.session(RoleMaker) // a different session's token
	makerBearer := "Bearer " + ts.fix.tokens["asha"]

	tests := []struct {
		name string
		rq   req
		want int
	}{
		{"json post without token", req{method: http.MethodPost, path: "/api/close-runs", body: `{"company":"x","month":"2026-09"}`, contentType: "application/json", cookies: []*http.Cookie{makerCk}}, http.StatusForbidden},
		{"text/plain post without token", req{method: http.MethodPost, path: "/api/close-runs", body: `{"company":"x"}`, contentType: "text/plain", cookies: []*http.Cookie{makerCk}}, http.StatusForbidden},
		{"form post without token", req{method: http.MethodPost, path: "/api/close-runs", body: "company=x", contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{makerCk}}, http.StatusForbidden},
		{"wrong token", req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: "wrong"}}, http.StatusForbidden},
		{"another session's token", req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: otherCSRF}}, http.StatusForbidden},
		{"token in query string is ignored", req{method: http.MethodPost, path: "/api/close-runs?csrf_token=" + url.QueryEscape(makerCSRF), cookies: []*http.Cookie{makerCk}}, http.StatusForbidden},
		{"checker post without token", req{method: http.MethodPost, path: "/api/proposals/p-1/approve", contentType: "application/json", body: "{}", cookies: []*http.Cookie{checkerCk}}, http.StatusForbidden},
		{"valid header token passes", req{method: http.MethodPost, path: "/api/close-runs", body: "{}", contentType: "application/json", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: makerCSRF}}, http.StatusNotImplemented},
		{"valid form token passes", req{method: http.MethodPost, path: "/api/close-runs", body: "csrf_token=" + url.QueryEscape(makerCSRF), contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{makerCk}}, http.StatusNotImplemented},
		{"valid token but cross-site", req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: makerCSRF, "Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},
		{"valid token but foreign origin", req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: makerCSRF, "Origin": "https://evil.example"}}, http.StatusForbidden},
		{"checker with token passes", req{method: http.MethodPost, path: "/api/proposals/p-1/approve", cookies: []*http.Cookie{checkerCk}, header: map[string]string{csrfHeaderName: checkerCSRF}}, http.StatusNotImplemented},
		{"bearer skips CSRF", req{method: http.MethodPost, path: "/api/close-runs", body: "{}", contentType: "application/json", header: map[string]string{"Authorization": makerBearer}}, http.StatusNotImplemented},
		{"bad bearer plus valid cookie is 401", req{method: http.MethodPost, path: "/api/close-runs", cookies: []*http.Cookie{makerCk}, header: map[string]string{"Authorization": "Bearer nope", csrfHeaderName: makerCSRF}}, http.StatusUnauthorized},
		{"GET needs no token", req{method: http.MethodGet, path: "/api/close-runs/run-1", cookies: []*http.Cookie{makerCk}}, http.StatusNotImplemented},
		{"role check still applies with token", req{method: http.MethodPost, path: "/api/proposals/p-1/approve", cookies: []*http.Cookie{makerCk}, header: map[string]string{csrfHeaderName: makerCSRF}}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ts.do(t, tt.rq).Code; got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLoginCSRF(t *testing.T) {
	ts := newTestServer(t)
	ck, tok := ts.loginForm(t)
	asha := ts.fix.passwords["asha"]
	fields := func(token string) url.Values {
		return url.Values{"csrf_token": {token}, "username": {"asha"}, "password": {asha}}
	}
	_, otherTok := ts.loginForm(t) // a token for a different cookie nonce

	tests := []struct {
		name   string
		cookie *http.Cookie
		fields url.Values
		header map[string]string
		want   int
	}{
		{"no cookie no token", nil, url.Values{"username": {"asha"}, "password": {asha}}, nil, http.StatusForbidden},
		{"token without cookie", nil, fields(tok), nil, http.StatusForbidden},
		{"cookie without token", ck, url.Values{"username": {"asha"}, "password": {asha}}, nil, http.StatusForbidden},
		{"wrong token", ck, fields("wrong"), nil, http.StatusForbidden},
		{"token for another cookie", ck, fields(otherTok), nil, http.StatusForbidden},
		{"cookie value as token (unsigned double submit)", ck, fields(ck.Value), nil, http.StatusForbidden},
		{"cross-site fetch metadata", ck, fields(tok), map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site but not same-origin", ck, fields(tok), map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign origin", ck, fields(tok), map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null origin", ck, fields(tok), map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"valid same-origin login", ck, fields(tok), map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost}, http.StatusSeeOther},
		{"valid without fetch metadata", ck, fields(tok), nil, http.StatusSeeOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.postLogin(t, tt.cookie, tt.fields, tt.header)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if got := responseCookie(t, w, sessionCookieName) != nil; got != (tt.want == http.StatusSeeOther) {
				t.Errorf("session cookie set = %v", got)
			}
		})
	}

	t.Run("bad credentials share one message", func(t *testing.T) {
		for _, creds := range [][2]string{{"asha", "wrong-password"}, {"nobody", "whatever"}, {"asha", ""}, {"asha", strings.Repeat("x", 100)}} {
			w := ts.postLogin(t, ck, url.Values{"csrf_token": {tok}, "username": {creds[0]}, "password": {creds[1]}}, nil)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), invalidLogin) {
				t.Errorf("%s: status %d, want 401 with %q", creds[0], w.Code, invalidLogin)
			}
			if responseCookie(t, w, sessionCookieName) != nil {
				t.Errorf("%s: failed login set a session", creds[0])
			}
		}
	})
}

func TestBearer(t *testing.T) {
	ts := newTestServer(t)
	f := ts.fix
	viewerCk, _ := ts.session(RoleViewer)
	tests := []struct {
		name    string
		path    string
		auth    string
		cookie  *http.Cookie
		want    int
		wantLoc bool
	}{
		{"html: valid cookie plus bad bearer uses the cookie", "/", "Bearer nope", viewerCk, http.StatusOK, false},
		{"html: valid bearer alone is anonymous", "/", "Bearer " + f.tokens["asha"], nil, http.StatusSeeOther, true},
		{"api: valid viewer token", "/api/close-runs/run-1", "Bearer " + f.tokens["demo"], nil, http.StatusNotImplemented, false},
		{"api: lower-case scheme accepted", "/api/close-runs/run-1", "bearer " + f.tokens["demo"], nil, http.StatusNotImplemented, false},
		{"api: wrong token", "/api/close-runs/run-1", "Bearer tok-wrong", nil, http.StatusUnauthorized, false},
		{"api: token hash itself is not a token", "/api/close-runs/run-1", "Bearer " + f.byName["demo"].TokenSHA256, nil, http.StatusUnauthorized, false},
		{"api: empty bearer", "/api/close-runs/run-1", "Bearer ", nil, http.StatusUnauthorized, false},
		{"api: basic auth scheme", "/api/close-runs/run-1", "Basic YXNoYTp4", nil, http.StatusUnauthorized, false},
		{"api: bad bearer does not fall back to cookie", "/api/close-runs/run-1", "Bearer nope", viewerCk, http.StatusUnauthorized, false},
		{"api: user without token can't use bearer", "/api/close-runs/run-1", "Bearer " + f.passwords["nora"], nil, http.StatusUnauthorized, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rq := req{method: http.MethodGet, path: tt.path, header: map[string]string{"Authorization": tt.auth}}
			if tt.cookie != nil {
				rq.cookies = []*http.Cookie{tt.cookie}
			}
			w := ts.do(t, rq)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if tt.wantLoc && !strings.HasPrefix(w.Header().Get("Location"), "/login") {
				t.Errorf("Location = %q, want /login", w.Header().Get("Location"))
			}
			if responseCookie(t, w, sessionCookieName) != nil {
				t.Error("bearer request set a session cookie")
			}
		})
	}

	t.Run("bearer authenticates as its user and role", func(t *testing.T) {
		for _, name := range []string{"asha", "ravi", "demo"} {
			p, ok := ts.bearerPrincipal("Bearer " + f.tokens[name])
			if !ok || p.user.Name != name || p.user.Role != f.byName[name].Role || p.method != authBearer {
				t.Errorf("%s: principal = %+v, %v", name, p, ok)
			}
		}
	})

	t.Run("user with no token_sha256 never matches", func(t *testing.T) {
		// nora has no token; no presented token can be her, not even one
		// whose hash is the empty string's.
		for _, tok := range []string{"", " ", tokenHash(""), "nora"} {
			if p, ok := ts.bearerPrincipal("Bearer " + tok); ok {
				t.Errorf("token %q authenticated as %s", tok, p.user.Name)
			}
		}
	})
}

func TestLogout(t *testing.T) {
	ts := newTestServer(t)
	ck, csrf := ts.session(RoleViewer)
	tests := []struct {
		name   string
		rq     req
		want   int
		clears bool
	}{
		{"without CSRF", req{method: http.MethodPost, path: "/logout", cookies: []*http.Cookie{ck}}, http.StatusForbidden, false},
		{"wrong CSRF", req{method: http.MethodPost, path: "/logout", cookies: []*http.Cookie{ck}, body: "csrf_token=bad", contentType: "application/x-www-form-urlencoded"}, http.StatusForbidden, false},
		{"GET is not logout", req{method: http.MethodGet, path: "/logout", cookies: []*http.Cookie{ck}}, http.StatusNotFound, false},
		{"form CSRF", req{method: http.MethodPost, path: "/logout", cookies: []*http.Cookie{ck}, body: "csrf_token=" + url.QueryEscape(csrf), contentType: "application/x-www-form-urlencoded"}, http.StatusSeeOther, true},
		{"header CSRF", req{method: http.MethodPost, path: "/logout", cookies: []*http.Cookie{ck}, header: map[string]string{csrfHeaderName: csrf}}, http.StatusSeeOther, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.do(t, tt.rq)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			c := responseCookie(t, w, sessionCookieName)
			if !tt.clears {
				if c != nil {
					t.Error("cookie changed without a valid logout")
				}
				return
			}
			if c == nil || c.MaxAge >= 0 || c.Value != "" || c.Path != "/" || !c.Secure || !c.HttpOnly {
				t.Fatalf("logout cookie = %+v, want an expired, cleared cookie", c)
			}
			if w.Header().Get("Location") != "/login" {
				t.Errorf("Location = %q, want /login", w.Header().Get("Location"))
			}
		})
	}
}

func TestOpenRedirect(t *testing.T) {
	tests := []struct {
		next, want string
	}{
		{"//evil.example", "/"},
		{"https://evil.example", "/"},
		{"http:/evil.example", "/"},
		{`/\evil.example`, "/"},
		{`\\evil.example`, "/"},
		{"/%2F%2Fevil.example", "/"},
		{"/%2f%2fevil.example", "/"},
		{"/%5Cevil.example", "/"},
		{"/%5cevil.example", "/"},
		{"/%zz", "/"},
		{"javascript:alert(1)", "/"},
		{"/\x00evil", "/"},
		{"/ok\r\nLocation: //evil", "/"},
		{"/\t/evil.example", "/"},
		{"evil.example", "/"},
		{"", "/"},
		{"/", "/"},
		{"/runs/abc", "/runs/abc"},
		{"/runs/abc?tab=findings#x", "/runs/abc?tab=findings#x"},
	}
	for _, tt := range tests {
		t.Run(tt.next, func(t *testing.T) {
			if got := safeNext(tt.next); got != tt.want {
				t.Errorf("safeNext(%q) = %q, want %q", tt.next, got, tt.want)
			}
		})
	}

	ts := newTestServer(t)
	for _, next := range []string{"//evil.example", "https://evil.example", `/\evil.example`, "/%2F%2Fevil.example", "javascript:alert(1)", ""} {
		t.Run("login next "+next, func(t *testing.T) {
			// The login form renders the sanitised value.
			w := ts.do(t, req{method: http.MethodGet, path: "/login?next=" + url.QueryEscape(next)})
			if !strings.Contains(w.Body.String(), `name="next" value="/"`) {
				t.Errorf("login form next for %q is not /", next)
			}
		})
	}
	t.Run("login redirects only to a safe next", func(t *testing.T) {
		w := ts.login(t, "demo", ts.fix.passwords["demo"], "//evil.example")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("login with evil next = %d to %q, want 303 to /", w.Code, w.Header().Get("Location"))
		}
		w = ts.login(t, "demo", ts.fix.passwords["demo"], "/runs/abc?tab=1")
		if w.Header().Get("Location") != "/runs/abc?tab=1" {
			t.Errorf("login with safe next went to %q", w.Header().Get("Location"))
		}
	})
}

func TestShortSessionKey(t *testing.T) {
	f := testFixture(t)
	tests := []struct {
		name string
		key  string
		ok   bool
	}{
		{"empty", "", false},
		{"31 bytes", strings.Repeat("k", 31), false},
		{"32 bytes", strings.Repeat("k", 32), true},
		{"64 bytes", strings.Repeat("k", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewServer(Options{SessionKey: config.NewSecret(tt.key)}, f.users, nil)
			if tt.ok {
				if err != nil || s == nil {
					t.Fatalf("NewServer: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("NewServer accepted a short key")
			}
			if tt.key != "" && strings.Contains(err.Error(), tt.key) {
				t.Error("error message contains the key")
			}
		})
	}
	t.Run("no users", func(t *testing.T) {
		if _, err := NewServer(Options{SessionKey: config.NewSecret(testKey)}, Users{}, nil); err == nil {
			t.Error("NewServer accepted an empty users set")
		}
	})
}

func TestNoSecretsInLogs(t *testing.T) {
	ts := newTestServer(t)
	f := ts.fix
	var responses []string
	record := func(code int, header http.Header, body string) {
		var b strings.Builder
		for k, vs := range header {
			b.WriteString(k + ": " + strings.Join(vs, ", ") + "\n")
		}
		responses = append(responses, b.String()+body)
		_ = code
	}

	// Successful and failed logins, bearer calls good and bad, cookie calls,
	// a CSRF failure and a logout.
	for _, name := range []string{"asha", "ravi", "demo"} {
		w := ts.login(t, name, f.passwords[name], "/")
		record(w.Code, w.Header(), w.Body.String())
		w = ts.login(t, name, f.passwords[name]+"-wrong", "/")
		record(w.Code, w.Header(), w.Body.String())
		// A password typed into the username field must not be logged either.
		w = ts.login(t, f.passwords[name], f.passwords[name], "/")
		record(w.Code, w.Header(), w.Body.String())
		w = ts.do(t, req{method: http.MethodPost, path: "/api/close-runs", header: map[string]string{"Authorization": "Bearer " + f.tokens[name]}})
		record(w.Code, w.Header(), w.Body.String())
		w = ts.do(t, req{method: http.MethodGet, path: "/api/close-runs/run-1", header: map[string]string{"Authorization": "Bearer " + f.tokens[name] + "x"}})
		record(w.Code, w.Header(), w.Body.String())
		ck, csrf := ts.newSession(f.byName[name])
		w = ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
		record(w.Code, w.Header(), w.Body.String())
		w = ts.do(t, req{method: http.MethodPost, path: "/api/ask", cookies: []*http.Cookie{ck}, body: "csrf_token=wrong&q=" + url.QueryEscape(f.passwords[name]), contentType: "application/x-www-form-urlencoded"})
		record(w.Code, w.Header(), w.Body.String())
		w = ts.do(t, req{method: http.MethodPost, path: "/logout", cookies: []*http.Cookie{ck}, header: map[string]string{csrfHeaderName: csrf}})
		record(w.Code, w.Header(), w.Body.String())
	}

	logs := ts.logs.String()
	if !strings.Contains(logs, `"user":"asha"`) || !strings.Contains(logs, `"role":"maker"`) {
		t.Fatalf("request log lacks user and role:\n%s", logs)
	}
	secrets := map[string]string{"session key": testKey}
	for name, u := range f.byName {
		secrets["password of "+name] = f.passwords[name]
		secrets["hash of "+name] = u.PasswordHash
		if u.TokenSHA256 != "" {
			secrets["token of "+name] = f.tokens[name]
			secrets["token hash of "+name] = u.TokenSHA256
		}
	}
	for what, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains the %s", what)
		}
		for i, r := range responses {
			if strings.Contains(r, secret) {
				t.Errorf("response %d contains the %s", i, what)
			}
		}
	}
	// The log is JSON lines.
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("log line is not JSON: %s", line)
		}
	}
}

func TestDeriveKeysErrorHidesKey(t *testing.T) {
	key := "short-secret-value"
	_, err := deriveKeys(config.NewSecret(key))
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("deriveKeys error = %v", err)
	}
	if errors.Unwrap(err) != nil {
		t.Errorf("deriveKeys error wraps %v", errors.Unwrap(err))
	}
}
