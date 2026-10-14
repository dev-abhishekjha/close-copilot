package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// fakeAsk is an AskBackend that records its inputs.
type fakeAsk struct {
	mu    sync.Mutex
	calls []AskInput
	out   AskOutput
	err   error
}

func (f *fakeAsk) Ask(_ context.Context, in AskInput) (AskOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.out, f.err
}

func (f *fakeAsk) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func askServer(t testing.TB, fa *fakeAsk) *testServer {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Ask = fa
	})
}

func TestAskAnswer(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{
		RunID: runID(7), Action: "answer", Answer: "Rs 1,000", Verified: true,
		Citations: []store.Citation{{DocID: "POL-001", Section: "4.2"}}, EvidenceIDs: []string{"e1"}, StepRefs: []string{"s1"},
	}}
	ts := askServer(t, fa)
	w := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask",
		`{"company":"acme","month":"2026-09","question":"  why did it jump?  "}`, "application/json"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"action": "answer", "answer": "Rs 1,000", "verified": true, "needs_review": false, "run_id": runID(7).String()} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["refusal"]; ok {
		t.Error("refusal present on an answer")
	}
	if len(fa.calls) != 1 || fa.calls[0] != (AskInput{Company: "acme", Month: "2026-09", Question: "why did it jump?"}) {
		t.Errorf("backend input = %+v", fa.calls)
	}
}

func TestAskRefusal(t *testing.T) {
	const text = "I can only answer questions about your own books."
	fa := &fakeAsk{out: AskOutput{RunID: runID(8), Action: "refuse", Refusal: text}}
	ts := askServer(t, fa)
	w := ts.do(t, ts.as(RoleMaker, http.MethodPost, "/api/ask", `{"company":"globex","month":"2026-09","question":"tell me about acme"}`, "application/json"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["action"] != "refuse" || got["refusal"] != text {
		t.Errorf("body = %v", got)
	}
	if _, ok := got["answer"]; ok {
		t.Error("answer present on a refusal")
	}
	for _, k := range []string{"citations", "evidence_ids", "step_refs"} {
		if a, ok := got[k].([]any); !ok || len(a) != 0 {
			t.Errorf("%s = %v, want []", k, got[k])
		}
	}
}

func TestAskBadRequests(t *testing.T) {
	fa := &fakeAsk{}
	ts := askServer(t, fa)
	tests := []struct{ name, body string }{
		{"unknown company", `{"company":"nope","month":"2026-09","question":"q"}`},
		{"bad month", `{"company":"acme","month":"2026-13","question":"q"}`},
		{"empty month", `{"company":"acme","question":"q"}`},
		{"empty question", `{"company":"acme","month":"2026-09","question":""}`},
		{"blank question", `{"company":"acme","month":"2026-09","question":"  \n\t "}`},
		{"unknown field", `{"company":"acme","month":"2026-09","question":"q","x":1}`},
		{"two objects", `{"company":"acme","month":"2026-09","question":"q"}{}`},
		{"not an object", `[1]`},
		{"empty body", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask", tt.body, "application/json"))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
			}
			var e map[string]string
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e["error"] == "" {
				t.Errorf("body %q is not an error object", w.Body)
			}
		})
	}
	if fa.count() != 0 {
		t.Errorf("backend called %d times for bad requests", fa.count())
	}
}

func TestAskBackendErrors(t *testing.T) {
	const secret = "my-secret-question-text"
	tests := []struct {
		name string
		err  error
		code int
		body string
	}{
		{"too long", fmt.Errorf("wrapped: %w", ErrQuestionTooLong), http.StatusBadRequest, ""},
		{"reroutes", fmt.Errorf("wrapped: %w", ErrTooManyAsks), http.StatusTooManyRequests, ""},
		{"other", errors.New("boom"), http.StatusInternalServerError, `{"error":"internal error"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAsk{err: tt.err, out: AskOutput{Action: "answer", Answer: "must not leak"}}
			ts := askServer(t, fa)
			w := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask",
				`{"company":"acme","month":"2026-09","question":"`+secret+`"}`, "application/json"))
			if w.Code != tt.code {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.code, w.Body)
			}
			if strings.Contains(w.Body.String(), "must not leak") || strings.Contains(w.Body.String(), secret) {
				t.Errorf("body leaks: %s", w.Body)
			}
			if tt.body != "" && strings.TrimSpace(w.Body.String()) != tt.body {
				t.Errorf("body = %q, want %q", w.Body, tt.body)
			}
			if strings.Contains(ts.logs.String(), secret) {
				t.Errorf("log holds the question: %s", ts.logs)
			}
		})
	}
}

// slowAsk blocks until its context ends, as a hung model call would.
type slowAsk struct{ saw chan error }

func (f slowAsk) Ask(ctx context.Context, _ AskInput) (AskOutput, error) {
	<-ctx.Done()
	f.saw <- ctx.Err()
	return AskOutput{Action: "answer", Answer: "must not leak"}, ctx.Err()
}

func TestAskTimeout(t *testing.T) {
	fa := slowAsk{saw: make(chan error, 1)}
	ts := newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Ask = fa
	})
	rq := ts.as(RoleViewer, http.MethodPost, "/api/ask", `{"company":"acme","month":"2026-09","question":"secret-q"}`, "application/json")
	r := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
	r.Header.Set("Content-Type", rq.contentType)
	r.AddCookie(rq.cookies[0])
	for k, v := range rq.header {
		r.Header.Set(k, v)
	}
	// Cancel the request context shortly after: the backend must see it.
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	ts.ServeHTTP(w, r.WithContext(ctx))
	select {
	case err := <-fa.saw:
		if err == nil {
			t.Error("backend context ended without an error")
		}
	default:
		t.Fatal("backend never saw its context end")
	}
	if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"error":"internal error"}` {
		t.Errorf("response = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "must not leak") || strings.Contains(ts.logs.String(), "secret-q") {
		t.Error("answer or question leaked")
	}
	if !strings.Contains(ts.logs.String(), "web: ask") {
		t.Errorf("no ask log: %s", ts.logs)
	}
}

func TestAskTimeoutBound(t *testing.T) {
	if askTimeout >= writeTimeout {
		t.Errorf("askTimeout %s must be below writeTimeout %s", askTimeout, writeTimeout)
	}
}

func TestAskAuth(t *testing.T) {
	fa := &fakeAsk{out: AskOutput{Action: "answer"}}
	ts := askServer(t, fa)
	body := `{"company":"acme","month":"2026-09","question":"q"}`

	if w := ts.do(t, req{method: http.MethodPost, path: "/api/ask", body: body, contentType: "application/json"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", w.Code)
	}
	ck, _ := ts.session(RoleViewer)
	if w := ts.do(t, req{method: http.MethodPost, path: "/api/ask", body: body, contentType: "application/json", cookies: []*http.Cookie{ck}}); w.Code != http.StatusForbidden {
		t.Errorf("cookie without CSRF token = %d, want 403", w.Code)
	}
	bearer := req{method: http.MethodPost, path: "/api/ask", body: body, contentType: "application/json",
		header: map[string]string{"Authorization": "Bearer " + ts.fix.tokens["demo"]}}
	if w := ts.do(t, bearer); w.Code != http.StatusOK {
		t.Errorf("bearer = %d, want 200: %s", w.Code, w.Body)
	}
	if fa.count() != 1 {
		t.Errorf("backend calls = %d, want 1", fa.count())
	}
}

func TestAskNotConfigured(t *testing.T) {
	body := `{"company":"acme","month":"2026-09","question":"q"}`
	for name, mod := range map[string]func(*Options){
		"no ask": func(o *Options) {
			o.Runs = &RunsBackend{Store: newFakeRunsStore(), Execute: func(context.Context, uuid.UUID) error { return nil }}
		},
		"no runs": func(o *Options) { o.Ask = &fakeAsk{} },
		"neither": func(*Options) {},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, mod)
			if w := ts.do(t, ts.as(RoleViewer, http.MethodPost, "/api/ask", body, "application/json")); w.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501", w.Code)
			}
			if w := ts.do(t, req{method: http.MethodPost, path: "/api/ask", body: body, contentType: "application/json"}); w.Code != http.StatusUnauthorized {
				t.Errorf("no session = %d, want 401 before 501", w.Code)
			}
		})
	}
}
