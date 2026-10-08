package frappe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// Test credentials. The secret is distinctive so a leak is easy to spot.
const (
	testKey    = "botkey0000000ab"
	testSecret = Secret("SECRETmustNEVERleak7f3a")
	testSite   = "erp.localhost"
)

// newTestClient starts an httptest server with h and returns a client for
// it whose retries don't sleep. edit can add URLs to the config (and so to
// the httpx allowlist).
func newTestClient(t *testing.T, h http.Handler, edit ...func(*config.Config)) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := config.Config{ERPBaseURL: srv.URL, ERPSite: testSite}
	for _, e := range edit {
		e(&cfg)
	}
	c, err := New(cfg, testKey, testSecret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c, srv
}

// waits records the retry waits a client asked for, without sleeping.
type waits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waits) sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
	return ctx.Err()
}

func (w *waits) get() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestNew(t *testing.T) {
	good := config.Config{ERPBaseURL: "http://localhost:8080/", ERPSite: testSite}
	tests := []struct {
		name   string
		cfg    config.Config
		key    string
		secret Secret
		want   string // substring of the error; empty means success
	}{
		{"ok", good, testKey, testSecret, ""},
		{"no key", good, "", testSecret, "key and secret are required"},
		{"no secret", good, testKey, "", "key and secret are required"},
		{"colon in secret", good, testKey, "a:b", "colon or whitespace"},
		{"no base URL", config.Config{}, testKey, testSecret, config.EnvERPBaseURL},
		{"not http", config.Config{ERPBaseURL: "ftp://localhost"}, testKey, testSecret, "http or https"},
		{"user info", config.Config{ERPBaseURL: "http://u:p@localhost:8080"}, testKey, testSecret, "user info"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg, tt.key, tt.secret)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if c.base != "http://localhost:8080" {
					t.Errorf("base = %q, want the URL without a trailing slash", c.base)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestHeaders(t *testing.T) {
	var (
		mu   sync.Mutex
		got  http.Header
		host string
	)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got, host = r.Header.Clone(), r.Host
		mu.Unlock()
		writeRaw(w, http.StatusOK, `{"data":{"name":"Cash - STPL"}}`)
	}))
	if _, err := Get[map[string]any](t.Context(), c, "Account", "Cash - STPL"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "token " + testKey + ":" + testSecret.reveal(); got.Get("Authorization") != want {
		t.Error("Authorization header is not token <key>:<secret>")
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
	if host != testSite {
		t.Errorf("Host = %q, want %q", host, testSite)
	}
}

func TestSecretRedacted(t *testing.T) {
	secret := testSecret.reveal()
	var sawAuth atomic.Bool
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, secret) {
			sawAuth.Store(true)
		}
		switch r.URL.Path {
		case "/api/resource/Echo/json":
			// A misbehaving server that quotes the credentials back.
			sm, _ := json.Marshal([]string{`{"message":"bad header ` + auth + `"}`})
			writeJSON(w, http.StatusExpectationFailed, map[string]any{
				"exc_type":         "ValidationError",
				"exception":        "frappe.exceptions.ValidationError: " + auth,
				"_server_messages": string(sm),
			})
		case "/api/resource/Echo/html":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<html>bad request from "+auth+"</html>")
		case "/api/resource/Echo/long":
			// The secret straddles the truncation point.
			writeJSON(w, http.StatusExpectationFailed, map[string]any{
				"exc_type": "ValidationError",
				"message":  strings.Repeat("a", maxMessage-5) + secret,
			})
		default:
			writeRaw(w, http.StatusOK, `{"data":{}}`)
		}
	}))

	// Positive control: the secret does reach the server.
	if _, err := Get[map[string]any](t.Context(), c, "Echo", "ok"); err != nil {
		t.Fatal(err)
	}
	if !sawAuth.Load() {
		t.Fatal("the server never saw the secret; the test proves nothing")
	}

	type holder struct {
		c Client // unexported: fmt walks it with reflection
		p *Client
	}
	type exported struct {
		C Client
		S Secret
	}
	var outputs []string
	add := func(s string) { outputs = append(outputs, s) }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		add(fmt.Sprintf(verb, c))
		add(fmt.Sprintf(verb, *c))
		add(fmt.Sprintf(verb, testSecret))
		add(fmt.Sprintf(verb, &holder{c: *c, p: c}))
		add(fmt.Sprintf(verb, exported{C: *c, S: testSecret}))
	}
	add(fmt.Sprint(*c))
	add(fmt.Sprint(c))
	add(fmt.Sprint(testSecret))
	add(c.String())
	for _, v := range []any{c, *c, testSecret, exported{C: *c, S: testSecret}, map[string]Secret{"s": testSecret}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		add(string(b))
	}
	var logBuf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&logBuf, nil), slog.NewTextHandler(&logBuf, nil)} {
		slog.New(h).Info("client", "client", c, "value", *c, "secret", testSecret, slog.Any("any", exported{C: *c, S: testSecret}))
	}
	add(logBuf.String())

	for _, path := range []string{"json", "html", "long"} {
		_, err := Get[map[string]any](t.Context(), c, "Echo", path)
		if err == nil {
			t.Fatalf("Echo/%s: want an error", path)
		}
		add(err.Error())
		add(fmt.Sprintf("%+v", err))
		var ae *APIError
		if errors.As(err, &ae) {
			add(ae.Message)
		}
	}

	for i, out := range outputs {
		if strings.Contains(out, secret[:8]) {
			t.Errorf("output %d leaks (a prefix of) the secret: %s", i, out)
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", testSecret), redacted) {
		t.Errorf("Secret %%v = %q, want %q", fmt.Sprintf("%v", testSecret), redacted)
	}
}

func TestRedirect(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		writeRaw(w, http.StatusOK, `{"data":{"name":"stolen"}}`)
	}))
	defer other.Close()

	var (
		mu   sync.Mutex
		hits = map[string]int{}
		auth = map[string]string{}
	)
	var mainURLp atomic.Pointer[url.URL]
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		auth[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		mainURL := mainURLp.Load()
		switch r.URL.Path {
		case "/api/resource/X/port":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		case "/api/resource/X/host":
			http.Redirect(w, r, "http://localhost:"+mainURL.Port()+"/api/resource/X/landed", http.StatusFound)
		case "/api/resource/X/scheme":
			http.Redirect(w, r, "https://"+mainURL.Host+"/api/resource/X/landed", http.StatusFound)
		case "/api/resource/X/same":
			http.Redirect(w, r, "/api/resource/X/landed", http.StatusFound)
		default:
			writeRaw(w, http.StatusOK, `{"data":{"name":"landed"}}`)
		}
	}), func(cfg *config.Config) {
		// Put the targets on the httpx allowlist, so the refusal below is
		// the client's redirect policy and not the egress guard.
		cfg.BooksMCPURL = other.URL
		u, _ := url.Parse(cfg.ERPBaseURL)
		cfg.DoclingURL = "http://localhost:" + u.Port()
	})
	mainURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mainURLp.Store(mainURL)

	for _, name := range []string{"port", "host", "scheme"} {
		t.Run(name, func(t *testing.T) {
			_, err := Get[map[string]any](t.Context(), c, "X", name)
			if !errors.Is(err, errRedirectRefused) {
				t.Fatalf("err = %v, want a refused redirect", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if n := hits["/api/resource/X/"+name]; n != 1 {
				t.Errorf("redirecting path hit %d times, want 1 (a refused redirect is not retried)", n)
			}
		})
	}
	mu.Lock()
	if hits["/api/resource/X/landed"] != 0 {
		t.Errorf("a refused redirect still reached its target")
	}
	mu.Unlock()
	if otherHits.Load() != 0 {
		t.Errorf("the other port received %d requests (and the Authorization header)", otherHits.Load())
	}

	t.Run("same origin is followed", func(t *testing.T) {
		doc, err := Get[map[string]any](t.Context(), c, "X", "same")
		if err != nil {
			t.Fatal(err)
		}
		if doc["name"] != "landed" {
			t.Errorf("doc = %v", doc)
		}
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(auth["/api/resource/X/landed"], testSecret.reveal()) {
			t.Error("same-origin redirect lost the Authorization header")
		}
	})
}

func TestCheckRedirectLimit(t *testing.T) {
	u, _ := url.Parse("http://localhost:8080/a")
	req := &http.Request{URL: u}
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = req
	}
	if err := checkRedirect(req, via); !errors.Is(err, errRedirectRefused) {
		t.Errorf("checkRedirect after %d redirects = %v, want refused", maxRedirects, err)
	}
	if err := checkRedirect(req, via[:1]); err != nil {
		t.Errorf("same-origin redirect refused: %v", err)
	}
}
