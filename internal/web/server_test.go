package web

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	ck, _ := ts.session(RoleViewer)
	for _, rq := range []req{
		{method: http.MethodGet, path: "/login"},
		{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}},
		{method: http.MethodGet, path: "/api/close-runs/run-1"},
		{method: http.MethodGet, path: "/healthz"},
	} {
		t.Run(rq.path, func(t *testing.T) {
			h := ts.do(t, rq).Header()
			if h.Get("X-Content-Type-Options") != "nosniff" {
				t.Error("X-Content-Type-Options missing")
			}
			if h.Get("Referrer-Policy") != "same-origin" {
				t.Error("Referrer-Policy missing")
			}
			csp := h.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self';", "script-src 'self';", "frame-ancestors 'none'", "object-src 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP %q lacks %q", csp, want)
				}
			}
			for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "jsdelivr", "*"} {
				if strings.Contains(csp, bad) {
					t.Errorf("CSP allows %q: %q", bad, csp)
				}
			}
		})
	}
}

func TestBaseLayout(t *testing.T) {
	ts := newTestServer(t)
	body := ts.do(t, req{method: http.MethodGet, path: "/login"}).Body.String()
	for _, want := range []string{
		`src="/static/htmx.min.js"`,
		`integrity="sha384-HGfztofotfshcF7+8n44JQL2oJmowVChPTg48S+jvZoztPfvwD79OC/LTtG6dMp+"`,
		`crossorigin="anonymous"`,
		`&#34;includeIndicatorStyles&#34;:false`,
		`&#34;allowEval&#34;:false`,
		`&#34;selfRequestsOnly&#34;:true`,
		`href="/static/app.css"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("layout lacks %s", want)
		}
	}
	for _, bad := range []string{"hx-on", "<style", "style=", "jsdelivr", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("layout contains %q (blocked by CSP)", bad)
		}
	}
}

func TestPanicRecovery(t *testing.T) {
	ts := newTestServer(t)
	h := ts.logRequests(ts.securityHeaders(ts.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail " + testKey)
	}))))
	for _, path := range []string{"/x", "/api/x"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "secret detail") {
			t.Errorf("%s: response leaks the panic value", path)
		}
	}
	if logs := ts.logs.String(); strings.Contains(logs, testKey) || !strings.Contains(logs, "handler panic") {
		t.Errorf("panic log wrong: %s", logs)
	}
}

func TestBodyLimit(t *testing.T) {
	ts := newTestServer(t)
	ck, _ := ts.session(RoleMaker)
	big := "csrf_token=" + strings.Repeat("a", maxBodyBytes+1)
	w := ts.do(t, req{method: http.MethodPost, path: "/api/close-runs", body: big, contentType: "application/x-www-form-urlencoded", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusForbidden {
		t.Errorf("oversized body = %d, want 403 (token unreadable)", w.Code)
	}
}

func TestStaticAndHealthz(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, req{method: http.MethodGet, path: "/static/app.css"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("app.css = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	w = ts.do(t, req{method: http.MethodGet, path: "/healthz"})
	body, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Errorf("healthz = %d %s", w.Code, body)
	}
	// The vendored htmx is served from this origin and matches the layout's
	// Subresource Integrity hash.
	w = ts.do(t, req{method: http.MethodGet, path: "/static/htmx.min.js"})
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("htmx.min.js = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	sum := sha512.Sum384(w.Body.Bytes())
	if got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); !strings.Contains(ts.do(t, req{method: http.MethodGet, path: "/login"}).Body.String(), `integrity="`+got+`"`) {
		t.Errorf("served htmx.min.js hash %s does not match the layout's integrity attribute", got)
	}
	if got := ts.do(t, req{method: http.MethodGet, path: "/static/missing.css"}).Code; got != http.StatusNotFound {
		t.Errorf("missing static file = %d, want 404", got)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestOptionsFromConfig(t *testing.T) {
	opts := OptionsFromConfig(config.Config{AppAddr: ":9001", AppSessionKey: config.NewSecret(testKey)})
	if opts.Addr != ":9001" || opts.SessionKey.Reveal() != testKey || opts.InsecureCookies {
		t.Errorf("OptionsFromConfig = %+v", opts)
	}
	ts := newTestServer(t, func(o *Options) { o.Addr = "" })
	if ts.addr != defaultAddr {
		t.Errorf("addr = %q, want %q", ts.addr, defaultAddr)
	}
}
