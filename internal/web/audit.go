package web

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// AuditStore is the slice of *store.Store the audit and metrics pages use.
type AuditStore interface {
	ListAuditPage(ctx context.Context, f store.AuditFilter) ([]store.AuditLogEntry, int64, error)
	ListAuditFacets(ctx context.Context) (actors, servers, tools []string, err error)
	RunMetricsByStep(ctx context.Context, runID uuid.UUID) ([]store.StepUsage, error)
	RunsSummaryStats(ctx context.Context) (store.LastRunStats, error)
}

var _ AuditStore = (*store.Store)(nil)

const (
	auditPageSize = 50
	// auditArgsMax is how many characters of the arguments show in the table.
	auditArgsMax = 120
	dateLayout   = "2006-01-02"
)

// auditRoute answers 501 until Options.Audit is set; the role check has run
// by then.
func (s *Server) auditRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.audit == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		h(w, r)
	})
}

// truncateRunes cuts s to at most n runes, adding an ellipsis when it cut.
func truncateRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]) + "…", true
}

// compactAuditArgs returns args as compact JSON; text that is not valid JSON is
// returned as it is.
func compactAuditArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var b bytes.Buffer
	if err := json.Compact(&b, args); err != nil {
		return string(args)
	}
	return b.String()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func auditRow(e store.AuditLogEntry) templates.AuditRow {
	full := compactAuditArgs(e.Args)
	short, cut := truncateRunes(full, auditArgsMax)
	return templates.AuditRow{
		Time: e.At.UTC().Format(time.RFC3339), Actor: e.Actor,
		Server: derefStr(e.Server), Tool: derefStr(e.Tool),
		ArgsShort: short, ArgsFull: full, Truncated: cut,
		ResultCount: derefInt(e.ResultCount), Latency: derefInt(e.LatencyMS), Error: derefStr(e.Error),
	}
}

// withSelected adds sel to opts when it is missing, so a filter value
// outside the capped facet list still shows.
func withSelected(opts []string, sel string) []string {
	if sel == "" {
		return opts
	}
	for _, o := range opts {
		if o == sel {
			return opts
		}
	}
	return append(opts[:len(opts):len(opts)], sel)
}

// parseAuditFilter reads the audit filter from the query. On a bad value it
// returns the 400 message and ok false.
func parseAuditFilter(r *http.Request) (f store.AuditFilter, msg string, ok bool) {
	q := r.URL.Query()
	f = store.AuditFilter{Actor: q.Get("actor"), Server: q.Get("server"), Tool: q.Get("tool"), Limit: auditPageSize}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			return f, "from must be a date as YYYY-MM-DD", false
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			return f, "to must be a date as YYYY-MM-DD", false
		}
		end := t.AddDate(0, 0, 1) // inclusive of that day
		f.To = &end
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, "before must be a row number", false
		}
		f.Before = n
	}
	return f, "", true
}

// auditFilterKeys are the query parameters that make up a filter, in the
// order chips show.
var auditFilterKeys = []struct{ key, label string }{
	{"actor", "Actor"}, {"server", "Server"}, {"tool", "Tool"}, {"from", "From"}, {"to", "To"},
}

// auditFilterValues returns the filter parameters of q, never before.
func auditFilterValues(q url.Values, drop string) url.Values {
	v := url.Values{}
	for _, k := range auditFilterKeys {
		if k.key != drop && q.Get(k.key) != "" {
			v.Set(k.key, q.Get(k.key))
		}
	}
	return v
}

func auditURL(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	q := r.URL.Query()
	f, msg, ok := parseAuditFilter(r)
	if !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	rows, next, err := s.audit.ListAuditPage(r.Context(), f)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	actors, servers, tools, err := s.audit.ListAuditFacets(r.Context())
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	page := templates.AuditPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		Actors: withSelected(actors, f.Actor), Servers: withSelected(servers, f.Server), Tools: withSelected(tools, f.Tool),
		SelActor: f.Actor, SelServer: f.Server, SelTool: f.Tool, From: q.Get("from"), To: q.Get("to"),
		ClearURL:  "/audit",
		ExportURL: auditURL("/audit/export.csv", auditFilterValues(q, "")),
	}
	for _, k := range auditFilterKeys {
		if v := q.Get(k.key); v != "" {
			page.Chips = append(page.Chips, templates.FilterChip{
				Label: k.label, Value: v, RemoveURL: auditURL("/audit", auditFilterValues(q, k.key)),
			})
		}
	}
	for _, e := range rows {
		page.Rows = append(page.Rows, auditRow(e))
	}
	if next > 0 {
		v := auditFilterValues(q, "")
		v.Set("before", strconv.FormatInt(next, 10))
		page.OlderURL = "/audit?" + v.Encode()
	}
	s.render(w, r, http.StatusOK, templates.Audit(page))
}

const (
	// auditExportCap is the most rows one CSV download carries.
	auditExportCap = 5000
	// auditExportPage is the page size used to read the store.
	auditExportPage = 200
)

var auditCSVHeader = []string{"time_utc", "actor", "server", "tool", "args", "result_count", "latency_ms", "error"}

// csvSafe stops a spreadsheet from running a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func auditCSVRecord(e store.AuditLogEntry) []string {
	rec := []string{
		e.At.UTC().Format(time.RFC3339), e.Actor, derefStr(e.Server), derefStr(e.Tool),
		compactAuditArgs(e.Args), derefInt(e.ResultCount), derefInt(e.LatencyMS), derefStr(e.Error),
	}
	for i, c := range rec {
		rec[i] = csvSafe(c)
	}
	return rec
}

// handleAuditExport downloads the filtered audit log as CSV, at most
// auditExportCap rows. It never logs arguments or actors.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	f, msg, ok := parseAuditFilter(r)
	if !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	f.Limit, f.Before = auditExportPage, 0
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(auditCSVHeader)
	total, truncated := 0, false
	for {
		if err := r.Context().Err(); err != nil {
			return // client gone
		}
		rows, next, err := s.audit.ListAuditPage(r.Context(), f)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.serverError(w, r, false, err)
			return
		}
		for _, e := range rows {
			if total == auditExportCap {
				break
			}
			_ = cw.Write(auditCSVRecord(e))
			total++
		}
		if next == 0 {
			break
		}
		if total >= auditExportCap {
			truncated = true
			break
		}
		f.Before = next
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.serverError(w, r, false, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="audit-`+s.now().UTC().Format("20060102")+`.csv"`)
	h.Set("Cache-Control", "no-store")
	if truncated {
		h.Set("X-Export-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	s.log.InfoContext(r.Context(), "web: audit export", "rows", total, "user", p.user.Name)
}
