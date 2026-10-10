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
	testKey  = "botkey0000000ab"
	testSite = "erp.localhost"
)

var testSecret = config.NewSecret("SECRETmustNEVERleak7f3a")

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
		secret config.Secret
		want   string // substring of the error; empty means success
	}{
		{"ok", good, testKey, testSecret, ""},
		{"no key", good, "", testSecret, "key and secret are required"},
		{"no secret", good, testKey, config.Secret{}, "key and secret are required"},
		{"colon in secret", good, testKey, config.NewSecret("a:b"), "colon or whitespace"},
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
	if want := "token " + testKey + ":" + testSecret.Reveal(); got.Get("Authorization") != want {
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
	secret := testSecret.Reveal()
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
		case "/api/resource/Echo/exctype":
			// The secret is a valid identifier, so only redaction keeps it
			// out of ExcType (which Error() prints verbatim).
			writeJSON(w, http.StatusExpectationFailed, map[string]any{"exc_type": secret, "message": "x"})
		case "/api/resource/Echo/split":
			// Inline markup splits the secret; stripping the tags rejoins it,
			// and the rejoined secret must be redacted too.
			writeJSON(w, http.StatusExpectationFailed, map[string]any{
				"exc_type": "ValidationError",
				"message":  "key " + secret[:6] + "<b></b>" + secret[6:] + " and " + secret[:9] + "<span>" + secret[9:] + "</span>",
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
		S config.Secret
	}
	var outputs []string
	add := func(s string) { outputs = append(outputs, s) }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%p"} {
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
	for _, v := range []any{c, *c, testSecret, exported{C: *c, S: testSecret}, map[string]config.Secret{"s": testSecret}} {
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

	for _, path := range []string{"json", "html", "long", "split", "exctype"} {
		_, err := Get[map[string]any](t.Context(), c, "Echo", path)
		if err == nil {
			t.Fatalf("Echo/%s: want an error", path)
		}
		add(err.Error())
		add(fmt.Sprintf("%+v", err))
		var ae *APIError
		if errors.As(err, &ae) {
			add(ae.Message)
			add(ae.ExcType)
			if path == "exctype" && ae.ExcType != "" {
				t.Errorf("Echo/exctype: ExcType = %d bytes, want empty", len(ae.ExcType))
			}
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
	)
	var mainURLp atomic.Pointer[url.URL]
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		mainURL := mainURLp.Load()
		switch r.URL.Path {
		case "/api/resource/X/port":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		case "/api/resource/X/host":
			http.Redirect(w, r, "http://localhost:"+mainURL.Port()+"/api/resource/X/landed", http.StatusFound)
		case "/api/resource/X/scheme":
			http.Redirect(w, r, "https://"+mainURL.Host+"/api/resource/X/landed", http.StatusFound)
		case "/api/resource/X/same-relative":
			http.Redirect(w, r, "/api/resource/X/landed", http.StatusFound)
		case "/api/resource/X/same-absolute":
			// Same origin, absolute Location: net/http would keep the
			// Authorization header but send Host 127.0.0.1:<port>, not ERP_SITE.
			http.Redirect(w, r, mainURL.String()+"/api/resource/X/landed", http.StatusMovedPermanently)
		case "/api/resource/X/hostile":
			// Percent-encoded LF and ESC decode into the target's Path.
			w.Header().Set("Location", "/api/x%0Atime=now%20level=INFO%20msg=approved%1B[2J")
			w.WriteHeader(http.StatusFound)
		case "/api/method/x.y":
			http.Redirect(w, r, "/api/method/x.z", http.StatusTemporaryRedirect)
		default:
			writeRaw(w, http.StatusOK, `{"data":{"name":"landed"},"message":"landed"}`)
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

	// Every redirect is refused: other port, other host, other scheme and
	// the same origin with a relative or an absolute Location.
	for _, name := range []string{"port", "host", "scheme", "same-relative", "same-absolute", "hostile"} {
		t.Run(name, func(t *testing.T) {
			_, err := Get[map[string]any](t.Context(), c, "X", name)
			if !errors.Is(err, errRedirectRefused) {
				t.Fatalf("err = %v, want a refused redirect", err)
			}
			for _, r := range err.Error() {
				if unsafeRune(r) {
					t.Errorf("redirect error has unsafe rune %U: %q", r, err.Error())
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if n := hits["/api/resource/X/"+name]; n != 1 {
				t.Errorf("redirecting path hit %d times, want 1 (a refused redirect is not retried)", n)
			}
		})
	}
	t.Run("method call", func(t *testing.T) {
		if _, err := Call[string](t.Context(), c, http.MethodPost, "x.y", nil); !errors.Is(err, errRedirectRefused) {
			t.Fatalf("err = %v, want a refused redirect", err)
		}
	})
	mu.Lock()
	if n := hits["/api/resource/X/landed"] + hits["/api/method/x.z"]; n != 0 {
		t.Errorf("a refused redirect still reached its target %d times", n)
	}
	mu.Unlock()
	if otherHits.Load() != 0 {
		t.Errorf("the other port received %d requests (and the Authorization header)", otherHits.Load())
	}
}

func TestCheckRedirect(t *testing.T) {
	first, _ := url.Parse("http://localhost:8080/api/resource/Account")
	same, _ := url.Parse("http://localhost:8080/api/resource/Account/")
	orig := &http.Request{Method: http.MethodGet, URL: first}
	if err := checkRedirect(orig, nil); err != nil {
		t.Errorf("the original request was refused: %v", err)
	}
	if err := checkRedirect(&http.Request{URL: same}, []*http.Request{orig}); !errors.Is(err, errRedirectRefused) {
		t.Errorf("same-origin redirect = %v, want refused", err)
	}
}
