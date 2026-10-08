package frappe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

// Limits and defaults. They are package constants; a Client copies them
// into fields so the unit tests can shrink them.
const (
	// PageSize is the number of rows List asks for per request.
	PageSize = 500
	// MaxPages bounds List: a server that ignores limit_start would
	// otherwise be paged forever.
	MaxPages = 10_000
	// MaxInFlight is how many Get requests GetMany runs at once.
	MaxInFlight = 4
	// MaxAttempts is the most times an idempotent (GET) request is sent.
	MaxAttempts = 4

	requestTimeout = 15 * time.Second
	maxBody        = 32 << 20 // 32 MiB; the largest list page is far smaller
	backoffBase    = 250 * time.Millisecond
	backoffMax     = 4 * time.Second
	retryAfterCap  = 30 * time.Second
)

const redacted = "[redacted]"

// Secret is an API secret. Every way of printing or encoding it (fmt verbs,
// slog, encoding/json) yields "[redacted]"; only the unexported reveal
// method, used to build the Authorization header, returns the value.
type Secret string

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// reveal is the only way to the raw value. It builds the Authorization
// header and the redaction filter, nothing else.
func (s Secret) reveal() string { return string(s) }

// Client is a Frappe REST client for one site and one API key. It is safe
// for concurrent use. Its fmt, slog and JSON forms never show the secret.
//
// The Secret sits inside a closure. fmt bypasses String and Format on
// unexported fields and walks them by reflection, and a mismatched verb
// (%s on a pointer) even dereferences a nested pointer, so a struct that
// holds a Client in an unexported field would print a plain Secret field.
// A func value prints only as an address.
type Client struct {
	hc     *http.Client
	base   string // ERP_BASE_URL without a trailing slash
	site   string // sent as the Host header when set
	key    string
	secret func() Secret

	pageSize    int
	maxPages    int
	maxAttempts int
	backoffBase time.Duration
	backoffMax  time.Duration
	afterCap    time.Duration
	sleep       func(context.Context, time.Duration) error
}

// New returns a client for cfg.ERPBaseURL and cfg.ERPSite that
// authenticates with key and secret (the bot pair ERP_API_KEY and
// ERP_API_SECRET, or the seeder pair in seeding code). Requests go through
// internal/httpx with a 15 s timeout, and every redirect is refused.
func New(cfg config.Config, key string, secret Secret) (*Client, error) {
	if key == "" || secret == "" {
		return nil, errors.New("frappe: an API key and secret are required")
	}
	if strings.ContainsAny(key, ": \t\r\n") || strings.ContainsAny(secret.reveal(), ": \t\r\n") {
		return nil, errors.New("frappe: the API key or secret contains a colon or whitespace")
	}
	base, err := url.Parse(cfg.ERPBaseURL)
	switch {
	case err != nil:
		return nil, fmt.Errorf("frappe: %s: %w", config.EnvERPBaseURL, err)
	case base.Scheme != "http" && base.Scheme != "https":
		return nil, fmt.Errorf("frappe: %s must be an http or https URL", config.EnvERPBaseURL)
	case base.Host == "":
		return nil, fmt.Errorf("frappe: %s has no host", config.EnvERPBaseURL)
	case base.User != nil || base.RawQuery != "" || base.Fragment != "":
		return nil, fmt.Errorf("frappe: %s must not carry user info, a query or a fragment", config.EnvERPBaseURL)
	}
	hc, err := httpx.New(cfg, httpx.WithTimeout(requestTimeout))
	if err != nil {
		return nil, fmt.Errorf("frappe: %w", err)
	}
	hc.CheckRedirect = checkRedirect
	return &Client{
		hc:          hc,
		base:        strings.TrimRight(cfg.ERPBaseURL, "/"),
		site:        cfg.ERPSite,
		key:         key,
		secret:      func() Secret { return secret },
		pageSize:    PageSize,
		maxPages:    MaxPages,
		maxAttempts: MaxAttempts,
		backoffBase: backoffBase,
		backoffMax:  backoffMax,
		afterCap:    retryAfterCap,
		sleep:       sleepCtx,
	}, nil
}

// String, GoString, Format and LogValue have value receivers, so a Client
// and a *Client print the same redacted form under every verb.
func (c Client) String() string {
	return fmt.Sprintf("frappe.Client{base: %s, site: %s, credentials: %s}", c.base, c.site, redacted)
}

func (c Client) GoString() string { return c.String() }

func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c Client) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.base),
		slog.String("site", c.site),
		slog.String("credentials", redacted),
	)
}

// MarshalJSON keeps a Client out of JSON output altogether.
func (c Client) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"base_url": c.base, "site": c.site, "credentials": redacted})
}

// redact removes the secret (raw and query-escaped) from text.
func (c *Client) redact(text string) string {
	if c == nil || c.secret == nil {
		return text
	}
	return redactSecret(text, c.secret().reveal())
}

func redactSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	text = strings.ReplaceAll(text, secret, redacted)
	if esc := url.QueryEscape(secret); esc != secret {
		text = strings.ReplaceAll(text, esc, redacted)
	}
	return text
}

// errRedirectRefused is wrapped by the error for any redirect.
var errRedirectRefused = errors.New("frappe: redirect refused")

// checkRedirect refuses every redirect. The Frappe REST API answers
// /api/resource and /api/method directly and never needs one, while a
// followed redirect is risky: net/http keeps the Authorization header on a
// redirect to the same host on another port (other local ports are on the
// httpx allowlist), and a same-origin redirect with an absolute Location
// keeps Authorization but drops the Host override for ERP_SITE.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	// Escaped paths, quoted: the decoded Path of a hostile Location can hold
	// LF or ESC, which must never reach an error message or a log line.
	return fmt.Errorf("%w: %s %q redirected to %q", errRedirectRefused,
		via[0].Method, via[0].URL.EscapedPath(), origin(req.URL)+req.URL.EscapedPath())
}

// origin is scheme://host:port in lower case, with the scheme's default
// port filled in.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// opError is a failure without a Frappe error body (transport, decoding).
// Its text is redacted; Unwrap keeps errors.Is working for context errors.
type opError struct {
	msg string
	err error
}

func (e *opError) Error() string { return e.msg }
func (e *opError) Unwrap() error { return e.err }

func (c *Client) opErr(method, path, what string, err error) error {
	msg := method + " " + path + ": "
	if what != "" {
		msg += what + ": "
	}
	return &opError{msg: msg + c.redact(err.Error()), err: err}
}

// response is a fully read HTTP response.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends method to path (escaped, starting with /api/) with query and,
// when body isn't nil, a JSON body. A 2xx response is decoded into out
// (when not nil) with UseNumber; anything else becomes an *APIError. Only
// GET is retried: on 429, 502, 503 and 504, and on transport errors before
// a response arrived.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return c.opErr(method, path, "encode request", err)
		}
		payload = b
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = c.maxAttempts
	}

	var resp *response
	for attempt := 1; ; attempt++ {
		r, responded, err := c.send(ctx, method, target, payload)
		var wait time.Duration
		switch {
		case err != nil:
			if responded || attempt >= attempts || !retryableTransport(ctx, err) {
				return c.opErr(method, path, "", err)
			}
			wait = c.backoff(attempt, nil)
		case retryableStatus(r.status) && attempt < attempts:
			wait = c.backoff(attempt, r.header)
		default:
			resp = r
		}
		if resp != nil {
			break
		}
		status := 0
		if r != nil {
			status = r.status
		}
		slog.DebugContext(ctx, "frappe: retrying", "method", method, "path", path,
			"attempt", attempt, "status", status, "wait", wait)
		if err := c.sleep(ctx, wait); err != nil {
			return c.opErr(method, path, fmt.Sprintf("gave up after attempt %d", attempt), err)
		}
	}

	if resp.status < 200 || resp.status > 299 {
		return c.parseError(method, path, resp.status, resp.body)
	}
	if out == nil {
		return nil
	}
	if err := decodeJSON(resp.body, out); err != nil {
		return c.opErr(method, path, "decode response", err)
	}
	return nil
}

// send makes one request. responded is true when the server answered, so a
// later failure (reading the body) must not be retried.
func (c *Client) send(ctx context.Context, method, target string, payload []byte) (r *response, responded bool, err error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "token "+c.key+":"+c.secret().reveal())
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.site != "" {
		req.Host = c.site
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, true, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxBody {
		return nil, true, fmt.Errorf("response body over %d bytes", maxBody)
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}, true, nil
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryableTransport reports whether a failure before any response is worth
// another attempt: not when the caller gave up, and not when httpx or the
// redirect policy refused the request (that answer won't change).
func retryableTransport(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, httpx.ErrHostRefused) && !errors.Is(err, errRedirectRefused)
}

// backoff is the wait before attempt+1. A Retry-After header wins, capped at
// afterCap; otherwise the wait is exponential with jitter (half fixed, half
// random), capped at backoffMax.
func (c *Client) backoff(attempt int, header http.Header) time.Duration {
	if d, ok := retryAfter(header, time.Now()); ok {
		return min(d, c.afterCap)
	}
	d := c.backoffBase
	for i := 1; i < attempt && d < c.backoffMax; i++ {
		d *= 2
	}
	d = min(d, c.backoffMax)
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + rand.N(d-half+1) //nolint:gosec // retry jitter, not a security decision
}

// retryAfter parses a Retry-After header: delay-seconds or an HTTP date.
func retryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int64(retryAfterCap/time.Second) {
			return retryAfterCap, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// decodeJSON decodes data into v with UseNumber, so amounts arrive as
// json.Number (exact decimal strings) and never as float64.
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
