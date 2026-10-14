package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
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

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	q := r.URL.Query()
	f := store.AuditFilter{Actor: q.Get("actor"), Server: q.Get("server"), Tool: q.Get("tool"), Limit: auditPageSize}
	bad := func(msg string) { http.Error(w, msg, http.StatusBadRequest) }
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			bad("from must be a date as YYYY-MM-DD")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			bad("to must be a date as YYYY-MM-DD")
			return
		}
		end := t.AddDate(0, 0, 1) // inclusive of that day
		f.To = &end
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			bad("before must be a row number")
			return
		}
		f.Before = n
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
	}
	for _, e := range rows {
		page.Rows = append(page.Rows, auditRow(e))
	}
	if next > 0 {
		v := url.Values{}
		for k, val := range map[string]string{"actor": f.Actor, "server": f.Server, "tool": f.Tool, "from": q.Get("from"), "to": q.Get("to")} {
			if val != "" {
				v.Set(k, val)
			}
		}
		v.Set("before", strconv.FormatInt(next, 10))
		page.OlderURL = "/audit?" + v.Encode()
	}
	s.render(w, r, http.StatusOK, templates.Audit(page))
}
