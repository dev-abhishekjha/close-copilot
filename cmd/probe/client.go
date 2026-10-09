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

// client is the small private Frappe request helper that CC-202's
// internal/frappe replaces. It never puts the secret in an error, a log
// line or its own string form. The credentials stay config.Secret until do
// builds the Authorization header, so even reflection-driven printing of a
// client (%#v, or a client inside another struct) finds no raw value.
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
// permission: HTTP 403 or exc_type PermissionError. An authentication
// failure (401) is not a refusal: it means the key itself is wrong.
func isRefused(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusForbidden || ae.ExcType == "PermissionError"
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
