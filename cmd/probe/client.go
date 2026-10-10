package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

// maxBody caps a response body. The largest DocType meta (Sales Invoice) is
// well under 1 MiB.
const maxBody = 16 << 20

// client is the probe's own small Frappe request helper. It stays separate
// from internal/frappe because check (b) needs a raw request that client
// refuses on purpose: GET frappe.client.has_permission (internal/frappe
// allows GET only on frappe.client's get, get_list, get_count and
// get_value), and the probe must send no POST at all. It follows the same
// rules as internal/frappe where they apply (CC-205): every redirect is
// refused, a refusal is HTTP 403 with exc_type PermissionError, and the
// secret never appears in an error, a log line or its own string form. The
// credentials stay config.Secret until do builds the Authorization header,
// so even reflection-driven printing of a client (%#v, or a client inside
// another struct) finds no raw value. httpx bounds every request (30 s) and
// nothing is retried.
type client struct {
	http   *http.Client
	base   string // ERP_BASE_URL without a trailing slash
	site   string
	key    config.Secret
	secret config.Secret
}

func newClient(cfg config.Config, key, secret config.Secret) (*client, error) {
	if key.IsZero() || secret.IsZero() {
		return nil, errors.New("probe: an ERPNext API key and secret are required")
	}
	hc, err := httpx.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	hc.CheckRedirect = checkRedirect
	return &client{
		http:   hc,
		base:   strings.TrimRight(cfg.ERPBaseURL, "/"),
		site:   cfg.ERPSite,
		key:    key,
		secret: secret,
	}, nil
}

// String, GoString, Format and LogValue keep the secret out of fmt and slog
// output.
func (c *client) String() string { return "erpnext client (credentials redacted)" }

// GoString implements fmt.GoStringer.
func (c *client) GoString() string { return c.String() }

// Format implements fmt.Formatter: every verb prints String.
func (c *client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c *client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// apiError is a non-2xx Frappe response.
type apiError struct {
	Method  string
	Path    string
	Status  int
	ExcType string
	Message string
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.ExcType != "" {
		msg += " " + e.ExcType
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// isRefused reports whether err is Frappe refusing the request for lack of
// permission: HTTP 403 with exc_type PermissionError. A bare 403 (a proxy or
// WAF) says nothing about the user's roles, a PermissionError on another
// status is not what Frappe sends for a refusal, and an authentication
// failure (401) means the key itself is wrong; none of them counts.
func isRefused(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusForbidden && ae.ExcType == "PermissionError"
}

// errRedirectRefused is wrapped by the error for any redirect.
var errRedirectRefused = errors.New("probe: redirect refused")

// checkRedirect refuses every redirect, as internal/frappe does. The Frappe
// REST API never needs one, and a followed redirect is risky: net/http
// keeps the Authorization header on a redirect to the same host on another
// port, and a same-origin redirect with an absolute Location keeps it while
// dropping the Host override for ERP_SITE. A redirect to another scheme,
// host or port is therefore refused, and so is every other one.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	// Escaped paths, quoted, so a hostile Location can't put control
	// characters into an error message.
	return fmt.Errorf("%w: %s %q redirected to %q", errRedirectRefused,
		via[0].Method, via[0].URL.EscapedPath(), req.URL.Scheme+"://"+req.URL.Host+req.URL.EscapedPath())
}

// redact removes the secret from s.
func (c *client) redact(s string) string {
	if c.secret.IsZero() {
		return s
	}
	return strings.ReplaceAll(s, c.secret.Reveal(), "[redacted]")
}

// do sends one request to path (already escaped, starting with /api/) with
// query, encodes body as JSON when it isn't nil, and decodes a 2xx response
// into out when out isn't nil.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode body: %w", method, path, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("%s %s: %s", method, path, c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "token "+c.key.Reveal()+":"+c.secret.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.site != "" {
		req.Host = c.site
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %s", method, path, c.redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %s", method, path, c.redact(err.Error()))
	}
	if len(data) > maxBody {
		return fmt.Errorf("%s %s: response body over %d bytes", method, path, maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.parseError(method, path, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %s", method, path, c.redact(err.Error()))
	}
	return nil
}

// frappeError is the part of a Frappe error body that probe reports. The
// traceback in "exc" is dropped.
type frappeError struct {
	ExcType        string `json:"exc_type"`
	Exception      string `json:"exception"`
	Message        any    `json:"message"`
	ServerMessages string `json:"_server_messages"`
}

func (c *client) parseError(method, path string, status int, data []byte) error {
	e := &apiError{Method: method, Path: path, Status: status}
	var fe frappeError
	if err := json.Unmarshal(data, &fe); err != nil {
		e.Message = truncate(c.redact(strings.TrimSpace(string(data))))
		return e
	}
	e.ExcType = fe.ExcType
	msg := serverMessage(fe.ServerMessages)
	if msg == "" {
		if s, ok := fe.Message.(string); ok {
			msg = s
		}
	}
	if msg == "" {
		msg = fe.Exception
	}
	e.Message = truncate(c.redact(msg))
	return e
}

// serverMessage returns the first message in Frappe's _server_messages, a
// JSON array of JSON-encoded {"message": ...} objects.
func serverMessage(raw string) string {
	if raw == "" {
		return ""
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) == 0 {
		return ""
	}
	var m struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(list[0]), &m); err != nil {
		return list[0]
	}
	return m.Message
}

func truncate(s string) string {
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// resourcePath is /api/resource/<doctype>[/<name>] with each part escaped.
func resourcePath(doctype string, name ...string) string {
	p := "/api/resource/" + url.PathEscape(doctype)
	for _, n := range name {
		p += "/" + url.PathEscape(n)
	}
	return p
}

// methodPath is /api/method/<dotted.path>.
func methodPath(method string) string {
	return "/api/method/" + method
}
