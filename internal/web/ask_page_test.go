package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const formCT = "application/x-www-form-urlencoded"

func askFormBody(company, month, question string) string {
	return url.Values{"company": {company}, "month": {month}, "question": {question}}.Encode()
}

// postAsk posts the Ask form as role with a valid CSRF header.
func postAsk(t testing.TB, ts *testServer, role Role, company, month, question string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.do(t, ts.as(role, http.MethodPost, "/ask", askFormBody(company, month, question), formCT))
}

var reTextarea = regexp.MustCompile(`(?s)<textarea[^>]*>(.*?)</textarea>`)

func textareaValue(t testing.TB, body string) string {
	t.Helper()
	m := reTextarea.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no textarea in page")
	}
	return html.UnescapeString(m[1])
}

func TestAskPageForm(t *testing.T) {
	ts := askServer(t, &fakeAsk{})
	w := ts.get(t, RoleViewer, "/ask")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Ask the books", `action="/ask"`, `name="csrf_token"`, `name="company"`, `name="month"`, `name="question"`,
		`<option value="acme" selected>`, `<option value="globex">`, `value="2026-09"`, `placeholder="YYYY-MM"`,
		`maxlength="400"`, "Max 400 characters", `href="/ask?q=`, "Ask",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `id="ask-result"`) {
		t.Error("an empty page shows a result area")
	}

	t.Run("prefill is escaped", func(t *testing.T) {
		w := ts.get(t, RoleViewer, "/ask?q="+url.QueryEscape(`  <script>alert(1)</script> "x"  `))
		if strings.Contains(w.Body.String(), "<script>alert(1)") {
			t.Error("prefill is not escaped")
		}
		if got := textareaValue(t, w.Body.String()); got != `<script>alert(1)</script> "x"` {
			t.Errorf("prefill = %q", got)
		}
	})
	t.Run("prefill is cut on a rune boundary", func(t *testing.T) {
		long := "a" + strings.Repeat("é", 300) // 601 bytes; byte 400 is mid-rune
		got := textareaValue(t, ts.get(t, RoleViewer, "/ask?q="+url.QueryEscape(long)).Body.String())
		if len(got) > 400 || !utf8.ValidString(got) || got != "a"+strings.Repeat("é", 199) {
			t.Errorf("prefill = %d bytes, valid=%v", len(got), utf8.ValidString(got))
		}
	})
	t.Run("prefill is shown nowhere else", func(t *testing.T) {
		body := ts.get(t, RoleViewer, "/ask?q=zzqmarker").Body.String()
		if n := strings.Count(body, "zzqmarker"); n != 1 {
			t.Errorf("prefill appears %d times, want 1 (the textarea)", n)
		}
	})
	t.Run("a post rendering is not stored: reload is empty", func(t *testing.T) {
		if strings.Contains(ts.get(t, RoleViewer, "/ask").Body.String(), "ask-result") {
			t.Error("reload shows a result")
		}
	})
}

func TestAskPageAnswer(t *testing.T) {
	const answer = "Total <script>alert(1)</script> **bold** # head\n- item"
	fa := &fakeAsk{out: AskOutput{
		RunID: runID(1), Action: "answer", Answer: answer, Verified: true,
		Citations:   []store.Citation{{DocID: "POL-001", Section: "4.2 <b>x</b>"}},
		EvidenceIDs: []string{"ev-1"}, StepRefs: []string{"step-9"},
	}}
	ts := askServer(t, fa)
	w := postAsk(t, ts, RoleViewer, "acme", "2026-09", "  why?  ")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="ask-result"`, `class="ask-answer"`, "Verified", "POL-001", "How this was checked", "<code>ev-1</code>", "<code>step-9</code>",
		html.EscapeString(answer), "4.2 &lt;b&gt;x&lt;/b&gt;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script>alert", "<b>x</b>", "<strong>", "<h1>head", "<li>item"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains interpreted markup %q", bad)
		}
	}
	if strings.Contains(body, "Needs review: this answer") {
		t.Error("a verified answer shows the needs-review warning")
	}
	if got := textareaValue(t, body); got != "why?" {
		t.Errorf("question echo = %q", got)
	}
	if len(fa.calls) != 1 || fa.calls[0] != (AskInput{Company: "acme", Month: "2026-09", Question: "why?"}) {
		t.Errorf("backend input = %+v", fa.calls)
	}
}

func TestAskPageNeedsReview(t *testing.T) {
	amt := money.Paise(1234567)
	fa := &fakeAsk{out: AskOutput{
		Action: "answer", Answer: "maybe", NeedsReview: true,
		Violations: []AskViolation{
			{Code: "untraced_number", Field: "total", ValuePaise: &amt, Detail: "no evidence <i>row</i>"},
			{Code: "no_value", Field: "x", Detail: "d"},
		},
	}}
	ts := askServer(t, fa)
	body := postAsk(t, ts, RoleMaker, "acme", "2026-09", "q").Body.String()
	for _, want := range []string{
		"Needs review: this answer could not be fully verified.", "untraced_number", "total", amt.Format(),
		"no evidence &lt;i&gt;row&lt;/i&gt;", "no_value", "Unverified",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<i>row") {
		t.Error("violation detail is not escaped")
	}
}

func TestAskPageRefuseClarify(t *testing.T) {
	tests := []struct {
		action, text, title string
		tone                string
	}{
		{"refuse", "I can only answer about your own books.", "Not answered", "ui-tone-default"},
		{"clarify", "Which month do you mean?", "A question first", "ui-tone-info"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			ts := askServer(t, &fakeAsk{out: AskOutput{Action: tt.action, Refusal: tt.text}})
			w := postAsk(t, ts, RoleChecker, "globex", "2026-08", "my question")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			body := w.Body.String()
			for _, want := range []string{tt.text, tt.title, tt.tone, `action="/ask"`, `<option value="globex" selected>`, `value="2026-08"`} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q", want)
				}
			}
			if got := textareaValue(t, body); got != "my question" {
				t.Errorf("question echo = %q", got)
			}
			if strings.Contains(body, "How this was checked") {
				t.Error("a refusal shows the verification details")
			}
		})
	}
	t.Run("unknown action", func(t *testing.T) {
		ts := askServer(t, &fakeAsk{out: AskOutput{Action: "mystery", Answer: "SECRETTEXT"}})
		body := postAsk(t, ts, RoleViewer, "acme", "2026-09", "q").Body.String()
		if !strings.Contains(body, "No answer was produced.") || strings.Contains(body, "SECRETTEXT") {
			t.Error("unknown action must render only the generic line")
		}
	})
}

func TestAskPageValidation(t *testing.T) {
	tests := []struct {
		name, company, month, question string
		backendErr                     error
		wantMsg                        string
	}{
		{"empty question", "acme", "2026-09", "  \n ", nil, "question must not be empty"},
		{"too long", "acme", "2026-09", "q", ErrQuestionTooLong, "question is too long"},
		{"bad month", "acme", "2026-13", "keep <me>", nil, "month must be YYYY-MM"},
		{"empty month", "acme", "", "q", nil, "month must be YYYY-MM"},
		{"unknown company", "nope", "2026-09", "q", nil, "unknown company"},
		{"no company", "", "2026-09", "q", nil, "no company selected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAsk{err: tt.backendErr}
			ts := askServer(t, fa)
			// The API's status for the same input.
			api := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask",
				fmt.Sprintf(`{"company":%q,"month":%q,"question":%q}`, tt.company, tt.month, tt.question), "application/json"))
			w := postAsk(t, ts, RoleViewer, tt.company, tt.month, tt.question)
			if w.Code != http.StatusBadRequest || api.Code != w.Code {
				t.Fatalf("form = %d, api = %d, want both 400: %s", w.Code, api.Code, w.Body)
			}
			body := w.Body.String()
			if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, tt.wantMsg) {
				t.Errorf("page lacks alert %q", tt.wantMsg)
			}
			if strings.Contains(body, `id="ask-result"`) {
				t.Error("an error shows a result")
			}
			if q := strings.TrimSpace(tt.question); q != "" && textareaValue(t, body) != q {
				t.Errorf("question echo = %q, want %q", textareaValue(t, body), q)
			}
			if strings.Contains(body, "<me>") {
				t.Error("echo is not escaped")
			}
		})
	}
	t.Run("form values are kept", func(t *testing.T) {
		ts := askServer(t, &fakeAsk{})
		body := postAsk(t, ts, RoleViewer, "globex", "2026-13", "q").Body.String()
		if !strings.Contains(body, `<option value="globex" selected>`) || !strings.Contains(body, `value="2026-13"`) {
			t.Error("company or month not kept")
		}
	})
}

func TestAskPageBackendErrors(t *testing.T) {
	const secret = "my-secret-question-text"
	tests := []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"too many", fmt.Errorf("wrapped: %w", ErrTooManyAsks), http.StatusTooManyRequests, "too many attempts for this question"},
		{"internal", fmt.Errorf("model exploded"), http.StatusInternalServerError, "internal error"},
		{"deadline", context.DeadlineExceeded, http.StatusInternalServerError, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := askServer(t, &fakeAsk{err: tt.err})
			w := postAsk(t, ts, RoleViewer, "acme", "2026-09", secret)
			if w.Code != tt.code {
				t.Fatalf("status = %d, want %d", w.Code, tt.code)
			}
			body := w.Body.String()
			if !strings.Contains(body, tt.msg) || strings.Contains(body, "exploded") {
				t.Errorf("body message wrong: %q", tt.msg)
			}
			if strings.Contains(ts.logs.String(), secret) {
				t.Errorf("the question reached the log: %s", ts.logs.String())
			}
			if tt.code == http.StatusInternalServerError && !strings.Contains(ts.logs.String(), "web: ask") {
				t.Error("the failure was not logged")
			}
		})
	}
}

func TestAskPageRateLimitShared(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: "ok"}}
	ts := newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Ask = fa
		o.AskRate, o.AskBurst = 0.001, 3
	})
	api := func(role Role) *httptest.ResponseRecorder {
		return ts.do(t, ts.as(role, http.MethodPost, "/api/ask", `{"company":"acme","month":"2026-09","question":"q"}`, "application/json"))
	}
	for i, call := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return api(RoleViewer) },
		func() *httptest.ResponseRecorder { return postAsk(t, ts, RoleViewer, "acme", "2026-09", "q") },
		func() *httptest.ResponseRecorder { return api(RoleViewer) },
	} {
		if w := call(); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: %s", i+1, w.Code, w.Body)
		}
	}
	// The fourth, on either route, is refused and does not reach the backend.
	w := postAsk(t, ts, RoleViewer, "acme", "2026-09", "keep this question")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("form after burst = %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want HTML", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "You are asking too fast. Try again in a few seconds.") || !strings.Contains(body, `action="/ask"`) {
		t.Error("429 is not the Ask page with the alert")
	}
	if textareaValue(t, body) != "keep this question" {
		t.Error("the question was not kept on a throttled post")
	}
	if a := api(RoleViewer); a.Code != http.StatusTooManyRequests || a.Header().Get("Retry-After") == "" {
		t.Errorf("api after burst = %d", a.Code)
	}
	if fa.count() != 3 {
		t.Errorf("backend calls = %d, want 3", fa.count())
	}
	// Another user has their own allowance.
	if o := postAsk(t, ts, RoleMaker, "acme", "2026-09", "q"); o.Code != http.StatusOK {
		t.Errorf("other user = %d, want 200", o.Code)
	}
	// GET /ask is never throttled.
	for i := 0; i < 10; i++ {
		if g := ts.get(t, RoleViewer, "/ask"); g.Code != http.StatusOK {
			t.Fatalf("GET /ask #%d = %d", i, g.Code)
		}
	}
}

func TestAskPageCSRF(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: "ok"}}
	ts := askServer(t, fa)
	ck, csrf := ts.session(RoleViewer)
	body := askFormBody("acme", "2026-09", "q") + "&csrf_token="
	tests := []struct {
		name string
		rq   req
		want int
	}{
		{"no token", req{method: http.MethodPost, path: "/ask", body: askFormBody("acme", "2026-09", "q"), contentType: formCT, cookies: []*http.Cookie{ck}}, http.StatusForbidden},
		{"wrong token", req{method: http.MethodPost, path: "/ask", body: body + "nope", contentType: formCT, cookies: []*http.Cookie{ck}}, http.StatusForbidden},
		{"cross-site", req{method: http.MethodPost, path: "/ask", body: body + url.QueryEscape(csrf), contentType: formCT, cookies: []*http.Cookie{ck},
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},
		{"foreign origin", req{method: http.MethodPost, path: "/ask", body: body + url.QueryEscape(csrf), contentType: formCT, cookies: []*http.Cookie{ck},
			header: map[string]string{"Origin": "https://evil.example"}}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := ts.do(t, tt.rq); w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
	if fa.count() != 0 {
		t.Errorf("backend called %d times without a valid token", fa.count())
	}
	ok := req{method: http.MethodPost, path: "/ask", body: body + url.QueryEscape(csrf), contentType: formCT, cookies: []*http.Cookie{ck}}
	if w := ts.do(t, ok); w.Code != http.StatusOK || fa.count() != 1 {
		t.Errorf("form token: status %d, calls %d", w.Code, fa.count())
	}
}

func TestAskPageRoles(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: "ok"}}
	ts := askServer(t, fa)
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		t.Run(string(role), func(t *testing.T) {
			if w := ts.get(t, role, "/ask"); w.Code != http.StatusOK {
				t.Errorf("GET = %d", w.Code)
			}
			if w := postAsk(t, ts, role, "acme", "2026-09", "q"); w.Code != http.StatusOK {
				t.Errorf("POST = %d", w.Code)
			}
		})
	}
	if w := ts.do(t, req{method: http.MethodGet, path: "/ask"}); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous GET = %d %q, want the login redirect", w.Code, w.Header().Get("Location"))
	}
	if w := ts.do(t, req{method: http.MethodPost, path: "/ask", body: askFormBody("acme", "2026-09", "q"), contentType: formCT}); w.Code == http.StatusOK {
		t.Errorf("anonymous POST = %d", w.Code)
	}
	if fa.count() != 3 {
		t.Errorf("backend calls = %d, want 3", fa.count())
	}
}

func TestAskPageNotConfigured(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"no ask": func(o *Options) {
			o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		},
		"no runs": func(o *Options) { o.Ask = &fakeAsk{} },
		"neither": func(*Options) {},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, mod)
			for _, w := range []*httptest.ResponseRecorder{
				ts.get(t, RoleViewer, "/ask"),
				postAsk(t, ts, RoleViewer, "acme", "2026-09", "q"),
			} {
				if w.Code != http.StatusNotImplemented {
					t.Fatalf("status = %d, want 501", w.Code)
				}
				if !strings.Contains(w.Body.String(), "Ask is not set up on this server") || strings.Contains(w.Body.String(), `name="question"`) {
					t.Error("not the friendly 501 page")
				}
			}
			if w := ts.do(t, req{method: http.MethodGet, path: "/ask"}); w.Code != http.StatusSeeOther {
				t.Errorf("anonymous = %d, want the login redirect before 501", w.Code)
			}
		})
	}
}

func TestAskPageNav(t *testing.T) {
	ts := askServer(t, &fakeAsk{out: AskOutput{Action: "answer", Answer: "Rs 1,000", Verified: true}})
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		ck, _ := ts.session(role)
		w := ts.do(t, req{method: http.MethodGet, path: "/ask", cookies: []*http.Cookie{ck}})
		if nav := mainNav(t, w.Body.String()); !strings.Contains(nav, `href="/ask"`) {
			t.Errorf("%s: Ask not in the sidebar", role)
		}
	}
	// The API is unchanged by the page.
	w := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask", `{"company":"acme","month":"2026-09","question":"q"}`, "application/json"))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"answer":"Rs 1,000"`) {
		t.Errorf("api = %d %s", w.Code, w.Body)
	}
	w = ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask", `{"company":"nope","month":"2026-09","question":"q"}`, "application/json"))
	if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != `{"error":"unknown company"}` {
		t.Errorf("api error = %d %s", w.Code, w.Body)
	}
}

func TestAskPageNoStore(t *testing.T) {
	const q, a = "qqq-secret-question", "aaa-secret-answer"
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: a, Verified: true}}
	ts := askServer(t, fa)
	w := postAsk(t, ts, RoleViewer, "acme", "2026-09", q)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), a) {
		t.Fatalf("status = %d", w.Code)
	}
	for k, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, q) || strings.Contains(v, a) {
				t.Errorf("header %s carries question or answer", k)
			}
		}
	}
	if strings.Contains(ts.logs.String(), q) || strings.Contains(ts.logs.String(), a) {
		t.Errorf("log carries question or answer: %s", ts.logs.String())
	}
	// A reload (GET) shows neither.
	if body := ts.get(t, RoleViewer, "/ask").Body.String(); strings.Contains(body, q) || strings.Contains(body, a) {
		t.Error("the page remembers the last ask")
	}
}

func TestAskPageBodyLimit(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: "ok"}}
	ts := askServer(t, fa)
	big := askFormBody("acme", "2026-09", strings.Repeat("x", maxAPIBody+1))
	// With the token in the header, the body is not parsed by the CSRF check;
	// the handler sees an unreadable form, so it asks nothing.
	w := postAskRaw(t, ts, big, true)
	if w.Code == http.StatusOK || w.Code < 400 {
		t.Errorf("oversized form with header token = %d, want an error", w.Code)
	} else {
		t.Logf("oversized form with header token = %d", w.Code)
	}
	// With the token in the body, the CSRF check cannot read it: 403.
	w = postAskRaw(t, ts, big, false)
	if w.Code != http.StatusForbidden {
		t.Errorf("oversized form with body token = %d, want 403", w.Code)
	}
	if fa.count() != 0 {
		t.Errorf("backend called %d times for an oversized body", fa.count())
	}
	if w := postAsk(t, ts, RoleViewer, "acme", "2026-09", strings.Repeat("x", 300)); w.Code != http.StatusOK || fa.count() != 1 {
		t.Errorf("normal post = %d, calls %d", w.Code, fa.count())
	}
}

func postAskRaw(t testing.TB, ts *testServer, body string, headerToken bool) *httptest.ResponseRecorder {
	t.Helper()
	ck, csrf := ts.session(RoleViewer)
	rq := req{method: http.MethodPost, path: "/ask", contentType: formCT, cookies: []*http.Cookie{ck}}
	if headerToken {
		rq.body = body
		rq.header = map[string]string{csrfHeaderName: csrf}
	} else {
		rq.body = "csrf_token=" + url.QueryEscape(csrf) + "&" + body
	}
	return ts.do(t, rq)
}

func TestAskPageRateLimitEchoGate(t *testing.T) {
	const q = "echo-gate <b>question</b>"
	fa := &fakeAsk{out: AskOutput{Action: "answer", Answer: "ok"}}
	ts := newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Ask = fa
		o.AskRate, o.AskBurst = 0.001, 1
	})
	ck, csrf := ts.session(RoleViewer)
	post := func(token string, hdr map[string]string) *httptest.ResponseRecorder {
		return ts.do(t, req{method: http.MethodPost, path: "/ask", contentType: formCT, cookies: []*http.Cookie{ck}, header: hdr,
			body: askFormBody("acme", "2026-09", q) + "&csrf_token=" + url.QueryEscape(token)})
	}
	if w := post(csrf, nil); w.Code != http.StatusOK {
		t.Fatalf("first post = %d", w.Code)
	}
	tests := []struct {
		name  string
		token string
		hdr   map[string]string
		echo  bool
	}{
		{"wrong token", "nope", nil, false},
		{"cross-site", csrf, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"valid", csrf, map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := post(tt.token, tt.hdr)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, "<b>question</b>") {
				t.Error("question echoed unescaped")
			}
			if got := strings.Contains(body, "echo-gate &lt;b&gt;question&lt;/b&gt;"); got != tt.echo {
				t.Errorf("question echoed = %v, want %v", got, tt.echo)
			}
		})
	}
	if fa.count() != 1 {
		t.Errorf("backend calls = %d, want 1", fa.count())
	}
}
