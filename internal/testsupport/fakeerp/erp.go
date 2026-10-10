// Package fakeerp is a fake ERPNext and a synthetic month for tests (CC-702,
// moved here in CC-703): enough of the Frappe REST API for the ERPNext
// client, a month of books for one synthetic company with three planted,
// unrecorded bank charges on the statement, a helper that seeds the
// matching bank lines into a store, and the books and evidence MCP servers
// served over the fake in process.
//
// Only tests (and the test helper binary under cmd/) may import this
// package; deps_test.go enforces it. Every name, number and GSTIN here is
// invented.
package fakeerp

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Doc is one ERPNext document as the REST API returns it.
type Doc = map[string]any

// ERP is a fake ERPNext with enough of the REST API for the ERPNext client:
// list with filters (=, !=, <, <=, >, >=, between, in, and the child-table
// form [doctype, field, op, value]), order_by on any fields, limit_start
// and limit_page_length; and get by name. It is safe for concurrent use.
type ERP struct {
	mu   sync.RWMutex
	docs map[string][]Doc // doctype -> documents
}

// NewERP returns an empty fake.
func NewERP() *ERP {
	return &ERP{docs: map[string][]Doc{}}
}

// Add appends documents of a DocType.
func (f *ERP) Add(doctype string, d ...Doc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[doctype] = append(f.docs[doctype], d...)
}

// ServeHTTP serves GET /api/resource/<doctype>[/<name>].
func (f *ERP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/resource/")
	if !ok || r.Method != http.MethodGet {
		http.Error(w, "unexpected request", http.StatusNotFound)
		return
	}
	doctype, name, isGet := strings.Cut(rest, "/")
	w.Header().Set("Content-Type", "application/json")
	if isGet {
		for _, d := range f.docs[doctype] {
			if d["name"] == name {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": d})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"exc_type":"DoesNotExistError"}`))
		return
	}

	q := r.URL.Query()
	var filters [][]any
	if raw := q.Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			http.Error(w, "bad filters", http.StatusBadRequest)
			return
		}
	}
	var rows []Doc
	for _, d := range f.docs[doctype] {
		if match(d, filters) {
			rows = append(rows, listRow(d))
		}
	}
	sortRows(rows, q.Get("order_by"))
	start, _ := strconv.Atoi(q.Get("limit_start"))
	n, _ := strconv.Atoi(q.Get("limit_page_length"))
	if start < 0 {
		start = 0
	}
	if start > len(rows) {
		start = len(rows)
	}
	end := len(rows)
	if n > 0 && start+n < end {
		end = start + n
	}
	page := rows[start:end]
	if page == nil {
		page = []Doc{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
}

// listRow drops child tables, as Frappe's list API does.
func listRow(d Doc) Doc {
	out := Doc{}
	for k, v := range d {
		if _, isTable := v.([]Doc); !isTable {
			out[k] = v
		}
	}
	return out
}

func sortRows(rows []Doc, orderBy string) {
	var keys []string
	for part := range strings.SplitSeq(orderBy, ",") {
		if f := strings.Fields(part); len(f) > 0 {
			keys = append(keys, f[0])
		}
	}
	slices.SortStableFunc(rows, func(a, b Doc) int {
		for _, k := range keys {
			if c := cmp.Compare(fmt.Sprint(a[k]), fmt.Sprint(b[k])); c != 0 {
				return c
			}
		}
		return 0
	})
}

func match(d Doc, filters [][]any) bool {
	for _, flt := range filters {
		switch len(flt) {
		case 3:
			field, _ := flt[0].(string)
			op, _ := flt[1].(string)
			if !matchOp(fmt.Sprint(d[field]), op, flt[2]) {
				return false
			}
		case 4:
			// A child-table filter: some row of the table matches.
			field, _ := flt[1].(string)
			op, _ := flt[2].(string)
			rows, _ := d[childTable(flt[0])].([]Doc)
			if !slices.ContainsFunc(rows, func(r Doc) bool { return matchOp(fmt.Sprint(r[field]), op, flt[3]) }) {
				return false
			}
		}
	}
	return true
}

func childTable(doctype any) string {
	if doctype == "Payment Entry Reference" {
		return "references"
	}
	return ""
}

func matchOp(got, op string, want any) bool {
	switch op {
	case "=":
		return got == fmt.Sprint(want)
	case "!=":
		return got != fmt.Sprint(want)
	case "<":
		return got < fmt.Sprint(want)
	case "<=":
		return got <= fmt.Sprint(want)
	case ">":
		return got > fmt.Sprint(want)
	case ">=":
		return got >= fmt.Sprint(want)
	case "between":
		lim, _ := want.([]any)
		return len(lim) == 2 && got >= fmt.Sprint(lim[0]) && got <= fmt.Sprint(lim[1])
	case "in":
		vals, _ := want.([]any)
		return slices.ContainsFunc(vals, func(v any) bool { return fmt.Sprint(v) == got })
	}
	return false
}
