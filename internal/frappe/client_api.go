package frappe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Query selects rows for List. Filters use Frappe's JSON array form, for
// example
//
//	[][]any{{"company", "=", "Sharma Traders Pvt Ltd"},
//		{"posting_date", "between", []string{"2026-09-01", "2026-09-30"}}}
//
// Without OrderBy Frappe sorts by the DocType's default (usually modified
// desc), which can shift between pages while documents change; set OrderBy
// (for example "name asc") when paging a live ledger.
type Query struct {
	Fields  []string
	Filters [][]any
	OrderBy string
	Limit   int // 0 = all pages
}

// List returns the rows of doctype matching q, fetching PageSize rows per
// request via limit_start and limit_page_length. List rows never include
// child tables; use Get or GetMany for those.
//
// q is checked first (see ErrUnsafeQuery): Fields and filter fields must be
// column names, OrderBy a comma-separated list of "<field> asc|desc", and
// nothing is sent when they aren't.
//
// List stops at the first empty page (or once it has q.Limit rows), never
// at a merely short one, so a server that caps pages below PageSize can't
// make it drop rows; the cost is one extra request that returns nothing.
// So that a misbehaving server can't make it page forever, List fails when
// a page repeats the previous one byte for byte or starts with the same
// row name, after MaxPages pages, or once it holds more than
// Options.MaxRows rows. (Two genuinely identical consecutive pages are only
// possible when Fields omits "name"; include it when selecting few columns.)
//
// Without a caller deadline, the whole call is bounded by
// Options.ListDeadline and each page request by Options.Deadline (see New).
func List[T any](ctx context.Context, c *Client, doctype string, q Query) ([]T, error) {
	path, err := resourcePath(doctype)
	if err != nil {
		return nil, err
	}
	ctx, cancelWhole := c.wholeCall(ctx)
	defer cancelWhole()
	if q.Limit < 0 {
		return nil, fmt.Errorf("frappe: list %s: negative limit %d", doctype, q.Limit)
	}
	if err := validateQuery(q); err != nil {
		return nil, fmt.Errorf("frappe: list %s: %w", doctype, err)
	}
	base := url.Values{}
	if len(q.Fields) > 0 {
		b, err := json.Marshal(q.Fields)
		if err != nil {
			return nil, fmt.Errorf("frappe: list %s: encode fields: %w", doctype, err)
		}
		base.Set("fields", string(b))
	}
	if len(q.Filters) > 0 {
		b, err := json.Marshal(q.Filters)
		if err != nil {
			return nil, fmt.Errorf("frappe: list %s: encode filters: %w", doctype, err)
		}
		base.Set("filters", string(b))
	}
	if q.OrderBy != "" {
		base.Set("order_by", q.OrderBy)
	}

	out := []T{}
	var prev, prevFirst json.RawMessage
	start := 0
	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, fmt.Errorf("frappe: list %s: stopped after %d pages (%d rows) without reaching an empty page", doctype, c.maxPages, len(out))
		}
		want := c.pageSize
		if q.Limit > 0 {
			want = min(want, q.Limit-len(out))
		}
		v := url.Values{}
		for k, vs := range base {
			v[k] = vs
		}
		v.Set("limit_start", strconv.Itoa(start))
		v.Set("limit_page_length", strconv.Itoa(want))

		var resp struct {
			Data json.RawMessage `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, path, v, nil, &resp); err != nil {
			return nil, fmt.Errorf("frappe: list %s: %w", doctype, err)
		}
		var rows []T
		if len(resp.Data) > 0 {
			if err := decodeJSON(resp.Data, &rows); err != nil {
				return nil, fmt.Errorf("frappe: list %s: decode page at limit_start %d: %w", doctype, start, err)
			}
		}
		// Only an empty page ends the list. A short page is not proof of
		// the end: a server (or proxy) that caps page length below what we
		// asked for would otherwise silently drop the remaining rows.
		if len(rows) == 0 {
			return out, nil
		}
		first := firstRowName(resp.Data)
		if bytes.Equal(resp.Data, prev) || (first != nil && bytes.Equal(first, prevFirst)) {
			return nil, fmt.Errorf("frappe: list %s: page at limit_start %d repeats the previous page; refusing to page on", doctype, start)
		}
		prev, prevFirst = resp.Data, first
		out = append(out, rows...)
		if q.Limit > 0 && len(out) >= q.Limit {
			return out[:q.Limit], nil
		}
		if len(out) > c.maxRows {
			return nil, fmt.Errorf("frappe: list %s: more than %d rows; narrow the filters", doctype, c.maxRows)
		}
		start += len(rows)
	}
}

// firstRowName is the raw JSON "name" of the first row of a list page, or
// nil when the page has no rows or the first row has no (or a null) name.
func firstRowName(data json.RawMessage) json.RawMessage {
	var rows []struct {
		Name json.RawMessage `json:"name"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	if n := rows[0].Name; len(n) > 0 && string(n) != "null" {
		return n
	}
	return nil
}

// Get returns one document, child tables included.
func Get[T any](ctx context.Context, c *Client, doctype, name string) (T, error) {
	var zero T
	path, err := resourcePath(doctype, name)
	if err != nil {
		return zero, err
	}
	var resp struct {
		Data *T `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return zero, fmt.Errorf("frappe: get %s %q: %w", doctype, name, err)
	}
	if resp.Data == nil {
		return zero, fmt.Errorf("frappe: get %s %q: response has no data", doctype, name)
	}
	return *resp.Data, nil
}

// GetMany fetches each named document with Get (so child tables are
// included), at most MaxInFlight at a time. Results are in the order of
// names. The first error cancels the requests still running or waiting and
// is the error returned. Without a caller deadline, the whole call is
// bounded by Options.ListDeadline and each Get by Options.Deadline.
func GetMany[T any](ctx context.Context, c *Client, doctype string, names []string) ([]T, error) {
	out := make([]T, len(names))
	if len(names) == 0 {
		return out, nil
	}
	ctx, cancelWhole := c.wholeCall(ctx)
	defer cancelWhole()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	sem := make(chan struct{}, MaxInFlight)
loop:
	for i, name := range names {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Go(func() {
			defer func() { <-sem }()
			doc, err := Get[T](ctx, c, doctype, name)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = doc
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("frappe: get many %s: %w", doctype, err)
	}
	return out, nil
}

// Insert creates a document (POST /api/resource/<doctype>) and returns it as
// saved, with name and defaults filled in. It is never retried.
func Insert(ctx context.Context, c *Client, doctype string, doc any) (map[string]any, error) {
	path, err := resourcePath(doctype)
	if err != nil {
		return nil, err
	}
	return writeDoc(ctx, c, http.MethodPost, path, "insert "+doctype, doc)
}

// Update changes fields of a draft (or allow-on-submit fields of a
// submitted) document (PUT /api/resource/<doctype>/<name>) and returns it.
// It is never retried.
func Update(ctx context.Context, c *Client, doctype, name string, fields any) (map[string]any, error) {
	path, err := resourcePath(doctype, name)
	if err != nil {
		return nil, err
	}
	return writeDoc(ctx, c, http.MethodPut, path, fmt.Sprintf("update %s %q", doctype, name), fields)
}

func writeDoc(ctx context.Context, c *Client, method, path, what string, doc any) (map[string]any, error) {
	if doc == nil {
		return nil, fmt.Errorf("frappe: %s: document is nil", what)
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := c.do(ctx, method, path, nil, doc, &resp); err != nil {
		return nil, fmt.Errorf("frappe: %s: %w", what, err)
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("frappe: %s: response has no data", what)
	}
	return resp.Data, nil
}

// Submit submits a draft through the whitelisted frappe.client.submit,
// which takes the whole document. Submit reads the latest version first so
// its "modified" timestamp passes Frappe's TimestampMismatchError check, then
// posts it back (not retried). It returns the submitted document
// (docstatus 1). This is the path the CC-202 integration test proves on
// frappe 15.
func Submit(ctx context.Context, c *Client, doctype, name string) (map[string]any, error) {
	doc, err := Get[map[string]any](ctx, c, doctype, name)
	if err != nil {
		return nil, fmt.Errorf("frappe: submit %s %q: %w", doctype, name, err)
	}
	doc["doctype"] = doctype
	out, err := Call[map[string]any](ctx, c, http.MethodPost, "frappe.client.submit", map[string]any{"doc": doc})
	if err != nil {
		return nil, fmt.Errorf("frappe: submit %s %q: %w", doctype, name, err)
	}
	return out, nil
}

// Cancel cancels a submitted document through the whitelisted
// frappe.client.cancel and returns it (docstatus 2). It is never retried.
func Cancel(ctx context.Context, c *Client, doctype, name string) (map[string]any, error) {
	if _, err := resourcePath(doctype, name); err != nil {
		return nil, err
	}
	out, err := Call[map[string]any](ctx, c, http.MethodPost, "frappe.client.cancel",
		map[string]any{"doctype": doctype, "name": name})
	if err != nil {
		return nil, fmt.Errorf("frappe: cancel %s %q: %w", doctype, name, err)
	}
	return out, nil
}

// Delete deletes a draft or cancelled document (DELETE
// /api/resource/<doctype>/<name>). It is never retried.
func Delete(ctx context.Context, c *Client, doctype, name string) error {
	path, err := resourcePath(doctype, name)
	if err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return fmt.Errorf("frappe: delete %s %q: %w", doctype, name, err)
	}
	return nil
}

var dottedPathRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)

// Call invokes the whitelisted method at /api/method/<dottedPath> and
// decodes its "message" into T (the zero T when the method returns
// nothing). httpMethod is GET or POST. GET sends params in the query
// string (strings as they are, everything else JSON-encoded); POST sends
// them as a JSON body. Before anything is sent (ErrUnsafeQuery):
//
//   - for frappe.client, GET is refused on anything but get, get_list,
//     get_count and get_value;
//   - for get_list, get_count and get_value, only their known parameters
//     are accepted, and fields, fieldname, order_by, filters and or_filters
//     go through the same checks as a List Query.
//
// Only a GET to one of those four frappe.client reads is retried; every
// other call, GET or POST, is sent exactly once.
func Call[T any](ctx context.Context, c *Client, httpMethod, dottedPath string, params map[string]any) (T, error) {
	var zero T
	if !dottedPathRE.MatchString(dottedPath) {
		return zero, fmt.Errorf("frappe: call %q: not a dotted method path", dottedPath)
	}
	if err := validateCall(httpMethod, dottedPath); err != nil {
		return zero, fmt.Errorf("frappe: call: %w", err)
	}
	if err := validateCallParams(dottedPath, params); err != nil {
		return zero, fmt.Errorf("frappe: call %s: %w", dottedPath, err)
	}
	path := "/api/method/" + dottedPath
	var (
		query url.Values
		body  any
	)
	switch httpMethod {
	case http.MethodGet:
		q, err := encodeParams(params)
		if err != nil {
			return zero, fmt.Errorf("frappe: call %s: %w", dottedPath, err)
		}
		query = q
	case http.MethodPost:
		body = params
		if params == nil {
			body = map[string]any{}
		}
	default:
		return zero, fmt.Errorf("frappe: call %s: method %q, want GET or POST", dottedPath, httpMethod)
	}
	var resp struct {
		Message T `json:"message"`
	}
	retryable := httpMethod == http.MethodGet && isClientRead(dottedPath)
	if err := c.request(ctx, httpMethod, path, query, body, &resp, retryable); err != nil {
		return zero, fmt.Errorf("frappe: call %s: %w", dottedPath, err)
	}
	return resp.Message, nil
}

// encodeParams turns method parameters into a query string: strings and
// json.Number as they are, everything else as JSON (Frappe parses JSON
// arguments such as filters).
func encodeParams(params map[string]any) (url.Values, error) {
	v := url.Values{}
	for k, p := range params {
		switch p := p.(type) {
		case string:
			v.Set(k, p)
		case json.Number:
			v.Set(k, p.String())
		default:
			b, err := json.Marshal(p)
			if err != nil {
				return nil, fmt.Errorf("encode parameter %q: %w", k, err)
			}
			v.Set(k, string(b))
		}
	}
	return v, nil
}

// errBadName is wrapped when a DocType or document name can't be used in a
// resource path.
var errBadName = errors.New("frappe: invalid DocType or document name")

// resourcePath is /api/resource/<doctype>[/<name>] with each part escaped.
// A DocType never contains a slash; a document name may (Frappe routes it
// as a path), but neither may be empty, "." or "..".
func resourcePath(doctype string, name ...string) (string, error) {
	if doctype == "" || strings.ContainsAny(doctype, "/\\?#") || strings.TrimSpace(doctype) != doctype {
		return "", fmt.Errorf("%w: DocType %q", errBadName, doctype)
	}
	p := "/api/resource/" + url.PathEscape(doctype)
	for _, n := range name {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "\r\n") {
			return "", fmt.Errorf("%w: %s name %q", errBadName, doctype, n)
		}
		p += "/" + url.PathEscape(n)
	}
	return p, nil
}
