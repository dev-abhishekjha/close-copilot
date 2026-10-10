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
	// MaxPages bounds List together with MaxRows: List fails rather than
	// page on past it.
	MaxPages = 10_000
	// MaxInFlight is how many Get requests GetMany runs at once.
	MaxInFlight = 4
	// MaxAttempts is the most times an idempotent (GET) request is sent.
	MaxAttempts = 4
	// DefaultDeadline is Options.Deadline when it is zero.
	DefaultDeadline = 60 * time.Second
	// DefaultMaxRows is Options.MaxRows when it is zero.
	DefaultMaxRows = 200_000

	requestTimeout = 15 * time.Second
	maxBody        = 32 << 20 // 32 MiB; the largest list page is far smaller
	backoffBase    = 250 * time.Millisecond
	backoffMax     = 4 * time.Second
	retryAfterCap  = 30 * time.Second
)

const redacted = "[redacted]"

// Options tunes a Client. A zero field takes its default.
type Options struct {
	// Deadline bounds one request, with all its retries and Retry-After
	// waits, when the caller's context has no deadline of its own. Zero
	// means DefaultDeadline (60 s). A caller's deadline always wins, longer
	// or shorter.
	Deadline time.Duration
	// MaxRows is the most rows one List call may collect; List fails once
	// a server returns more. Zero means DefaultMaxRows (200,000).
	MaxRows int
}

// Client is a Frappe REST client for one site and one API key. It is safe
// for concurrent use. Its fmt, slog and JSON forms never show the secret.
//
// The secret is a config.Secret, which holds its value behind a pointer:
// fmt walks a Client held in an unexported field by reflection, without
// calling String or Format, and finds only an address.
type Client struct {
	hc     *http.Client
	base   string // ERP_BASE_URL without a trailing slash
	site   string // sent as the Host header when set
	key    string
	secret config.Secret

	pageSize    int
	maxPages    int
	maxRows     int
	maxAttempts int
	deadline    time.Duration
	backoffBase time.Duration
	backoffMax  time.Duration
	afterCap    time.Duration
	sleep       func(context.Context, time.Duration) error
}

// New returns a client for cfg.ERPBaseURL and cfg.ERPSite that
// authenticates with key and secret (the bot pair ERP_API_KEY and
// ERP_API_SECRET, or the seeder pair in seeding code), with the default
// Options. Requests go through internal/httpx with a 15 s timeout per
// attempt, and every redirect is refused.
//
// Deadline: when the caller's context has no deadline, each request (one
// Get, Insert, Call or List page, with all its retries and Retry-After
// waits) gets DefaultDeadline, 60 s. A retry wait that would end after the
// deadline is not taken: the request fails at once with the last error.
// List and GetMany make many requests, and each has its own deadline; give
// them a context deadline to bound the whole call.
func New(cfg config.Config, key string, secret config.Secret) (*Client, error) {
	return NewWithOptions(cfg, key, secret, Options{})
}

// NewWithOptions is New with explicit Options.
func NewWithOptions(cfg config.Config, key string, secret config.Secret, opts Options) (*Client, error) {
	if _, err := authorization(key, secret); err != nil {
		return nil, err
	}
	switch {
	case opts.Deadline < 0:
		return nil, fmt.Errorf("frappe: negative Options.Deadline %s", opts.Deadline)
	case opts.MaxRows < 0:
		return nil, fmt.Errorf("frappe: negative Options.MaxRows %d", opts.MaxRows)
	}
	if opts.Deadline == 0 {
		opts.Deadline = DefaultDeadline
	}
	if opts.MaxRows == 0 {
		opts.MaxRows = DefaultMaxRows
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
		secret:      secret,
		pageSize:    PageSize,
		maxPages:    MaxPages,
		maxRows:     opts.MaxRows,
		maxAttempts: MaxAttempts,
		deadline:    opts.Deadline,
		backoffBase: backoffBase,
		backoffMax:  backoffMax,
		afterCap:    retryAfterCap,
		sleep:       sleepCtx,
	}, nil
}

// authorization is the Authorization header value for key and secret. It
// is one of the two places that reveal the secret (redact is the other);
// New calls it once to validate the pair.
func authorization(key string, secret config.Secret) (string, error) {
	raw := secret.Reveal()
	if key == "" || raw == "" {
		return "", errors.New("frappe: an API key and secret are required")
	}
	if strings.ContainsAny(key, ": \t\r\n") || strings.ContainsAny(raw, ": \t\r\n") {
		return "", errors.New("frappe: the API key or secret contains a colon or whitespace")
	}
	return "token " + key + ":" + raw, nil
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
	if c == nil {
		return text
	}
	return redactSecret(text, c.secret.Reveal())
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
// a response arrived. POST, PUT and DELETE are sent exactly once.
//
// When ctx has no deadline, do applies c.deadline across every attempt and
// wait. A wait that would outlast the deadline (a long Retry-After, say) is
// not taken: do returns the last error at once.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if _, ok := ctx.Deadline(); !ok && c.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.deadline)
		defer cancel()
	}
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
		retry := attempt < attempts
		if err != nil {
			retry = retry && !responded && retryableTransport(ctx, err)
		} else {
			retry = retry && retryableStatus(r.status)
		}
		var wait time.Duration
		if retry {
			var header http.Header
			if r != nil {
				header = r.header
			}
			wait = c.backoff(attempt, header)
			if !fitsDeadline(ctx, wait) {
				slog.DebugContext(ctx, "frappe: not retrying; the wait would outlast the deadline",
					"method", method, "path", path, "attempt", attempt, "wait", wait)
				retry = false
			}
		}
		if !retry {
			if err != nil {
				return c.opErr(method, path, "", err)
			}
			resp = r
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

// fitsDeadline reports whether a wait of d ends before ctx's deadline.
func fitsDeadline(ctx context.Context, d time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > d
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
	auth, err := authorization(c.key, c.secret)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", auth)
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
