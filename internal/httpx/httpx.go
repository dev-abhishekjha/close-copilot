// Package httpx builds every outbound HTTP client in the repo. Its transport
// refuses any host that isn't one of the configured services, so a
// prompt-injected URL or a redirect can't make the process talk to anything
// else. The CC-002 egress analyzer keeps other packages from building their
// own clients.
package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// DefaultTimeout bounds a whole request, redirects and body read included.
const DefaultTimeout = 30 * time.Second

// AnthropicHost is the Anthropic API, always allowed (LLM_PROVIDER=anthropic).
const AnthropicHost = "api.anthropic.com"

// ErrHostRefused is wrapped by the error a client returns for a host
// outside the allowlist.
var ErrHostRefused = errors.New("httpx: host not in allowlist")

// Option changes how New builds the client.
type Option func(*options)

type options struct {
	timeout time.Duration
}

// WithTimeout sets the client's overall request timeout (default 30 s).
// Zero means no timeout, as with http.Client.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// New returns a client whose transport only reaches the hosts of the
// configured service URLs (ERPBaseURL, BooksMCPURL, EvidenceMCPURL,
// TEIEmbedURL, TEIRerankURL, DoclingURL and, when set, the OTLP endpoint)
// plus api.anthropic.com. Unset URLs are skipped. A host is compared with
// its port (the scheme's default when absent), so two services on
// localhost stay distinct. Redirects pass through the same transport, so a
// redirect to a refused host fails.
//
// New fails if a configured URL can't be parsed or has no host.
func New(cfg config.Config, opts ...Option) (*http.Client, error) {
	o := options{timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&o)
	}

	allowed, err := allowlist(cfg)
	if err != nil {
		return nil, err
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("httpx: http.DefaultTransport is not an *http.Transport")
	}
	return &http.Client{
		Timeout:   o.timeout,
		Transport: &guard{base: base.Clone(), allowed: allowed},
	}, nil
}

func allowlist(cfg config.Config) (map[string]bool, error) {
	urls := []struct{ env, value string }{
		{config.EnvERPBaseURL, cfg.ERPBaseURL},
		{config.EnvBooksMCPURL, cfg.BooksMCPURL},
		{config.EnvEvidenceMCPURL, cfg.EvidenceMCPURL},
		{config.EnvTEIEmbedURL, cfg.TEIEmbedURL},
		{config.EnvTEIRerankURL, cfg.TEIRerankURL},
		{config.EnvDoclingURL, cfg.DoclingURL},
		{config.EnvOTLPEndpoint, cfg.OTLPEndpoint},
	}
	allowed := map[string]bool{hostKey("https", AnthropicHost): true}
	var errs []error
	for _, u := range urls {
		if u.value == "" {
			continue
		}
		parsed, err := url.Parse(u.value)
		if err != nil {
			errs = append(errs, fmt.Errorf("httpx: %s: %w", u.env, err))
			continue
		}
		if parsed.Host == "" {
			errs = append(errs, fmt.Errorf("httpx: %s=%q has no host", u.env, u.value))
			continue
		}
		allowed[hostKey(parsed.Scheme, parsed.Host)] = true
	}
	return allowed, errors.Join(errs...)
}

// hostKey normalises host to lower-case host:port, filling in the scheme's
// default port.
func hostKey(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port (or a bare IPv6 address in brackets).
		h = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		switch strings.ToLower(scheme) {
		case "http", "ws":
			port = "80"
		default:
			port = "443"
		}
	}
	return net.JoinHostPort(strings.ToLower(h), port)
}

// guard is the allowlisting RoundTripper.
type guard struct {
	base    http.RoundTripper
	allowed map[string]bool
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return nil, fmt.Errorf("%w: request has no URL", ErrHostRefused)
	}
	key := hostKey(req.URL.Scheme, req.URL.Host)
	if !g.allowed[key] {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrippers must close the body, even on error
		}
		return nil, fmt.Errorf("%w: refused request to host %q (allowed: %s)", ErrHostRefused, req.URL.Host, g.list())
	}
	return g.base.RoundTrip(req)
}

func (g *guard) list() string {
	hosts := make([]string, 0, len(g.allowed))
	for h := range g.allowed {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ", ")
}
