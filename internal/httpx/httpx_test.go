package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func get(t *testing.T, c *http.Client, rawURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func host(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestClient(t *testing.T) {
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should never be reached")
	}))
	defer refused.Close()

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, refused.URL+"/landing", http.StatusFound)
		case "/redirect-self":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	defer allowed.Close()

	c, err := New(config.Config{ERPBaseURL: allowed.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name      string
		url       string
		wantBody  string
		wantRefer string // host the error must name; empty means success
	}{
		{"allowed host", allowed.URL + "/ok", "ok", ""},
		{"redirect within allowed host", allowed.URL + "/redirect-self", "ok", ""},
		{"refused host", refused.URL + "/x", "", host(t, refused.URL)},
		{"redirect to refused host", allowed.URL + "/redirect", "", host(t, refused.URL)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := get(t, c, tt.url)
			if tt.wantRefer != "" {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatalf("got status %d, want an error naming %s", resp.StatusCode, tt.wantRefer)
				}
				if !errors.Is(err, ErrHostRefused) {
					t.Errorf("error %v does not wrap ErrHostRefused", err)
				}
				if !strings.Contains(err.Error(), tt.wantRefer) {
					t.Errorf("error %q does not name host %s", err, tt.wantRefer)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestAllowlist(t *testing.T) {
	cfg := config.Config{
		ERPBaseURL:     "http://localhost:8080",
		BooksMCPURL:    "http://localhost:8091/mcp",
		EvidenceMCPURL: "http://LOCALHOST:8092/mcp",
		TEIEmbedURL:    "http://tei-embed",
		TEIRerankURL:   "https://tei-rerank.internal",
		DoclingURL:     "http://[::1]:5001",
		OTLPEndpoint:   "https://cloud.langfuse.com/api/public/otel",
	}
	got, err := allowlist(cfg)
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	want := []string{
		"localhost:8080", "localhost:8091", "localhost:8092", "tei-embed:80",
		"tei-rerank.internal:443", "[::1]:5001", "cloud.langfuse.com:443", "api.anthropic.com:443",
	}
	if len(got) != len(want) {
		t.Errorf("allowlist has %d hosts, want %d: %v", len(got), len(want), got)
	}
	for _, h := range want {
		if !got[h] {
			t.Errorf("allowlist missing %s: %v", h, got)
		}
	}
	for _, h := range []string{"localhost:9999", "evil.example:443", "api.anthropic.com:80"} {
		if got[h] {
			t.Errorf("allowlist unexpectedly has %s", h)
		}
	}
}

func TestAllowlistSkipsUnsetOTLP(t *testing.T) {
	got, err := allowlist(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["api.anthropic.com:443"] {
		t.Errorf("empty config allowlist = %v, want only api.anthropic.com:443", got)
	}
}

func TestNewRejectsBadURLs(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"no host", config.Config{TEIEmbedURL: "tei-embed:80"}, config.EnvTEIEmbedURL},
		{"unparseable", config.Config{DoclingURL: "http://bad host/"}, config.EnvDoclingURL},
		{"relative", config.Config{ERPBaseURL: "/api"}, config.EnvERPBaseURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error = %v, want one naming %s", err, tt.want)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{"default", nil, 30 * time.Second},
		{"option", []Option{WithTimeout(5 * time.Second)}, 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(config.Config{}, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if c.Timeout != tt.want {
				t.Errorf("Timeout = %v, want %v", c.Timeout, tt.want)
			}
		})
	}
}

func TestTimeoutApplies(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	c, err := New(config.Config{BooksMCPURL: srv.URL}, WithTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := get(t, c, srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("want a timeout error")
	}
}
