package frappe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type glRow struct {
	Name  string      `json:"name"`
	Debit json.Number `json:"debit"`
}

// pagedGL serves total GL Entry rows, honouring limit_start and
// limit_page_length, and records each request's query.
type pagedGL struct {
	t       *testing.T
	total   int
	capRows int // when > 0, never serve more rows per page than this

	mu      sync.Mutex
	queries []map[string]string
}

func (p *pagedGL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/api/resource/GL Entry" {
		p.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := map[string]string{}
	for k := range r.URL.Query() {
		q[k] = r.URL.Query().Get(k)
	}
	p.mu.Lock()
	p.queries = append(p.queries, q)
	p.mu.Unlock()
	start, _ := strconv.Atoi(q["limit_start"])
	n, _ := strconv.Atoi(q["limit_page_length"])
	if p.capRows > 0 {
		n = min(n, p.capRows)
	}
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := start; i < min(start+n, p.total); i++ {
		if i > start {
			b.WriteByte(',')
		}
		// 1234.50 must arrive as the exact string, never a float.
		fmt.Fprintf(&b, `{"name":"GLE-%05d","debit":1234.50}`, i)
	}
	b.WriteString(`]}`)
	writeRaw(w, http.StatusOK, b.String())
}

func (p *pagedGL) query(i int) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queries[i]
}

func (p *pagedGL) pages() [][2]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][2]string, len(p.queries))
	for i, q := range p.queries {
		out[i] = [2]string{q["limit_start"], q["limit_page_length"]}
	}
	return out
}

func TestListPagination(t *testing.T) {
	if PageSize != 500 || MaxPages != 10_000 {
		t.Fatalf("PageSize = %d, MaxPages = %d; want 500 and 10000", PageSize, MaxPages)
	}

	t.Run("three pages with encoded query", func(t *testing.T) {
		srv := &pagedGL{t: t, total: 1003}
		c, _ := newTestClient(t, srv)
		q := Query{
			Fields: []string{"name", "debit"},
			Filters: [][]any{
				{"company", "=", "Sharma Traders Pvt Ltd"},
				{"posting_date", "between", []string{"2026-09-01", "2026-09-30"}},
			},
			OrderBy: "posting_date asc, name asc",
		}
		rows, err := List[glRow](t.Context(), c, "GL Entry", q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1003 {
			t.Fatalf("got %d rows, want 1003", len(rows))
		}
		for i, r := range rows {
			if want := fmt.Sprintf("GLE-%05d", i); r.Name != want {
				t.Fatalf("row %d = %s, want %s (order kept)", i, r.Name, want)
			}
		}
		if rows[1002].Debit.String() != "1234.50" {
			t.Errorf("debit = %q, want the exact json.Number 1234.50", rows[1002].Debit)
		}
		// The short third page is not trusted as the end; an empty page is.
		want := [][2]string{{"0", "500"}, {"500", "500"}, {"1000", "500"}, {"1003", "500"}}
		if got := srv.pages(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("pages (limit_start, limit_page_length) = %v, want %v", got, want)
		}
		first := srv.query(0)
		if first["fields"] != `["name","debit"]` {
			t.Errorf("fields = %s", first["fields"])
		}
		if first["filters"] != `[["company","=","Sharma Traders Pvt Ltd"],["posting_date","between",["2026-09-01","2026-09-30"]]]` {
			t.Errorf("filters = %s", first["filters"])
		}
		if first["order_by"] != "posting_date asc, name asc" {
			t.Errorf("order_by = %s", first["order_by"])
		}
	})

	t.Run("empty query sends no fields, filters or order_by", func(t *testing.T) {
		srv := &pagedGL{t: t, total: 2}
		c, _ := newTestClient(t, srv)
		rows, err := List[glRow](t.Context(), c, "GL Entry", Query{})
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows = %d, err = %v", len(rows), err)
		}
		for _, k := range []string{"fields", "filters", "order_by"} {
			if _, ok := srv.query(0)[k]; ok {
				t.Errorf("query has %s for an empty Query", k)
			}
		}
	})

	t.Run("no rows is an empty slice", func(t *testing.T) {
		c, _ := newTestClient(t, &pagedGL{t: t, total: 0})
		rows, err := List[glRow](t.Context(), c, "GL Entry", Query{})
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("rows = %v, err = %v; want an empty, non-nil slice", rows, err)
		}
	})

	limits := []struct {
		limit, total, want int
		pages              [][2]string
	}{
		{3, 1003, 3, [][2]string{{"0", "3"}}},
		{700, 1003, 700, [][2]string{{"0", "500"}, {"500", "200"}}},
		{500, 1003, 500, [][2]string{{"0", "500"}}},
		{700, 600, 600, [][2]string{{"0", "500"}, {"500", "200"}, {"600", "100"}}},
	}
	for _, tt := range limits {
		t.Run(fmt.Sprintf("limit %d of %d", tt.limit, tt.total), func(t *testing.T) {
			srv := &pagedGL{t: t, total: tt.total}
			c, _ := newTestClient(t, srv)
			rows, err := List[glRow](t.Context(), c, "GL Entry", Query{Limit: tt.limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tt.want {
				t.Errorf("got %d rows, want %d", len(rows), tt.want)
			}
			if got := srv.pages(); fmt.Sprint(got) != fmt.Sprint(tt.pages) {
				t.Errorf("pages = %v, want %v", got, tt.pages)
			}
		})
	}

	// A server (or proxy) that caps pages below the requested length must
	// not make List drop rows silently.
	capped := []struct {
		name               string
		total, capRows, lm int
		want               int
		pages              [][2]string
	}{
		{"capped at 2 of 4", 4, 2, 0, 4, [][2]string{{"0", "500"}, {"2", "500"}, {"4", "500"}}},
		{"capped at 2 of 5", 5, 2, 0, 5, [][2]string{{"0", "500"}, {"2", "500"}, {"4", "500"}, {"5", "500"}}},
		{"capped at 2 with limit 3", 4, 2, 3, 3, [][2]string{{"0", "3"}, {"2", "1"}}},
	}
	for _, tt := range capped {
		t.Run(tt.name, func(t *testing.T) {
			srv := &pagedGL{t: t, total: tt.total, capRows: tt.capRows}
			c, _ := newTestClient(t, srv)
			rows, err := List[glRow](t.Context(), c, "GL Entry", Query{Limit: tt.lm})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tt.want {
				t.Fatalf("got %d rows, want %d: a capped page was taken as the last", len(rows), tt.want)
			}
			for i, r := range rows {
				if want := fmt.Sprintf("GLE-%05d", i); r.Name != want {
					t.Errorf("row %d = %s, want %s", i, r.Name, want)
				}
			}
			if got := srv.pages(); fmt.Sprint(got) != fmt.Sprint(tt.pages) {
				t.Errorf("pages = %v, want %v", got, tt.pages)
			}
		})
	}

	t.Run("negative limit", func(t *testing.T) {
		c, _ := newTestClient(t, &pagedGL{t: t})
		if _, err := List[glRow](t.Context(), c, "GL Entry", Query{Limit: -1}); err == nil {
			t.Error("want an error for a negative limit")
		}
	})

	t.Run("server ignoring limit_start stops on a repeated page", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			n, _ := strconv.Atoi(r.URL.Query().Get("limit_page_length"))
			var b strings.Builder
			b.WriteString(`{"data":[`)
			for i := range n {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"name":"GLE-%05d"}`, i)
			}
			b.WriteString(`]}`)
			writeRaw(w, http.StatusOK, b.String())
		}))
		_, err := List[glRow](t.Context(), c, "GL Entry", Query{})
		if err == nil || !strings.Contains(err.Error(), "repeats the previous page") {
			t.Fatalf("err = %v, want a repeated-page error", err)
		}
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})

	t.Run("endless distinct pages stop at the page cap", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			writeRaw(w, http.StatusOK, fmt.Sprintf(`{"data":[{"name":"a%d"},{"name":"b%d"}]}`, n, n))
		}))
		c.pageSize, c.maxPages = 2, 5
		_, err := List[glRow](t.Context(), c, "GL Entry", Query{})
		if err == nil || !strings.Contains(err.Error(), "stopped after 5 pages") {
			t.Fatalf("err = %v, want the page-cap error", err)
		}
		if calls.Load() != 5 {
			t.Errorf("calls = %d, want 5", calls.Load())
		}
	})

	t.Run("bad doctype", func(t *testing.T) {
		c, _ := newTestClient(t, &pagedGL{t: t})
		for _, dt := range []string{"", "GL/Entry", " GL Entry"} {
			if _, err := List[glRow](t.Context(), c, dt, Query{}); err == nil {
				t.Errorf("List(%q): want an error", dt)
			}
		}
	})
}
