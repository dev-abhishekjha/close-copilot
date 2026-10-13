package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// testKey is a fixed 32-byte session key (the minimum).
const testKey = "0123456789abcdef0123456789abcdef"

// testHost is httptest.NewRequest's default Host.
const testHost = "example.com"

// fixture is the in-memory users set shared by the tests. Hashes use the
// minimum allowed bcrypt cost to keep the tests fast.
type fixture struct {
	users     Users
	passwords map[string]string // username -> password
	tokens    map[string]string // username -> bearer token (plaintext)
	byName    map[string]User
}

var (
	fixOnce sync.Once
	fix     fixture
	fixErr  error
)

func hashPassword(t testing.TB, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), MinBcryptCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// testFixture returns asha (maker), ravi (checker) and demo (viewer), each
// with a bearer token, and nora (viewer) without one.
func testFixture(t testing.TB) fixture {
	t.Helper()
	fixOnce.Do(func() {
		f := fixture{
			passwords: map[string]string{
				"asha": "asha-test-password-1",
				"ravi": "ravi-test-password-2",
				"demo": "demo-test-password-3",
				"nora": "nora-test-password-4",
			},
			tokens: map[string]string{
				"asha": "tok-asha-7b1e0c9d2f",
				"ravi": "tok-ravi-3a8f6e4b1c",
				"demo": "tok-demo-9d2c5a7e0f",
			},
			byName: map[string]User{},
		}
		roles := map[string]Role{"asha": RoleMaker, "ravi": RoleChecker, "demo": RoleViewer, "nora": RoleViewer}
		var list []User
		for _, name := range []string{"asha", "ravi", "demo", "nora"} {
			u := User{Name: name, Role: roles[name], PasswordHash: hashPassword(t, f.passwords[name])}
			if tok, ok := f.tokens[name]; ok {
				u.TokenSHA256 = tokenHash(tok)
			}
			list = append(list, u)
			f.byName[name] = u
		}
		f.users, fixErr = NewUsers(list...)
		fix = f
	})
	if fixErr != nil {
		t.Fatalf("fixture: %v", fixErr)
	}
	return fix
}

// userFor returns the fixture user with role r that has a bearer token.
func (f fixture) userFor(r Role) User {
	switch r {
	case RoleMaker:
		return f.byName["asha"]
	case RoleChecker:
		return f.byName["ravi"]
	default:
		return f.byName["demo"]
	}
}

// with returns the fixture users with u replacing (or adding) its entry and
// every name in drop removed.
func (f fixture) with(t testing.TB, u *User, drop ...string) Users {
	t.Helper()
	var list []User
	for _, x := range f.users.All() {
		skip := false
		for _, d := range drop {
			skip = skip || x.Name == d
		}
		if skip || (u != nil && x.Name == u.Name) {
			continue
		}
		list = append(list, x)
	}
	if u != nil {
		list = append(list, *u)
	}
	us, err := NewUsers(list...)
	if err != nil {
		t.Fatalf("NewUsers: %v", err)
	}
	return us
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testServer struct {
	*Server
	fix   fixture
	logs  *syncBuffer
	clock *fakeClock
}

// newTestServer builds a server over the fixture users with the test key,
// a fake clock and a captured JSON log. Cookies are Secure (the default).
func newTestServer(t testing.TB, mods ...func(*Options)) *testServer {
	t.Helper()
	f := testFixture(t)
	clock := &fakeClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	opts := Options{Addr: "127.0.0.1:0", SessionKey: config.NewSecret(testKey), Now: clock.Now}
	for _, m := range mods {
		m(&opts)
	}
	s, err := NewServer(opts, f.users, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return &testServer{Server: s, fix: f, logs: logs, clock: clock}
}

// session mints a session for the fixture user with role r.
func (ts *testServer) session(r Role) (*http.Cookie, string) {
	return ts.newSession(ts.fix.userFor(r))
}

type req struct {
	method      string
	path        string
	body        string
	contentType string
	cookies     []*http.Cookie
	header      map[string]string
}

func (ts *testServer) do(t testing.TB, rq req) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	}
	r := httptest.NewRequest(rq.method, rq.path, body)
	if rq.contentType != "" {
		r.Header.Set("Content-Type", rq.contentType)
	}
	for k, v := range rq.header {
		r.Header.Set(k, v)
	}
	for _, c := range rq.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	ts.ServeHTTP(w, r)
	return w
}

// responseCookie returns the Set-Cookie named name from w, or nil.
func responseCookie(t testing.TB, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, line := range w.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("parse Set-Cookie %q: %v", line, err)
		}
		if c.Name == name {
			return c
		}
	}
	return nil
}

var csrfFieldRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// loginForm does GET /login and returns the pre-session cookie and the
// form's CSRF token.
func (ts *testServer) loginForm(t testing.TB) (*http.Cookie, string) {
	t.Helper()
	w := ts.do(t, req{method: http.MethodGet, path: "/login"})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", w.Code)
	}
	ck := responseCookie(t, w, ts.loginCSRFCookieName())
	if ck == nil {
		t.Fatal("GET /login set no login CSRF cookie")
	}
	m := csrfFieldRE.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("login form has no csrf_token field")
	}
	return ck, m[1]
}

// postLogin posts the login form with the given fields and headers.
func (ts *testServer) postLogin(t testing.TB, ck *http.Cookie, fields url.Values, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var cookies []*http.Cookie
	if ck != nil {
		cookies = []*http.Cookie{{Name: ck.Name, Value: ck.Value}}
	}
	return ts.do(t, req{
		method:      http.MethodPost,
		path:        "/login",
		body:        fields.Encode(),
		contentType: "application/x-www-form-urlencoded",
		cookies:     cookies,
		header:      header,
	})
}

// login runs the whole browser login for username and returns the response.
func (ts *testServer) login(t testing.TB, username, password, next string) *httptest.ResponseRecorder {
	t.Helper()
	ck, tok := ts.loginForm(t)
	return ts.postLogin(t, ck, url.Values{
		"csrf_token": {tok},
		"username":   {username},
		"password":   {password},
		"next":       {next},
	}, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost})
}
