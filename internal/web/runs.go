package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// RunsStore is the slice of *store.Store the runs pages use.
type RunsStore interface {
	CreateCloseRun(ctx context.Context, r store.CloseRun) error
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
	ListCloseRuns(ctx context.Context, limit int) ([]store.RunSummary, error)
	ListFindingsByRun(ctx context.Context, runID uuid.UUID) ([]store.Finding, error)
	ListCompanies(ctx context.Context) ([]store.Company, error)
}

// RunExecutor drives an existing queued run to a terminal state and
// returns when it ends. The wiring passes agent.Workflow.ResumeClose.
type RunExecutor func(ctx context.Context, runID uuid.UUID) error

// RunsBackend is what Options.Runs carries.
type RunsBackend struct {
	Store   RunsStore
	Execute RunExecutor
	// Findings backs the finding pages (CC-1003). Nil means they answer 501.
	Findings FindingsStore
}

// runListLimit is how many past runs the runs page shows.
const runListLimit = 100

var monthRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// Start errors the handlers map to status codes.
var (
	errBusy       = errors.New("a close is already running for this company")
	errBadRequest = errors.New("bad request")
)

// timeline is the run states in display order.
var timeline = []string{store.RunQueued, store.RunRunning, store.RunDone, store.RunPartial, store.RunFailed}

func terminal(status string) bool {
	return status == store.RunDone || status == store.RunPartial || status == store.RunFailed
}

// runsService starts runs in the background and serves the runs pages.
type runsService struct {
	b   RunsBackend
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	busy map[string]struct{} // companies with a run in flight (this process only)
	base context.Context     // parent of every run's context
	wg   sync.WaitGroup
}

func newRunsService(b RunsBackend, log *slog.Logger, now func() time.Time) *runsService {
	return &runsService{b: b, log: log, now: now, busy: map[string]struct{}{}, base: context.Background()}
}

// setBase makes ctx the parent of runs started from now on.
func (rs *runsService) setBase(ctx context.Context) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.base = ctx
}

func (rs *runsService) baseCtx() context.Context {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.base
}

// drain waits for in-flight runs, at most timeout.
func (rs *runsService) drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { rs.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// start validates, creates the queued row and runs Execute in the
// background. The per-company lock is in memory: a restart forgets it.
func (rs *runsService) start(ctx context.Context, company, month string) (uuid.UUID, error) {
	if !monthRE.MatchString(month) {
		return uuid.Nil, fmt.Errorf("%w: month must be YYYY-MM", errBadRequest)
	}
	companies, err := rs.b.Store.ListCompanies(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("web: list companies: %w", err)
	}
	if !slices.ContainsFunc(companies, func(c store.Company) bool { return c.ID == company }) {
		return uuid.Nil, fmt.Errorf("%w: unknown company", errBadRequest)
	}

	rs.mu.Lock()
	if _, taken := rs.busy[company]; taken {
		rs.mu.Unlock()
		return uuid.Nil, errBusy
	}
	rs.busy[company] = struct{}{}
	runCtx := rs.base
	rs.wg.Add(1)
	rs.mu.Unlock()

	release := func() {
		rs.mu.Lock()
		delete(rs.busy, company)
		rs.mu.Unlock()
		rs.wg.Done()
	}
	id := uuid.New()
	if err := rs.b.Store.CreateCloseRun(ctx, store.CloseRun{ID: id, CompanyID: company, Month: month, Status: store.RunQueued}); err != nil {
		release()
		return uuid.Nil, fmt.Errorf("web: create close run: %w", err)
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				rs.log.ErrorContext(runCtx, "web: close run panicked", "run_id", id, "company", company, "month", month, "panic", fmt.Sprint(p))
			}
			release()
		}()
		rs.log.InfoContext(runCtx, "web: close run started", "run_id", id, "company", company, "month", month)
		if err := rs.b.Execute(runCtx, id); err != nil {
			rs.log.ErrorContext(runCtx, "web: close run ended with an error", "run_id", id, "company", company, "month", month, "err", err)
			return
		}
		rs.log.InfoContext(runCtx, "web: close run ended", "run_id", id, "company", company, "month", month)
	}()
	return id, nil
}

// --- findings: filter, sort and nesting ---

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

func rank(sev string) int {
	if r, ok := severityRank[sev]; ok {
		return r
	}
	return len(severityRank)
}

// sortedFindings returns fs ordered by severity (unknown last), amount
// descending (nil last, signed) and id.
func sortedFindings(fs []store.Finding) []store.Finding {
	out := slices.Clone(fs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a.Severity), rank(b.Severity); ra != rb {
			return ra < rb
		}
		switch {
		case a.AmountPaise != nil && b.AmountPaise != nil:
			if *a.AmountPaise != *b.AmountPaise {
				return *a.AmountPaise > *b.AmountPaise
			}
		case a.AmountPaise != nil:
			return true
		case b.AmountPaise != nil:
			return false
		}
		return a.ID.String() < b.ID.String()
	})
	return out
}

// filterOptions are the distinct types and statuses in fs, sorted.
func filterOptions(fs []store.Finding) (types, statuses []string) {
	for _, f := range fs {
		types = append(types, f.Type)
		statuses = append(statuses, f.Status)
	}
	slices.Sort(types)
	slices.Sort(statuses)
	return slices.Compact(types), slices.Compact(statuses)
}

// knownOrEmpty returns v when it is one of options, else "" (an unknown
// filter value is ignored).
func knownOrEmpty(v string, options []string) string {
	if slices.Contains(options, v) {
		return v
	}
	return ""
}

func matches(f store.Finding, typ, status string) bool {
	return (typ == "" || f.Type == typ) && (status == "" || f.Status == status)
}

// findingNode is a top-level finding with the variances nested under it.
type findingNode struct {
	f      store.Finding
	nested []store.Finding
}

// relatedParent returns the id a variance finding links to, when that is a
// non-variance finding of the same run.
func relatedParent(f store.Finding, byID map[uuid.UUID]store.Finding) (uuid.UUID, bool) {
	if f.Type != "variance" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(f.Keys["related"])
	if err != nil {
		return uuid.Nil, false
	}
	p, ok := byID[id]
	if !ok || p.Type == "variance" || p.RunID != f.RunID {
		return uuid.Nil, false
	}
	return id, true
}

// nestFindings builds the HTML rows from findings in display order. The
// filters apply to top-level rows; a nested variance follows its parent.
func nestFindings(sorted []store.Finding, typ, status string) []findingNode {
	byID := make(map[uuid.UUID]store.Finding, len(sorted))
	for _, f := range sorted {
		byID[f.ID] = f
	}
	children := map[uuid.UUID][]store.Finding{}
	var tops []store.Finding
	for _, f := range sorted {
		if pid, ok := relatedParent(f, byID); ok {
			children[pid] = append(children[pid], f)
			continue
		}
		tops = append(tops, f)
	}
	var out []findingNode
	for _, f := range tops {
		if matches(f, typ, status) {
			out = append(out, findingNode{f: f, nested: children[f.ID]})
		}
	}
	return out
}

func findingRow(f store.Finding) templates.FindingRow {
	r := templates.FindingRow{
		ID: f.ID.String(), Severity: f.Severity, Type: f.Type, Title: f.Title,
		Status: f.Status, Verified: f.Verified,
	}
	if f.AmountPaise != nil {
		r.Amount = f.AmountPaise.Format()
	}
	return r
}

// --- shared helpers ---

func formatCost(c float64) string { return fmt.Sprintf("$%.4f", c) }

func formatDuration(d time.Duration) string { return d.Round(time.Second).String() }

// runDuration is finished-started, empty if either is missing.
func runDuration(r store.CloseRun) string {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return ""
	}
	return formatDuration(r.FinishedAt.Sub(*r.StartedAt))
}

func countByType(fs []store.Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		m[f.Type]++
	}
	return m
}

const notFoundMsg = "not found"

// loadRun fetches the run named by the {id} path value. ok is false when a
// response (404 or 500) has been written already.
func (s *Server) loadRun(w http.ResponseWriter, r *http.Request, api bool) (store.CloseRun, bool) {
	notFound := func() {
		if api {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundMsg})
			return
		}
		http.Error(w, notFoundMsg, http.StatusNotFound)
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFound()
		return store.CloseRun{}, false
	}
	run, err := s.runs.b.Store.GetCloseRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		notFound()
		return store.CloseRun{}, false
	}
	if err != nil {
		s.serverError(w, r, api, err)
		return store.CloseRun{}, false
	}
	return run, true
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, api bool, err error) {
	s.log.ErrorContext(r.Context(), "web: runs", "path", r.URL.Path, "err", err)
	if api {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// runsHTML and runsAPI answer 501 until Options.Runs is set; the role check
// has run by then.
func (s *Server) runsHTML(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runs == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		h(w, r)
	})
}

func (s *Server) runsAPI(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runs == nil {
			notImplemented.ServeHTTP(w, r)
			return
		}
		h(w, r)
	})
}

// --- HTML handlers ---

func (s *Server) renderRuns(w http.ResponseWriter, r *http.Request, status int, p *principal, company, month, msg string) {
	ctx := r.Context()
	runs, err := s.runs.b.Store.ListCloseRuns(ctx, runListLimit)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	page := templates.RunsPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		CanStart: p.user.Role == RoleMaker, Company: company, Month: month, Error: msg,
	}
	if page.CanStart {
		cs, err := s.runs.b.Store.ListCompanies(ctx)
		if err != nil {
			s.serverError(w, r, false, err)
			return
		}
		for _, c := range cs {
			page.Companies = append(page.Companies, c.ID)
		}
	}
	for _, x := range runs {
		page.Runs = append(page.Runs, templates.RunRow{
			ID: x.ID.String(), Company: x.CompanyID, Month: x.Month, Status: x.Status,
			Findings: x.Findings, Cost: formatCost(x.CostUSD), Duration: runDuration(x.CloseRun),
		})
	}
	s.render(w, r, status, templates.Runs(page))
}

func (s *Server) handleRunsPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	s.renderRuns(w, r, http.StatusOK, p, "", "", "")
}

func (s *Server) handleRunsStartForm(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	company, month := r.PostFormValue("company"), r.PostFormValue("month")
	id, err := s.runs.start(r.Context(), company, month)
	switch {
	case err == nil:
		http.Redirect(w, r, "/runs/"+id.String(), http.StatusSeeOther)
	case errors.Is(err, errBadRequest):
		s.renderRuns(w, r, http.StatusBadRequest, p, company, month, err.Error())
	case errors.Is(err, errBusy):
		s.renderRuns(w, r, http.StatusConflict, p, company, month, err.Error())
	default:
		s.serverError(w, r, false, err)
	}
}

func (s *Server) panel(run store.CloseRun, fs []store.Finding) templates.RunPanel {
	p := templates.RunPanel{
		ID: run.ID.String(), Status: run.Status, Polling: !terminal(run.Status),
		Total: len(fs), InputTokens: run.InputTokens, OutputTokens: run.OutputTokens,
		CacheReadTokens: run.CacheReadTokens, Cost: formatCost(run.CostUSD),
	}
	for _, st := range timeline {
		p.Timeline = append(p.Timeline, templates.TimelineStep{Status: st, Current: st == run.Status})
	}
	counts := countByType(fs)
	for _, t := range slices.Sorted(maps.Keys(counts)) {
		p.Counts = append(p.Counts, templates.TypeCount{Type: t, Count: counts[t]})
	}
	switch {
	case run.StartedAt != nil && run.FinishedAt != nil:
		p.Duration = runDuration(run)
	case run.StartedAt != nil:
		p.Duration = formatDuration(s.now().Sub(*run.StartedAt))
	}
	if run.Error != nil {
		p.Error = *run.Error
	}
	return p
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	run, ok := s.loadRun(w, r, false)
	if !ok {
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), run.ID)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	s.render(w, r, http.StatusOK, templates.Run(templates.RunPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		Company: run.CompanyID, Month: run.Month, Panel: s.panel(run, fs),
	}))
}

func (s *Server) handleRunProgress(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r, false)
	if !ok {
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), run.ID)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	if terminal(run.Status) {
		w.Header().Set("HX-Redirect", "/runs/"+run.ID.String()+"/findings")
	}
	s.render(w, r, http.StatusOK, templates.RunProgress(s.panel(run, fs)))
}

func (s *Server) handleRunFindingsPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	run, ok := s.loadRun(w, r, false)
	if !ok {
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), run.ID)
	if err != nil {
		s.serverError(w, r, false, err)
		return
	}
	types, statuses := filterOptions(fs)
	typ := knownOrEmpty(r.URL.Query().Get("type"), types)
	status := knownOrEmpty(r.URL.Query().Get("status"), statuses)
	page := templates.FindingsPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		RunID: run.ID.String(), Company: run.CompanyID, Month: run.Month,
		Types: types, Statuses: statuses, SelType: typ, SelStatus: status,
	}
	for _, n := range nestFindings(sortedFindings(fs), typ, status) {
		row := findingRow(n.f)
		for _, c := range n.nested {
			row.Nested = append(row.Nested, findingRow(c))
		}
		page.Rows = append(page.Rows, row)
	}
	s.render(w, r, http.StatusOK, templates.RunFindings(page))
}

// --- JSON handlers ---

type startRequest struct {
	Company string `json:"company"`
	Month   string `json:"month"`
}

func (s *Server) handleAPIStartRun(w http.ResponseWriter, r *http.Request) {
	var in startRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be one JSON object with company and month"})
		return
	}
	id, err := s.runs.start(r.Context(), in.Company, in.Month)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, map[string]string{"run_id": id.String()})
	case errors.Is(err, errBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, errBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		s.serverError(w, r, true, err)
	}
}

type runJSON struct {
	ID              uuid.UUID      `json:"id"`
	Company         string         `json:"company"`
	Month           string         `json:"month"`
	Status          string         `json:"status"`
	StartedAt       *time.Time     `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at"`
	Error           *string        `json:"error"`
	FindingCounts   map[string]int `json:"finding_counts"`
	FindingsTotal   int            `json:"findings_total"`
	InputTokens     int64          `json:"input_tokens"`
	OutputTokens    int64          `json:"output_tokens"`
	CacheReadTokens int64          `json:"cache_read_tokens"`
	CostUSD         float64        `json:"cost_usd"`
}

func (s *Server) handleAPIRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r, true)
	if !ok {
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), run.ID)
	if err != nil {
		s.serverError(w, r, true, err)
		return
	}
	writeJSON(w, http.StatusOK, runJSON{
		ID: run.ID, Company: run.CompanyID, Month: run.Month, Status: run.Status,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Error: run.Error,
		FindingCounts: countByType(fs), FindingsTotal: len(fs),
		InputTokens: run.InputTokens, OutputTokens: run.OutputTokens,
		CacheReadTokens: run.CacheReadTokens, CostUSD: run.CostUSD,
	})
}

func (s *Server) handleAPIRunFindings(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r, true)
	if !ok {
		return
	}
	fs, err := s.runs.b.Store.ListFindingsByRun(r.Context(), run.ID)
	if err != nil {
		s.serverError(w, r, true, err)
		return
	}
	types, statuses := filterOptions(fs)
	typ := knownOrEmpty(r.URL.Query().Get("type"), types)
	status := knownOrEmpty(r.URL.Query().Get("status"), statuses)
	out := []store.Finding{}
	for _, f := range sortedFindings(fs) {
		if matches(f, typ, status) {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": out})
}
