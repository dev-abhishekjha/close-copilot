package mcpkit

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func rlHandler(t testing.TB, logs *lockedBuf, opts ...Option) http.Handler {
	t.Helper()
	routes := map[string]Route{
		"/mcp":       NewRoute(NewServer("a", "1"), "agent-token-aaaa"),
		AdminPath:    NewRoute(NewServer("b", "1"), "admin-token-bbbb"),
		"/mcp-other": NewRoute(NewServer("c", "1"), "agent-token-aaaa"),
	}
	var w interface{ Write([]byte) (int, error) } = &bytes.Buffer{}
	if logs != nil {
		w = logs
	}
	return BuildHandler(routes, slog.New(slog.NewJSONHandler(w, nil)), opts...)
}

func rlCall(h http.Handler, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(initBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRateLimitOverLimit(t *testing.T) {
	h := rlHandler(t, nil, WithRateLimit(0.001, 3))
	for i := 0; i < 3; i++ {
		if w := rlCall(h, "/mcp", "agent-token-aaaa"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i+1)
		}
	}
	w := rlCall(h, "/mcp", "agent-token-aaaa")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q", ra)
	}
	if !strings.Contains(w.Body.String(), "Too Many Requests") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestRateLimitPerToken(t *testing.T) {
	h := rlHandler(t, nil, WithRateLimit(0.001, 2))
	for i := 0; i < 2; i++ {
		rlCall(h, "/mcp", "agent-token-aaaa")
	}
	if w := rlCall(h, "/mcp", "agent-token-aaaa"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("agent over limit = %d", w.Code)
	}
	if w := rlCall(h, AdminPath, "admin-token-bbbb"); w.Code == http.StatusTooManyRequests {
		t.Fatal("admin token shares the agent bucket")
	}
}

func TestRateLimitBadTokenNoEntry(t *testing.T) {
	l := newTokenLimiter(1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireBearer("good", rateLimited(l, next))
	for i := 0; i < 50; i++ {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", fmt.Sprintf("Bearer bad-%d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad token = %d, want 401", w.Code)
		}
	}
	if n := l.size(); n != 0 {
		t.Fatalf("limiter entries = %d, want 0", n)
	}
}

func TestRateLimitHealthzExempt(t *testing.T) {
	h := rlHandler(t, nil, WithRateLimit(0.001, 1))
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("healthz = %d", w.Code)
		}
	}
}

func TestRateLimitDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{{"zero", WithRateLimit(0, 0)}, {"negative", WithRateLimit(-1, 5)}} {
		t.Run(tc.name, func(t *testing.T) {
			h := rlHandler(t, nil, tc.opt)
			for i := 0; i < 300; i++ {
				if w := rlCall(h, "/mcp", "agent-token-aaaa"); w.Code == http.StatusTooManyRequests {
					t.Fatalf("request %d limited while disabled", i)
				}
			}
		})
	}
}

func TestRateLimitDefaultOn(t *testing.T) {
	h := rlHandler(t, nil)
	limited := false
	for i := 0; i < defaultRateBurst+50; i++ {
		if rlCall(h, "/mcp", "agent-token-aaaa").Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("default limit never applied")
	}
}

func TestRateLimitBounded(t *testing.T) {
	l := newTokenLimiter(1, 1)
	l.max = 10
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		l.allow(fmt.Sprint("k", i)) // each uses its only token: active
	}
	if ok, _ := l.allow("new"); ok {
		t.Fatal("new key admitted over the cap while all are active")
	}
	if l.size() != 10 {
		t.Fatalf("size = %d", l.size())
	}
	now = now.Add(2 * time.Second) // all idle again
	if ok, _ := l.allow("new"); !ok {
		t.Fatal("new key refused after idle entries could be swept")
	}
	if l.size() > 10 {
		t.Fatalf("size = %d", l.size())
	}
}

func TestLoadNo5xx(t *testing.T) {
	srv := httptest.NewServer(rlHandler(t, nil, WithRateLimit(5, 10)))
	defer srv.Close()
	const n = 250
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer agent-token-aaaa")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- -1
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	got429 := 0
	for c := range codes {
		if c < 0 || c >= 500 {
			t.Fatalf("status %d", c)
		}
		if c == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 == 0 {
		t.Fatal("no 429 under load")
	}
}

func TestNoSecretsInLogs(t *testing.T) {
	logs := &lockedBuf{}
	h := rlHandler(t, logs, WithRateLimit(0.001, 1))
	rlCall(h, "/mcp", "agent-token-aaaa")
	rlCall(h, "/mcp", "agent-token-aaaa") // 429
	rlCall(h, "/mcp", "wrong-token-zzzz") // 401
	r := httptest.NewRequest(http.MethodPost, "/mcp?secret=querysecret-123", nil)
	r.Header.Set("Authorization", "Bearer agent-token-aaaa")
	r.AddCookie(&http.Cookie{Name: "c", Value: "cookiesecret-456"})
	h.ServeHTTP(httptest.NewRecorder(), r)
	out := logs.String()
	if out == "" {
		t.Fatal("no log output captured")
	}
	for _, s := range []string{"agent-token-aaaa", "admin-token-bbbb", "wrong-token-zzzz", "querysecret-123", "cookiesecret-456"} {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q", s)
		}
	}
}
