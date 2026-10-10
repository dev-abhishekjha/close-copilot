package frappe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// newOptsClient is newTestClient with explicit Options and the real sleep.
func newOptsClient(t *testing.T, h http.Handler, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewWithOptions(config.Config{ERPBaseURL: srv.URL, ERPSite: testSite}, testKey, testSecret, opts)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	return c
}

// countingHandler answers every request with an empty list page and counts
// the requests.
func countingHandler() (http.Handler, *atomic.Int32) {
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		writeRaw(w, http.StatusOK, `{"data":[]}`)
	}), &n
}

func TestQueryRejects(t *testing.T) {
	h, hits := countingHandler()
	c, _ := newTestClient(t, h)

	bad := map[string]Query{
		"field name; drop":        {Fields: []string{"name; drop table tabAccount"}},
		"field name)":             {Fields: []string{"name)"}},
		"field sleep(5)":          {Fields: []string{"sleep(5)"}},
		"field name`":             {Fields: []string{"name`"}},
		"field empty":             {Fields: []string{""}},
		"field aggregate":         {Fields: []string{"count(name)"}},
		"field star":              {Fields: []string{"*"}},
		"field alias":             {Fields: []string{"name as n"}},
		"field leading digit":     {Fields: []string{"1name"}},
		"field space":             {Fields: []string{"posting date"}},
		"field bad table":         {Fields: []string{"`tabGL Entry`;.name"}},
		"field unclosed table":    {Fields: []string{"`tabGL Entry.name"}},
		"field newline":           {Fields: []string{"name\n"}},
		"order subquery":          {OrderBy: "name desc, (select 1)"},
		"order limit":             {OrderBy: "name asc limit 1"},
		"order no direction":      {OrderBy: "name"},
		"order sleep":             {OrderBy: "sleep(5) asc"},
		"order trailing comma":    {OrderBy: "name asc,"},
		"order semicolon":         {OrderBy: "name asc; drop"},
		"order comment":           {OrderBy: "name asc -- x"},
		"order backquote":         {OrderBy: "name` asc"},
		"order tab separator":     {OrderBy: "name\tasc"},
		"filter x or 1=1":         {Filters: [][]any{{"x or 1=1", "=", 1}}},
		"filter name)":            {Filters: [][]any{{"name)", "=", 1}}},
		"filter non-string field": {Filters: [][]any{{1, "=", 1}}},
		"filter short":            {Filters: [][]any{{"name", "="}}},
		"filter long":             {Filters: [][]any{{"GL Entry", "name", "=", 1, 2}}},
		"filter bad doctype":      {Filters: [][]any{{"GL Entry`", "name", "=", 1}}},
		"filter 4 bad field":      {Filters: [][]any{{"GL Entry", "name; drop", "=", 1}}},
		"filter bad operator":     {Filters: [][]any{{"name", "= 1 or 1=1 --", 1}}},
		"filter numeric operator": {Filters: [][]any{{"name", 1, 1}}},
		"filter non-ASCII op":     {Filters: [][]any{{"name", "L\u0130KE", "%x%"}}},
		"filter Kelvin op":        {Filters: [][]any{{"name", "not \u212Aike", "%x%"}}},
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := List[map[string]any](t.Context(), c, "GL Entry", q)
			if !errors.Is(err, ErrUnsafeQuery) {
				t.Fatalf("err = %v, want ErrUnsafeQuery", err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("rejected queries still sent %d requests", n)
	}

	// The shapes current callers use (internal/books, internal/checks,
	// internal/seed, the integration tests) still pass.
	good := map[string]Query{
		"books gl entries": {
			Fields: Fields[GLEntryRaw](),
			Filters: [][]any{
				{"company", "=", "Sharma Traders Pvt Ltd"},
				{"posting_date", "between", []string{"2026-09-01", "2026-09-30"}},
				{"is_cancelled", "=", 0},
			},
			OrderBy: "name asc",
		},
		"books accounts": {Fields: Fields[AccountRaw](), Filters: [][]any{{"company", "=", "Sharma Traders Pvt Ltd"}}, OrderBy: "name asc"},
		"checks submitted": {
			Fields:  []string{"name"},
			Filters: [][]any{{"company", "=", "X"}, {"docstatus", "=", 1}, {"posting_date", "<=", "2026-09-30"}, {"posting_date", ">=", "2026-09-01"}},
			OrderBy: "posting_date asc, name asc",
		},
		"seed existing": {
			Fields:  []string{"name", "copilot_ext_id", "docstatus"},
			Filters: [][]any{{"copilot_ext_id", "is", "set"}, {"docstatus", "in", []int{0, 1, 2}}},
			OrderBy: "name asc",
		},
		"seed bootstrap": {
			Fields:  []string{"name", "supplier_name", "gst_category"},
			Filters: [][]any{{"supplier_name", "in", []string{"A", "B"}}},
			OrderBy: "name asc",
		},
		"fiscal year":       {Fields: []string{"name", "year_start_date", "year_end_date"}, OrderBy: "year_start_date asc"},
		"table-qualified":   {Fields: []string{"`tabGL Entry`.name"}, OrderBy: "`tabGL Entry`.posting_date DESC , name Asc"},
		"four-part filter":  {Filters: [][]any{{"Journal Entry Account", "account", "=", "Cash - STPL"}}},
		"operator any case": {Filters: [][]any{{"name", "NOT IN", []string{"a"}}, {"title", "Like", "%x%"}}},
		"empty":             {},
	}
	for name, q := range good {
		t.Run("ok "+name, func(t *testing.T) {
			if _, err := List[map[string]any](t.Context(), c, "GL Entry", q); err != nil {
				t.Fatalf("a current caller's query was refused: %v", err)
			}
		})
	}
}

func TestCallMethodAllowlist(t *testing.T) {
	h, hits := countingHandler()
	c, _ := newTestClient(t, h)

	for _, m := range []string{
		"frappe.client.set_value", "frappe.client.insert", "frappe.client.save",
		"frappe.client.submit", "frappe.client.cancel", "frappe.client.delete",
		"frappe.client.rename_doc", "frappe.client.insert_many", "frappe.client.bulk_update",
		"Frappe.Client.Delete", "frappe.client.get_list.x",
	} {
		t.Run("GET "+m, func(t *testing.T) {
			if _, err := Call[any](t.Context(), c, http.MethodGet, m, nil); !errors.Is(err, ErrUnsafeQuery) {
				t.Fatalf("err = %v, want ErrUnsafeQuery", err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("refused calls still sent %d requests", n)
	}
	for _, m := range []string{
		"frappe.client.get", "frappe.client.get_list", "frappe.client.get_count", "frappe.client.get_value",
		"frappe.desk.query_report.run", "frappe.auth.get_logged_user",
	} {
		if _, err := Call[any](t.Context(), c, http.MethodGet, m, nil); err != nil {
			t.Errorf("GET %s: %v", m, err)
		}
	}
	// POST to a write method is allowed (and never retried, below).
	if _, err := Call[any](t.Context(), c, http.MethodPost, "frappe.client.set_value", nil); err != nil {
		t.Errorf("POST frappe.client.set_value: %v", err)
	}
}

func TestCallQueryParams(t *testing.T) {
	h, hits := countingHandler()
	c, _ := newTestClient(t, h)

	bad := []struct {
		method string
		params map[string]any
	}{
		{"get_list", map[string]any{"doctype": "GL Entry", "fields": []string{"name", "sleep(5)"}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "fields": `["name","(select 1)"]`}},
		{"get_list", map[string]any{"doctype": "GL Entry", "fields": "sleep(5)"}},
		{"get_list", map[string]any{"doctype": "GL Entry", "order_by": "name desc, (select 1)"}},
		{"get_list", map[string]any{"doctype": "GL Entry", "order_by": "sleep(5) asc"}},
		{"get_list", map[string]any{"doctype": "GL Entry", "order_by": []string{"name asc"}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": [][]any{{"x or 1=1", "=", 1}}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": `[["name","= 1 or sleep(5) --","x"]]`}},
		{"get_list", map[string]any{"doctype": "GL Entry", "or_filters": [][]any{{"(select 1)", "=", 1}}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": map[string]any{"name; drop": 1}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": map[string]any{"name": []any{"= 1 or", 1}}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": `{"sleep(5)": 1}`}},
		{"get_list", map[string]any{"doctype": "GL Entry", "filters": []any{"name"}}},
		{"get_list", map[string]any{"doctype": "GL Entry", "group_by": "name"}},
		{"get_list", map[string]any{"doctype": "GL Entry", "debug": 1}},
		{"get_count", map[string]any{"doctype": "GL Entry", "filters": [][]any{{"name", "=", "x"}, {"sleep(5)", "=", 1}}}},
		{"get_count", map[string]any{"doctype": "GL Entry", "filters": "Cash - STPL"}},
		{"get_count", map[string]any{"doctype": "GL Entry", "fields": []string{"name"}}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": "sleep(5)"}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": `["name","(select 1)"]`}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": map[string]any{"a": "b"}}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": "name", "filters": map[string]any{"name": []any{"L\u0130KE", "x"}}}},
		{"get_value", map[string]any{"doctype": 7, "fieldname": "name"}},
	}
	for i, b := range bad {
		for _, hm := range []string{http.MethodGet, http.MethodPost} {
			t.Run(fmt.Sprintf("%d %s %s", i, hm, b.method), func(t *testing.T) {
				if _, err := Call[any](t.Context(), c, hm, "frappe.client."+b.method, b.params); !errors.Is(err, ErrUnsafeQuery) {
					t.Fatalf("params %v: err = %v, want ErrUnsafeQuery", b.params, err)
				}
			})
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("refused calls still sent %d requests", n)
	}

	good := []struct {
		method string
		params map[string]any
	}{
		{"get_list", map[string]any{
			"doctype": "GL Entry", "fields": []string{"name", "debit"}, "order_by": "posting_date asc, name asc",
			"filters":    [][]any{{"company", "=", "Sharma Traders Pvt Ltd"}, {"posting_date", "between", []string{"2026-09-01", "2026-09-30"}}},
			"or_filters": `[["account","like","%Bank%"]]`, "limit_start": 0, "limit_page_length": 500, "as_dict": 1,
		}},
		{"get_count", map[string]any{"doctype": "Journal Entry", "filters": [][]any{{"docstatus", "=", 1}}}},
		{"get_count", map[string]any{"doctype": "Journal Entry", "filters": map[string]any{"docstatus": 1, "name": []any{"in", []string{"a"}}}}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": []string{"name", "root_type"}, "filters": `{"name":"Cash - STPL"}`}},
		{"get_value", map[string]any{"doctype": "Account", "fieldname": "root_type", "filters": map[string]any{"name": "Cash - STPL"}}},
		{"get", map[string]any{"doctype": "Account", "name": "Cash - STPL"}},
	}
	for _, g := range good {
		if _, err := Call[any](t.Context(), c, http.MethodGet, "frappe.client."+g.method, g.params); err != nil {
			t.Errorf("%s %v: %v", g.method, g.params, err)
		}
	}
	if n := hits.Load(); n != int32(len(good)) {
		t.Errorf("valid calls sent %d requests, want %d", n, len(good))
	}
}

func TestCallNoRetry(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		writeRaw(w, http.StatusServiceUnavailable, `{}`)
	}))
	if _, err := Call[any](t.Context(), c, http.MethodPost, "frappe.client.submit", map[string]any{"doc": map[string]any{}}); err == nil {
		t.Fatal("want an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("POST call sent %d times, want 1", n)
	}
	calls.Store(0)
	if _, err := Call[any](t.Context(), c, http.MethodGet, "frappe.client.get_count", nil); err == nil {
		t.Fatal("want an error")
	}
	if n := calls.Load(); n != int32(MaxAttempts) {
		t.Errorf("GET read call sent %d times, want %d", n, MaxAttempts)
	}
	// A GET outside the frappe.client read allowlist is sent once.
	for _, m := range []string{"frappe.desk.query_report.run", "frappe.auth.get_logged_user", "erpnext.x.y"} {
		calls.Store(0)
		if _, err := Call[any](t.Context(), c, http.MethodGet, m, nil); err == nil {
			t.Fatalf("GET %s: want an error", m)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("GET %s sent %d times, want 1", m, n)
		}
	}
}

func TestPaginationRepeatedFirstRow(t *testing.T) {
	// The server ignores limit_start but changes a column between pages,
	// so the pages differ byte for byte while starting with the same row.
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		writeRaw(w, http.StatusOK, fmt.Sprintf(`{"data":[{"name":"GLE-1","modified":"%d"},{"name":"GLE-2","modified":"%d"}]}`, n, n))
	}))
	_, err := List[glRow](t.Context(), c, "GL Entry", Query{})
	if err == nil || !strings.Contains(err.Error(), "repeats the previous page") {
		t.Fatalf("err = %v, want a repeated-page error", err)
	}
	if strings.Contains(err.Error(), "ignoring limit_start") {
		t.Errorf("error still blames limit_start: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestPaginationMaxRows(t *testing.T) {
	if DefaultMaxRows != 200_000 {
		t.Errorf("DefaultMaxRows = %d, want 200000", DefaultMaxRows)
	}
	srv := &pagedGL{t: t, total: 25}
	c := newOptsClient(t, srv, Options{MaxRows: 10})
	c.pageSize = 4
	_, err := List[glRow](t.Context(), c, "GL Entry", Query{})
	if err == nil || !strings.Contains(err.Error(), "more than 10 rows") {
		t.Fatalf("err = %v, want the MaxRows error", err)
	}
	if n := len(srv.pages()); n != 3 {
		t.Errorf("pages = %d, want 3 (stop at the page that passes MaxRows)", n)
	}

	// Exactly MaxRows rows is fine, and so is a Limit within it.
	c2 := newOptsClient(t, &pagedGL{t: t, total: 10}, Options{MaxRows: 10})
	c2.pageSize = 4
	if rows, err := List[glRow](t.Context(), c2, "GL Entry", Query{}); err != nil || len(rows) != 10 {
		t.Errorf("10 rows under MaxRows 10: %d rows, err %v", len(rows), err)
	}
	if rows, err := List[glRow](t.Context(), c, "GL Entry", Query{Limit: 8}); err != nil || len(rows) != 8 {
		t.Errorf("Limit 8 under MaxRows 10: %d rows, err %v", len(rows), err)
	}
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"404 DoesNotExistError", http.StatusNotFound, `{"exc_type":"DoesNotExistError","exception":"Account X not found"}`, true},
		{"bare 404", http.StatusNotFound, `Not Found`, false},
		{"404 JSON without exc_type", http.StatusNotFound, `{"message":"nope"}`, false},
		{"404 PageDoesNotExistError", http.StatusNotFound, `{"exc_type":"PageDoesNotExistError"}`, false},
		{"404 ValidationError", http.StatusNotFound, `{"exc_type":"ValidationError"}`, false},
		{"417 DoesNotExistError", http.StatusExpectationFailed, `{"exc_type":"DoesNotExistError"}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeRaw(w, tt.status, tt.body)
			}))
			_, err := Get[map[string]any](t.Context(), c, "Account", "X")
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsNotFound(err); got != tt.want {
				t.Errorf("IsNotFound = %v, want %v (%v)", got, tt.want, err)
			}
		})
	}
}

func TestDeadline(t *testing.T) {
	if DefaultDeadline != 60*time.Second {
		t.Errorf("DefaultDeadline = %s, want 60s", DefaultDeadline)
	}
	const deadline = 300 * time.Millisecond
	const margin = 700 * time.Millisecond

	t.Run("slow server, context without deadline", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			writeRaw(w, http.StatusOK, `{"data":{}}`)
		}), Options{Deadline: deadline})
		start := time.Now()
		_, err := Get[map[string]any](context.Background(), c, "Account", "X")
		took := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if took > deadline+margin {
			t.Errorf("took %s, want at most %s", took, deadline+margin)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("calls = %d, want 1 (no retry once the deadline passed)", n)
		}
	})

	t.Run("retries stop at the deadline", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			time.Sleep(120 * time.Millisecond)
			writeRaw(w, http.StatusServiceUnavailable, `{}`)
		}), Options{Deadline: deadline})
		c.backoffBase, c.backoffMax = 50*time.Millisecond, 50*time.Millisecond
		start := time.Now()
		if _, err := Get[map[string]any](context.Background(), c, "Account", "X"); err == nil {
			t.Fatal("want an error")
		}
		if took := time.Since(start); took > deadline+margin {
			t.Errorf("took %s, want at most %s", took, deadline+margin)
		}
		if n := calls.Load(); n >= int32(MaxAttempts) {
			t.Errorf("calls = %d, want fewer than %d (the deadline cut the retries)", n, MaxAttempts)
		}
	})

	t.Run("Retry-After past the deadline fails fast", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "20")
			writeRaw(w, http.StatusTooManyRequests, `{}`)
		}), Options{Deadline: 5 * time.Second})
		start := time.Now()
		_, err := Get[map[string]any](context.Background(), c, "Account", "X")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
			t.Fatalf("err = %v, want the 429", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("took %s; a wait past the deadline must not be taken", took)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("calls = %d, want 1", n)
		}
	})

	t.Run("a caller's deadline wins", func(t *testing.T) {
		c := newOptsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			writeRaw(w, http.StatusOK, `{"data":{}}`)
		}), Options{Deadline: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		if _, err := Get[map[string]any](ctx, c, "Account", "X"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if took := time.Since(start); took > deadline+margin {
			t.Errorf("took %s, want at most %s", took, deadline+margin)
		}
	})

	t.Run("bad options", func(t *testing.T) {
		cfg := config.Config{ERPBaseURL: "http://localhost:8080"}
		for _, o := range []Options{{Deadline: -time.Second}, {MaxRows: -1}, {ListDeadline: -time.Second}} {
			if _, err := NewWithOptions(cfg, testKey, testSecret, o); err == nil {
				t.Errorf("NewWithOptions(%+v): want an error", o)
			}
		}
		c, err := New(cfg, testKey, testSecret)
		if err != nil {
			t.Fatal(err)
		}
		if c.deadline != DefaultDeadline || c.maxRows != DefaultMaxRows || c.listDeadline != DefaultListDeadline {
			t.Errorf("defaults: deadline %s, maxRows %d, listDeadline %s", c.deadline, c.maxRows, c.listDeadline)
		}
		if DefaultListDeadline != 5*time.Minute {
			t.Errorf("DefaultListDeadline = %s, want 5m", DefaultListDeadline)
		}
	})
}

// slowHandler waits d (or until the client gives up) before calling next.
func slowHandler(d time.Duration, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(d):
		}
		next(w, r)
	})
}

func TestDeadlineWholeCall(t *testing.T) {
	const (
		perRequest = 300 * time.Millisecond
		whole      = time.Second
		pageDelay  = 150 * time.Millisecond // under perRequest, so only the whole-call deadline can stop it
		margin     = 700 * time.Millisecond
	)
	opts := Options{Deadline: perRequest, ListDeadline: whole}

	t.Run("List of many slow pages", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, slowHandler(pageDelay, func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			writeRaw(w, http.StatusOK, fmt.Sprintf(`{"data":[{"name":"a%d"},{"name":"b%d"}]}`, n, n))
		}), opts)
		c.pageSize = 2
		start := time.Now()
		_, err := List[glRow](context.Background(), c, "GL Entry", Query{})
		took := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if took > whole+margin {
			t.Errorf("took %s, want at most %s", took, whole+margin)
		}
		if took < whole-100*time.Millisecond {
			t.Errorf("took %s; the per-request deadline fired before the whole-call one", took)
		}
		if n := calls.Load(); n < 3 {
			t.Errorf("only %d pages were fetched", n)
		}
	})

	t.Run("per-request deadline still applies inside List", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				writeRaw(w, http.StatusOK, `{"data":[{"name":"a"}]}`)
				return
			}
			<-r.Context().Done() // the second page hangs
		}), opts)
		start := time.Now()
		_, err := List[glRow](context.Background(), c, "GL Entry", Query{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if took := time.Since(start); took > perRequest+margin/2 {
			t.Errorf("took %s; the hung page should fail after the per-request %s", took, perRequest)
		}
	})

	t.Run("GetMany of many slow documents", func(t *testing.T) {
		c := newOptsClient(t, slowHandler(pageDelay, func(w http.ResponseWriter, r *http.Request) {
			writeRaw(w, http.StatusOK, `{"data":{"name":"`+docName(r)+`"}}`)
		}), opts)
		names := make([]string, 60) // 60 / MaxInFlight * 150 ms = 2.25 s, past the whole-call deadline
		for i := range names {
			names[i] = fmt.Sprintf("D%d", i)
		}
		start := time.Now()
		_, err := GetMany[map[string]any](context.Background(), c, "Account", names)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if took := time.Since(start); took > whole+margin {
			t.Errorf("took %s, want at most %s", took, whole+margin)
		}
	})

	t.Run("a caller's deadline replaces the whole-call one", func(t *testing.T) {
		var calls atomic.Int32
		c := newOptsClient(t, slowHandler(pageDelay, func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			if n > 12 {
				writeRaw(w, http.StatusOK, `{"data":[]}`)
				return
			}
			writeRaw(w, http.StatusOK, fmt.Sprintf(`{"data":[{"name":"a%d"}]}`, n))
		}), opts)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := List[glRow](ctx, c, "GL Entry", Query{}) // 13 pages * 150 ms, past ListDeadline
		if err != nil || len(rows) != 12 {
			t.Fatalf("rows = %d, err = %v; a caller deadline should allow the long call", len(rows), err)
		}
	})
}

// TestSecretFormatting formats a *Client, a Client and a Client held in an
// unexported struct field with every verb the spec names and through slog
// text and JSON; the secret must never appear.
func TestSecretFormatting(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler())
	raw := testSecret.Reveal()
	type holder struct {
		c Client
		p *Client
		s config.Secret
	}
	h := holder{c: *c, p: c, s: testSecret}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%p", "%s", "%q"} {
		for _, v := range []any{c, *c, h, &h, testSecret} {
			outputs = append(outputs, fmt.Sprintf(verb, v))
		}
	}
	var buf bytes.Buffer
	for _, hd := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		l := slog.New(hd)
		l.Info("client", "ptr", c, "val", *c, "holder", h, "holder_ptr", &h, "secret", testSecret)
	}
	outputs = append(outputs, buf.String())
	for i, out := range outputs {
		if strings.Contains(out, raw) || strings.Contains(out, raw[:8]) || strings.Contains(out, testKey) {
			t.Errorf("output %d leaks a credential: %s", i, out)
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", c), redacted) {
		t.Errorf("%%v of the client = %q, want it to say %q", fmt.Sprintf("%v", c), redacted)
	}
}
