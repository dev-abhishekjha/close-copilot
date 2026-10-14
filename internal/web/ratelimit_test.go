package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// defaultLimits clears the roomy test limits so the real defaults apply.
func defaultLimits(o *Options) {
	o.APIRate, o.APIBurst, o.AskRate, o.AskBurst = 0, 0, 0, 0
}

func (ts *testServer) bearer(user, method, path, body string) req {
	rq := req{method: method, path: path, body: body, header: map[string]string{"Authorization": "Bearer " + ts.fix.tokens[user]}}
	if body != "" {
		rq.contentType = "application/json"
	}
	return rq
}

// doFrom is do with a chosen client address.
func (ts *testServer) doFrom(t testing.TB, addr string, rq req) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(rq.method, rq.path, body)
	r.RemoteAddr = addr
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

// loginFrom runs the browser login from addr.
func (ts *testServer) loginFrom(t testing.TB, addr, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	w := ts.doFrom(t, addr, req{method: http.MethodGet, path: "/login"})
	ck := responseCookie(t, w, ts.loginCSRFCookieName())
	m := csrfFieldRE.FindStringSubmatch(w.Body.String())
	if ck == nil || m == nil {
		t.Fatalf("GET /login gave no form (status %d)", w.Code)
	}
	form := url.Values{"csrf_token": {m[1]}, "username": {user}, "password": {pw}}
	return ts.doFrom(t, addr, req{
		method: http.MethodPost, path: "/login", body: form.Encode(),
		contentType: "application/x-www-form-urlencoded",
		cookies:     []*http.Cookie{{Name: ck.Name, Value: ck.Value}},
		header:      map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost},
	})
}

func statusOf(t testing.TB, ts *testServer, rq req) int {
	t.Helper()
	return ts.do(t, rq).Code
}

func TestRateLimitBucket(t *testing.T) {
	fs := newFakeRunsStore()
	fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-09", Status: store.RunDone})
	ts := newTestServer(t, defaultLimits, func(o *Options) {
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
	get := ts.bearer("asha", http.MethodGet, "/api/close-runs/"+runID(1).String(), "")
	for i := 1; i <= 10; i++ {
		if c := statusOf(t, ts, get); c != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, c)
		}
	}
	w := ts.do(t, get)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th = %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1", ra)
	}
	if !strings.Contains(w.Body.String(), "rate limit exceeded") {
		t.Errorf("body = %q", w.Body)
	}
	for _, h := range []string{"X-Content-Type-Options", "Content-Security-Policy", "Referrer-Policy", "X-Frame-Options"} {
		if w.Header().Get(h) == "" {
			t.Errorf("429 lacks %s", h)
		}
	}
	if !strings.Contains(ts.logs.String(), `"event":"rate_limited"`) {
		t.Error("request log does not say why")
	}

	// Refill: 200 ms is one token at 5 a second.
	ts.clock.Set(ts.clock.Now().Add(250 * time.Millisecond))
	if c := statusOf(t, ts, get); c != http.StatusOK {
		t.Errorf("after refill = %d, want 200", c)
	}
	if c := statusOf(t, ts, get); c != http.StatusTooManyRequests {
		t.Errorf("immediately after = %d, want 429", c)
	}
	ts.clock.Set(ts.clock.Now().Add(10 * time.Second))
	for i := 1; i <= 10; i++ {
		if c := statusOf(t, ts, get); c != http.StatusOK {
			t.Fatalf("after a long wait request %d = %d, want 200", i, c)
		}
	}
}

func TestRateLimitPerUserAndSharedAuth(t *testing.T) {
	ts := newTestServer(t, defaultLimits)
	path := "/api/close-runs/" + runID(1).String()
	// Mix cookie and bearer for the same user: one shared bucket.
	for i := 0; i < 10; i++ {
		var rq req
		if i%2 == 0 {
			rq = ts.as(RoleMaker, http.MethodGet, path, "", "")
		} else {
			rq = ts.bearer("asha", http.MethodGet, path, "")
		}
		if c := statusOf(t, ts, rq); c == http.StatusTooManyRequests {
			t.Fatalf("request %d limited early", i+1)
		}
	}
	if c := statusOf(t, ts, ts.as(RoleMaker, http.MethodGet, path, "", "")); c != http.StatusTooManyRequests {
		t.Errorf("cookie after mixed use = %d, want 429", c)
	}
	if c := statusOf(t, ts, ts.bearer("asha", http.MethodGet, path, "")); c != http.StatusTooManyRequests {
		t.Errorf("bearer after mixed use = %d, want 429", c)
	}
	// Another user has a bucket of their own.
	if c := statusOf(t, ts, ts.bearer("ravi", http.MethodGet, path, "")); c == http.StatusTooManyRequests {
		t.Error("ravi throttled by asha's traffic")
	}
}

func TestRateLimitAskTighter(t *testing.T) {
	fa := &fakeAsk{}
	ts := newTestServer(t, defaultLimits, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Ask = fa
	})
	body := `{"company":"acme","month":"2026-09","question":"why?"}`
	for i := 1; i <= 3; i++ {
		if c := statusOf(t, ts, ts.bearer("asha", http.MethodPost, "/api/ask", body)); c == http.StatusTooManyRequests {
			t.Fatalf("ask %d limited early", i)
		}
	}
	w := ts.do(t, ts.bearer("asha", http.MethodPost, "/api/ask", body))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("4th ask = %d Retry-After %q, want 429 with header", w.Code, w.Header().Get("Retry-After"))
	}
	if fa.count() != 3 {
		t.Errorf("backend saw %d questions, want 3", fa.count())
	}
	// The rest of the API still has room for the same user.
	if c := statusOf(t, ts, ts.bearer("asha", http.MethodGet, "/api/close-runs/"+runID(1).String(), "")); c == http.StatusTooManyRequests {
		t.Error("ask bucket also throttled the general API")
	}
}

func TestRateLimitExemptRoutes(t *testing.T) {
	ts := newTestServer(t, defaultLimits, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
	for i := 0; i < 60; i++ {
		for _, p := range []string{"/healthz", "/runs", "/"} {
			rq := ts.as(RoleMaker, http.MethodGet, p, "", "")
			if p == "/healthz" {
				rq = req{method: http.MethodGet, path: p}
			}
			if c := statusOf(t, ts, rq); c == http.StatusTooManyRequests {
				t.Fatalf("%s limited on request %d", p, i+1)
			}
		}
	}
	// Anonymous API floods keep getting 401: they never reach a bucket.
	for i := 0; i < 30; i++ {
		if c := statusOf(t, ts, req{method: http.MethodGet, path: "/api/close-runs/x"}); c != http.StatusUnauthorized {
			t.Fatalf("anonymous = %d, want 401", c)
		}
	}
	if n := ts.limits.general.size(); n != 0 {
		t.Errorf("anonymous requests created %d limiter entries", n)
	}
}

func TestRateLimitBucketBounded(t *testing.T) {
	b := newBucketSet(1, 1)
	b.max = 50
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		b.allow(fmt.Sprint("u", i), now)
	}
	if ok, _ := b.allow("extra", now); ok {
		t.Error("new key admitted over the cap while all are active")
	}
	if ok, _ := b.allow("u3", now); ok {
		t.Error("active key got a second token")
	}
	later := now.Add(5 * time.Second)
	for i := 0; i < 500; i++ {
		b.allow(fmt.Sprint("n", i), later)
		if b.size() > 50 {
			t.Fatalf("size %d over the cap", b.size())
		}
	}
}

func TestLoginLimitUsername(t *testing.T) {
	ts := newTestServer(t)
	const a, b = "192.0.2.10:5000", "192.0.2.20:5000"
	for i := 0; i < 5; i++ {
		if w := ts.loginFrom(t, a, "asha", "wrong-password"); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, w.Code)
		}
	}
	// Locked even with the right password (so bcrypt result is not consulted)
	// and from another address.
	locked := ts.loginFrom(t, a, "asha", ts.fix.passwords["asha"])
	if locked.Code != http.StatusTooManyRequests || locked.Header().Get("Retry-After") == "" {
		t.Fatalf("locked login = %d %q", locked.Code, locked.Header().Get("Retry-After"))
	}
	if w := ts.loginFrom(t, b, "ASHA ", ts.fix.passwords["asha"]); w.Code != http.StatusTooManyRequests {
		t.Errorf("locked username from another IP (case and space varied) = %d, want 429", w.Code)
	}
	if !strings.Contains(ts.logs.String(), `"event":"login_locked"`) {
		t.Error("lockout not logged")
	}

	// An unknown user locks the same way and answers identically.
	for i := 0; i < 5; i++ {
		ts.loginFrom(t, a, "ghost", "x")
	}
	ghost := ts.loginFrom(t, b, "ghost", "x")
	asha := ts.loginFrom(t, b, "asha", "x")
	if ghost.Code != asha.Code || ghost.Body.String() != asha.Body.String() ||
		ghost.Header().Get("Retry-After") == "" || asha.Header().Get("Retry-After") == "" {
		t.Errorf("unknown and known locked answers differ: %d %q vs %d %q", ghost.Code, ghost.Body, asha.Code, asha.Body)
	}

	// A different user is unaffected, and success does not clear the lock.
	if w := ts.loginFrom(t, b, "ravi", ts.fix.passwords["ravi"]); w.Code != http.StatusSeeOther {
		t.Errorf("other user = %d, want 303", w.Code)
	}
	if w := ts.loginFrom(t, b, "asha", ts.fix.passwords["asha"]); w.Code != http.StatusTooManyRequests {
		t.Errorf("asha after another user's success = %d, want 429", w.Code)
	}

	// The lock ends with its window.
	ts.clock.Set(ts.clock.Now().Add(16 * time.Minute))
	if w := ts.loginFrom(t, b, "asha", ts.fix.passwords["asha"]); w.Code != http.StatusSeeOther {
		t.Errorf("after the window = %d, want 303", w.Code)
	}
}

func TestLoginLimitIP(t *testing.T) {
	ts := newTestServer(t)
	const a, b = "192.0.2.10:5000", "192.0.2.20:5000"
	for i := 0; i < 10; i++ {
		ts.loginFrom(t, a, fmt.Sprintf("user%d", i), "x")
	}
	if w := ts.loginFrom(t, a, "ravi", ts.fix.passwords["ravi"]); w.Code != http.StatusTooManyRequests {
		t.Errorf("locked IP = %d, want 429", w.Code)
	}
	if w := ts.loginFrom(t, b, "ravi", ts.fix.passwords["ravi"]); w.Code != http.StatusSeeOther {
		t.Errorf("another IP, another user = %d, want 303", w.Code)
	}
	// A forwarding header does not pick the bucket.
	rq := req{method: http.MethodGet, path: "/login", header: map[string]string{"X-Forwarded-For": "203.0.113.9"}}
	if got := clientIP(httptest.NewRequest(http.MethodGet, "/", nil)); got != "192.0.2.1" {
		t.Errorf("clientIP = %q", got)
	}
	_ = rq
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("Forwarded", "for=203.0.113.9")
	if got := clientIP(r); got != "192.0.2.1" {
		t.Errorf("clientIP with forwarding headers = %q, want RemoteAddr host", got)
	}
	ts.clock.Set(ts.clock.Now().Add(2 * time.Minute))
	if w := ts.loginFrom(t, a, "ravi", ts.fix.passwords["ravi"]); w.Code != http.StatusSeeOther {
		t.Errorf("IP after its window = %d, want 303", w.Code)
	}
}

func TestLoginLimitBounded(t *testing.T) {
	l := newLoginLimiter(Options{})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 20000; i++ {
		ok, _, done := l.begin(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255), fmt.Sprintf("user-%d", i), now)
		if !ok {
			t.Fatalf("attempt %d refused although nothing is locked", i)
		}
		done(false)
		if l.ip.size() > maxLimiterKeys || l.user.size() > maxLimiterKeys {
			t.Fatalf("limiter grew past the cap at %d: ip=%d user=%d", i, l.ip.size(), l.user.size())
		}
	}
	if ok, _, _ := l.begin("10.9.9.9", "never-seen", now); !ok {
		t.Error("a user with no failures was refused because the map is full")
	}
	// A long username is capped, not stored whole.
	if got := userKey(strings.Repeat("A", 500)); len(got) != maxUsernameKey {
		t.Errorf("userKey length = %d", len(got))
	}
}

func TestLoginLimitFullOfLocked(t *testing.T) {
	f := newFailSet(2, time.Hour)
	f.max = 5
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		for j := 0; j < 2; j++ {
			if _, ok, _ := f.reserve(fmt.Sprint("k", i), now); !ok {
				t.Fatalf("reserve k%d/%d refused", i, j)
			}
		}
	}
	if _, ok, _ := f.reserve("new", now); ok {
		t.Error("new key admitted while every entry is locked")
	}
	if f.size() != 5 {
		t.Errorf("a locked entry was evicted: size %d", f.size())
	}
	if _, ok, _ := f.reserve("k0", now); ok {
		t.Error("locked key admitted")
	}
	if _, ok, _ := f.reserve("new", now.Add(2*time.Hour)); !ok {
		t.Error("new key refused after windows expired")
	}
}

func TestLoginLimitConcurrent(t *testing.T) {
	ts := newTestServer(t)
	var bcrypts atomic.Int64
	ts.bcryptHook = func() { bcrypts.Add(1) }
	const n = 50
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = ts.loginFrom(t, fmt.Sprintf("192.0.2.%d:5000", 100+i), "asha", "wrong-password").Code
		}(i)
	}
	wg.Wait()
	var c401, c429 int
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			c401++
		case http.StatusTooManyRequests:
			c429++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if c401 > 5 || c401+c429 != n {
		t.Errorf("401=%d 429=%d, want at most 5 401s and the rest 429", c401, c429)
	}
	if got := bcrypts.Load(); got > 5 {
		t.Errorf("bcrypt ran %d times, want at most 5", got)
	}
}

func TestRunsGlobalCap(t *testing.T) {
	fs := newFakeRunsStore()
	fs.companies = append(fs.companies, store.Company{ID: "initech"})
	gate := make(chan struct{})
	started := make(chan struct{}, 10)
	ts := runsServer(t, fs, func(context.Context, uuid.UUID) error { started <- struct{}{}; <-gate; return nil })

	for _, c := range []string{"acme", "globex"} {
		if w := ts.startJSON(t, c, "2026-09"); w.Code != http.StatusCreated {
			t.Fatalf("start %s = %d", c, w.Code)
		}
		<-started
	}
	before := fs.createdCount()
	w := ts.startJSON(t, "initech", "2026-09")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("third start = %d Retry-After %q, want 429 and 30", w.Code, w.Header().Get("Retry-After"))
	}
	form := url.Values{"company": {"initech"}, "month": {"2026-09"}}
	wf := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/runs", form.Encode(), "application/x-www-form-urlencoded"))
	if wf.Code != http.StatusTooManyRequests || wf.Header().Get("Retry-After") != "30" {
		t.Errorf("form start = %d Retry-After %q, want 429 and 30", wf.Code, wf.Header().Get("Retry-After"))
	}
	// The per-company 409 still comes first.
	if c := ts.startJSON(t, "acme", "2026-08").Code; c != http.StatusConflict {
		t.Errorf("busy company at the cap = %d, want 409", c)
	}
	if got := fs.createdCount(); got != before {
		t.Errorf("a refused start created a row (%d -> %d)", before, got)
	}

	close(gate)
	waitIdle(t, ts)
	if c := ts.startJSON(t, "initech", "2026-09").Code; c != http.StatusCreated {
		t.Errorf("after the runs returned = %d, want 201", c)
	}
	waitIdle(t, ts)
}

func TestRunsInputValidation(t *testing.T) {
	fs := newFakeRunsStore()
	ts := runsServer(t, fs, nil)
	long := strings.Repeat("c", maxCompanyLen+1)
	if c := ts.startJSON(t, long, "2026-09").Code; c != http.StatusBadRequest {
		t.Errorf("over-long company = %d, want 400", c)
	}
	// A body over 16 KiB is refused with 413 before the handler decodes it.
	big := `{"company":"acme","month":"2026-09","pad":"` + strings.Repeat("a", maxAPIBody) + `"}`
	w := ts.do(t, ts.bearer("asha", http.MethodPost, "/api/close-runs", big))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized JSON body = %d, want 413", w.Code)
	}
	if fs.createdCount() != 0 {
		t.Error("a refused request created a run")
	}
	// A body just under the cap still reaches the handler.
	ok := `{"company":"acme","month":"2026-09"}`
	if c := ts.do(t, ts.bearer("asha", http.MethodPost, "/api/close-runs", ok)).Code; c != http.StatusCreated {
		t.Errorf("normal body = %d, want 201", c)
	}
	waitIdle(t, ts)
	// A malformed id is a 404, not a 500.
	if c := ts.do(t, ts.bearer("asha", http.MethodGet, "/api/close-runs/not-a-uuid", "")).Code; c != http.StatusNotFound {
		t.Errorf("bad run id = %d, want 404", c)
	}
}

func TestLoadNo5xx(t *testing.T) {
	fs := newFakeRunsStore()
	fs.put(store.CloseRun{ID: runID(1), CompanyID: "acme", Month: "2026-09", Status: store.RunDone})
	ts := newTestServer(t, defaultLimits, func(o *Options) {
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
	// Concurrent requests straight into the handler: a real listener here
	// makes gosec's taint analysis flag unrelated redirects in the package.
	const n = 250
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/close-runs/"+runID(1).String(), nil)
			if i%10 != 0 {
				r.Header.Set("Authorization", "Bearer "+ts.fix.tokens["asha"])
			} // else anonymous: 401
			w := httptest.NewRecorder()
			ts.ServeHTTP(w, r)
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	for c := range counts {
		if c != 200 && c != 401 && c != 403 && c != 429 {
			t.Errorf("unexpected status %d (%d times)", c, counts[c])
		}
	}
	if counts[429] == 0 || counts[200] == 0 {
		t.Errorf("want some 200s and some 429s, got %v", counts)
	}
}

func TestNoSecretsInLogsRateLimited(t *testing.T) {
	fs := newFakeRunsStore()
	ts := newTestServer(t, defaultLimits, func(o *Options) {
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
	})
	token := ts.fix.tokens["asha"]
	ck, csrf := ts.session(RoleMaker)
	path := "/api/close-runs/" + runID(1).String()
	for i := 0; i < 12; i++ { // the last ones are rate limited
		ts.do(t, ts.bearer("asha", http.MethodGet, path+"?apikey=querysecret-77", ""))
	}
	ts.do(t, req{method: http.MethodGet, path: "/runs", cookies: []*http.Cookie{ck}, header: map[string]string{csrfHeaderName: csrf}})

	badPw := "wrong-password-distinct-31337"
	const addr = "192.0.2.77:4000"
	for i := 0; i < 7; i++ { // failures, then a locked answer
		ts.loginFrom(t, addr, "asha", badPw)
	}
	loginCookie, loginTok := ts.loginForm(t)

	out := ts.logs.String()
	if !strings.Contains(out, `"status":429`) || !strings.Contains(out, "login_failed") {
		t.Fatalf("test did not exercise a 429 and a failed login:\n%s", out)
	}
	for name, secret := range map[string]string{
		"bearer token": token, "session cookie": ck.Value, "csrf token": csrf,
		"password": badPw, "good password": ts.fix.passwords["asha"], "query value": "querysecret-77",
		"login csrf token": loginTok, "login cookie": loginCookie.Value,
	} {
		if secret != "" && strings.Contains(out, secret) {
			t.Errorf("log contains the %s", name)
		}
	}
}
