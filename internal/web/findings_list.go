package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

const (
	findingsPerPage  = 25
	maxBulkAccept    = 50
	maxFindingsQuery = 100 // bytes of the q filter
	findingsRunScan  = 200 // runs read to find the newest for a company and month
)

var (
	listSeverities = []string{"high", "medium", "low"}
	listStatuses   = []string{"open", "needs_review", "accepted", "dismissed"}
	listSortKeys   = []string{"severity", "amount", "type", "status", "title"}
)

// listQuery is the validated filter, sort and page of the list.
type listQuery struct {
	company, month                     string
	severity, typ, status, verified, q string
	sortKey, dir                       string // empty: the default
	page                               int
}

// parseListQuery reads the filters from v; unknown values are ignored. types
// are the finding types present in the run.
func parseListQuery(v url.Values, types []string) listQuery {
	var q listQuery
	q.severity = knownOrEmpty(v.Get("severity"), listSeverities)
	q.typ = knownOrEmpty(v.Get("type"), types)
	q.status = knownOrEmpty(v.Get("status"), listStatuses)
	q.verified = knownOrEmpty(v.Get("verified"), []string{"yes", "no"})
	q.q = capQuery(strings.TrimSpace(v.Get("q")))
	q.sortKey = knownOrEmpty(v.Get("sort"), listSortKeys)
	q.dir = knownOrEmpty(v.Get("dir"), []string{"asc", "desc"})
	if n, err := strconv.Atoi(v.Get("page")); err == nil && n > 1 {
		q.page = n
	}
	return q
}

// capQuery cuts s to maxFindingsQuery bytes on a rune boundary.
func capQuery(s string) string {
	if len(s) <= maxFindingsQuery {
		return s
	}
	cut := maxFindingsQuery
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func defaultDir(key string) string {
	if key == "amount" {
		return "desc"
	}
	return "asc"
}

// effSort is the sort key and direction in force.
func (q listQuery) effSort() (key, dir string) {
	key = q.sortKey
	if key == "" {
		key = "severity"
	}
	dir = q.dir
	if dir == "" {
		dir = defaultDir(key)
	}
	return key, dir
}

// values is the query string that reproduces q. Company and month are always
// there, so a link works whatever the top bar says later.
func (q listQuery) values() url.Values {
	v := url.Values{"company": {q.company}, "month": {q.month}}
	for k, x := range map[string]string{
		"severity": q.severity, "type": q.typ, "status": q.status, "verified": q.verified,
		"q": q.q, "sort": q.sortKey, "dir": q.dir,
	} {
		if x != "" {
			v.Set(k, x)
		}
	}
	if q.page > 1 {
		v.Set("page", strconv.Itoa(q.page))
	}
	return v
}

func (q listQuery) url() string { return "/findings?" + q.values().Encode() }

// with returns a copy changed by f.
func (q listQuery) with(f func(*listQuery)) listQuery {
	f(&q)
	return q
}

// listNode is a top-level finding with its nested variances.
type listNode = findingNode

func (q listQuery) keeps(f store.Finding, withStatus bool) bool {
	switch {
	case q.severity != "" && f.Severity != q.severity:
		return false
	case q.typ != "" && f.Type != q.typ:
		return false
	case withStatus && q.status != "" && f.Status != q.status:
		return false
	case q.verified == "yes" && !f.Verified, q.verified == "no" && f.Verified:
		return false
	case q.q != "" && !strings.Contains(strings.ToLower(f.Title), strings.ToLower(q.q)):
		return false
	}
	return true
}

func compareFindings(key string, a, b store.Finding) int {
	switch key {
	case "severity":
		return cmp.Compare(rank(a.Severity), rank(b.Severity))
	case "type":
		return strings.Compare(a.Type, b.Type)
	case "status":
		return strings.Compare(a.Status, b.Status)
	case "title":
		return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	}
	return 0
}

// sortNodes orders nodes by key and dir. It is stable over the default
// order (severity, amount, id), so ties keep a fixed order. A missing amount
// sorts last whichever way the amounts go.
func sortNodes(nodes []listNode, key, dir string) {
	sign := 1
	if dir == "desc" {
		sign = -1
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].f, nodes[j].f
		if key == "amount" {
			switch {
			case a.AmountPaise == nil || b.AmountPaise == nil:
				return a.AmountPaise != nil && b.AmountPaise == nil
			default:
				return sign*cmp.Compare(int64(*a.AmountPaise), int64(*b.AmountPaise)) < 0
			}
		}
		return sign*compareFindings(key, a, b) < 0
	})
}

// listContext is the company and month of the request: the top bar's
// choice, overridden by valid company and month values in v.
func (s *Server) listContext(r *http.Request, v url.Values) (company, month string) {
	companies := s.shellCompanies(r.Context())
	company, month = s.resolveContext(r, companies)
	if c := v.Get("company"); validCompany(c, companies) {
		company = c
	}
	if m := v.Get("month"); validMonth(m) {
		month = m
	}
	return company, month
}

// findRun is the newest run for company and month; ok is false when there is none.
func (s *Server) findRun(ctx context.Context, company, month string) (store.CloseRun, bool, error) {
	runs, err := s.runs.b.Store.ListCloseRuns(ctx, findingsRunScan)
	if err != nil {
		return store.CloseRun{}, false, err
	}
	for _, x := range runs {
		if x.CompanyID == company && x.Month == month {
			return x.CloseRun, true, nil
		}
	}
	return store.CloseRun{}, false, nil
}

func (s *Server) handleFindingsList(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	v := r.URL.Query()
	company, month := s.listContext(r, v)
	s.renderFindingsList(w, r, http.StatusOK, p, company, month, v, nil, "")
}

// renderFindingsList reads the run's findings afresh and renders the list.
// v carries the filters; result and msg are the bulk accept's outcome.
func (s *Server) renderFindingsList(w http.ResponseWriter, r *http.Request, status int, p *principal,
	company, month string, v url.Values, result *templates.BulkResult, msg string) {
	ctx := r.Context()
	maker := p.user.Role == RoleMaker
	page := templates.FindingsListPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		Company: company, Month: month, CanBulk: maker, CanStart: maker,
		Result: result, Error: msg,
	}
	run, found, err := s.findRun(ctx, company, month)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	if !found {
		page.CanBulk = false
		s.render(w, r, status, templates.FindingsList(page))
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	page.HasRun = true
	types, _ := filterOptions(fs)
	q := parseListQuery(v, types)
	q.company, q.month = company, month

	all := nestFindings(sortedFindings(fs), "", "")
	var nodes []listNode
	counts := map[string]int{}
	byOthers := 0
	for _, n := range all {
		if !q.keeps(n.f, false) {
			continue
		}
		byOthers++
		counts[n.f.Status]++
		if q.status == "" || n.f.Status == q.status {
			nodes = append(nodes, n)
		}
	}
	key, dir := q.effSort()
	sortNodes(nodes, key, dir)

	pages := max(1, (len(nodes)+findingsPerPage-1)/findingsPerPage)
	q.page = min(max(q.page, 1), pages)
	lo := min((q.page-1)*findingsPerPage, len(nodes))
	hi := min(lo+findingsPerPage, len(nodes))
	for _, n := range nodes[lo:hi] {
		page.Rows = append(page.Rows, listRow(n.f, false, maker))
		for _, c := range n.nested {
			page.Rows = append(page.Rows, listRow(c, true, maker))
		}
	}
	page.Total = len(nodes)
	if len(nodes) > 0 {
		page.Showing = fmt.Sprintf("Showing %d-%d of %d", lo+1, hi, len(nodes))
	}
	if q.page > 1 {
		page.PrevURL = q.with(func(x *listQuery) { x.page = q.page - 1 }).url()
	}
	if q.page < pages {
		page.NextURL = q.with(func(x *listQuery) { x.page = q.page + 1 }).url()
	}

	page.Tabs = statusTabs(q, counts, byOthers)
	page.Columns = listColumns(q, key, dir)
	page.Types = types
	page.SelSev, page.SelType, page.SelVer, page.Query = q.severity, q.typ, q.verified, q.q
	page.Hidden = []templates.Hidden{{Name: "company", Value: company}, {Name: "month", Value: month}}
	if q.status != "" {
		page.Hidden = append(page.Hidden, templates.Hidden{Name: "status", Value: q.status})
	}
	if q.sortKey != "" {
		page.Hidden = append(page.Hidden, templates.Hidden{Name: "sort", Value: q.sortKey})
	}
	if q.dir != "" {
		page.Hidden = append(page.Hidden, templates.Hidden{Name: "dir", Value: q.dir})
	}
	page.ReturnURL = q.url()
	s.render(w, r, status, templates.FindingsList(page))
}

func listRow(f store.Finding, nested, maker bool) templates.ListRow {
	return templates.ListRow{
		FindingRow: findingRow(f), IsNested: nested,
		CanSelect: maker && (f.Status == "open" || f.Status == "needs_review"),
	}
}

func statusTabs(q listQuery, counts map[string]int, total int) []templates.Tab {
	tab := func(label, status string, n int) templates.Tab {
		return templates.Tab{
			Label: label, Count: n, Current: q.status == status,
			Href: q.with(func(x *listQuery) { x.status, x.page = status, 0 }).url(),
		}
	}
	return []templates.Tab{
		tab("All", "", total), tab("Open", "open", counts["open"]),
		tab("Needs review", "needs_review", counts["needs_review"]),
		tab("Accepted", "accepted", counts["accepted"]), tab("Dismissed", "dismissed", counts["dismissed"]),
	}
}

func listColumns(q listQuery, key, dir string) []templates.Column {
	col := func(label, k, align string) templates.Column {
		c := templates.Column{Label: label, Align: align}
		if k == "" {
			return c
		}
		next := defaultDir(k)
		if k == key {
			c.Sorted = dir
			next = "asc"
			if dir == "asc" {
				next = "desc"
			}
		}
		c.SortHref = q.with(func(x *listQuery) { x.sortKey, x.dir, x.page = k, next, 0 }).url()
		return c
	}
	return []templates.Column{
		col("Severity", "severity", ""), col("Type", "type", ""), col("Title", "title", ""),
		col("Amount", "amount", "right"), col("Status", "status", ""), col("Verified", "", ""), col("Detail", "", ""),
	}
}

// returnValues is the filter query of a bulk form's return value: only a
// /findings?... path (checked by safeNext) counts, anything else is empty.
func returnValues(ret string) url.Values {
	rest, ok := strings.CutPrefix(ret, "/findings?")
	if !ok || safeNext(ret) != ret {
		return url.Values{}
	}
	v, err := url.ParseQuery(rest)
	if err != nil {
		return url.Values{}
	}
	return v
}

// handleFindingsAcceptSelected accepts the ticked findings one by one with
// decideOne, the code a single accept runs, so every check and the audit row
// apply to each. The ids are checked against the run being listed, never
// trusted from the form. The outcome is rendered in the response itself.
func (s *Server) handleFindingsAcceptSelected(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	company, month := s.listContext(r, r.PostForm)
	v := returnValues(r.PostForm.Get("return"))
	refuse := func(msg string) {
		s.renderFindingsList(w, r, http.StatusBadRequest, p, company, month, v, nil, msg+" Nothing was changed.")
	}
	raw := r.PostForm["ids"]
	switch {
	case len(raw) == 0:
		refuse("Select at least one finding.")
		return
	case len(raw) > maxBulkAccept:
		refuse(fmt.Sprintf("Select at most %d findings at a time.", maxBulkAccept))
		return
	}
	ids := make([]uuid.UUID, 0, len(raw))
	for _, x := range raw {
		id, err := uuid.Parse(x)
		if err != nil {
			refuse("One of the selected ids is not valid.")
			return
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	run, found, err := s.findRun(ctx, company, month)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	if !found {
		refuse("There is no close run for " + company + " " + month + ".")
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	listed := make(map[uuid.UUID]bool, len(fs))
	for _, f := range fs {
		listed[f.ID] = true
	}

	res := &templates.BulkResult{}
	for _, id := range ids {
		if !listed[id] {
			res.Refused = append(res.Refused, templates.Refusal{Title: id.String(), Reason: "not in this view"})
			continue
		}
		d := s.decideOne(ctx, p, id, "accepted", "finding.accept", "")
		switch d.Kind {
		case decideDone:
			res.Accepted++
		case decideNotFound:
			res.Refused = append(res.Refused, templates.Refusal{Title: id.String(), Reason: "not found"})
		case decideFailed:
			s.log.ErrorContext(ctx, "web: bulk accept", "finding", id.String(), "err", d.Err)
			title := id.String()
			if d.Finding.Title != "" {
				title = d.Finding.Title
			}
			res.Refused = append(res.Refused, templates.Refusal{Title: title, Reason: "could not be saved, try again"})
		default:
			res.Refused = append(res.Refused, templates.Refusal{Title: d.Finding.Title, Reason: d.Msg})
		}
	}
	s.renderFindingsList(w, r, http.StatusOK, p, company, month, v, res, "")
}
