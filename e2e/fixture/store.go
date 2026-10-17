// Package fixture is an in-memory Close Copilot web server for the browser
// suite (UI-14). Every backend is a fake seeded with synthetic data: no
// Postgres, no ERPNext, no model, no network. It is for tests only.
package fixture

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// Store is the in-memory RunsStore, FindingsStore and AuditStore.
type Store struct {
	mu        sync.Mutex
	clock     time.Time
	companies []store.Company
	runs      map[uuid.UUID]store.CloseRun
	findings  map[uuid.UUID]store.Finding
	order     []uuid.UUID // finding ids in insertion order
	steps     map[uuid.UUID][]store.Step
	arts      map[string]store.Artifact
	proposals map[uuid.UUID]store.JournalProposal
	audits    []store.AuditLogEntry
	nextAudit int64
	stepDelay time.Duration
}

func newStore(base time.Time) *Store {
	return &Store{
		clock:     base,
		runs:      map[uuid.UUID]store.CloseRun{},
		findings:  map[uuid.UUID]store.Finding{},
		steps:     map[uuid.UUID][]store.Step{},
		arts:      map[string]store.Artifact{},
		proposals: map[uuid.UUID]store.JournalProposal{},
		stepDelay: 350 * time.Millisecond,
	}
}

// tick returns a strictly increasing fake time, so ordering is stable and the
// pages never show the wall clock.
func (s *Store) tick() time.Time {
	s.clock = s.clock.Add(time.Minute)
	return s.clock
}

// CreateCloseRun implements web.RunsStore.
func (s *Store) CreateCloseRun(_ context.Context, r store.CloseRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = r
	return nil
}

// GetCloseRun implements web.RunsStore and web.FindingsStore.
func (s *Store) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return store.CloseRun{}, fmt.Errorf("%w: run", store.ErrNotFound)
	}
	return r, nil
}

func (s *Store) findingsOf(run uuid.UUID) []store.Finding {
	var out []store.Finding
	for _, id := range s.order {
		if f := s.findings[id]; f.RunID == run {
			out = append(out, f)
		}
	}
	return out
}

// ListCloseRuns implements web.RunsStore: queued runs first, then newest.
func (s *Store) ListCloseRuns(_ context.Context, limit int) ([]store.RunSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.RunSummary, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, store.RunSummary{CloseRun: r, Findings: len(s.findingsOf(r.ID))})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].StartedAt, out[j].StartedAt
		switch {
		case a == nil && b != nil:
			return true
		case a != nil && b == nil:
			return false
		case a != nil && !a.Equal(*b):
			return a.After(*b)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListFindingsByRun implements web.RunsStore.
func (s *Store) ListFindingsByRun(_ context.Context, id uuid.UUID) ([]store.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findingsOf(id), nil
}

// ListCompanies implements web.RunsStore.
func (s *Store) ListCompanies(context.Context) ([]store.Company, error) {
	return slices.Clone(s.companies), nil
}

// GetFinding implements web.FindingsStore.
func (s *Store) GetFinding(_ context.Context, id uuid.UUID) (store.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.findings[id]
	if !ok {
		return store.Finding{}, fmt.Errorf("%w: finding", store.ErrNotFound)
	}
	return f, nil
}

// ListSteps implements web.FindingsStore.
func (s *Store) ListSteps(_ context.Context, runID uuid.UUID) ([]store.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.steps[runID]), nil
}

// GetArtifact implements web.FindingsStore.
func (s *Store) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.arts[sha]
	if !ok {
		return store.Artifact{}, fmt.Errorf("%w: artifact", store.ErrNotFound)
	}
	return a, nil
}

// SetFindingStatus implements web.FindingsStore.
func (s *Store) SetFindingStatus(_ context.Context, id uuid.UUID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setStatus(id, status)
}

func (s *Store) setStatus(id uuid.UUID, status string) error {
	f, ok := s.findings[id]
	if !ok {
		return fmt.Errorf("%w: finding", store.ErrNotFound)
	}
	f.Status = status
	s.findings[id] = f
	return nil
}

// InsertAuditLog implements web.FindingsStore.
func (s *Store) InsertAuditLog(_ context.Context, e store.AuditLogEntry) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addAudit(e), nil
}

func (s *Store) addAudit(e store.AuditLogEntry) int64 {
	s.nextAudit++
	e.ID = s.nextAudit
	if e.At.IsZero() {
		e.At = s.tick()
	}
	s.audits = append(s.audits, e)
	return e.ID
}

// GetProposalByFinding implements web.FindingsStore: the live proposal is any
// that is not rejected.
func (s *Store) GetProposalByFinding(_ context.Context, id uuid.UUID) (store.JournalProposal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.proposals {
		if p.FindingID != nil && *p.FindingID == id && p.Status != "rejected" {
			return p, true, nil
		}
	}
	return store.JournalProposal{}, false, nil
}

// ListAuditPage implements web.AuditStore.
func (s *Store) ListAuditPage(_ context.Context, f store.AuditFilter) ([]store.AuditLogEntry, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	var all []store.AuditLogEntry
	for i := len(s.audits) - 1; i >= 0; i-- {
		e := s.audits[i]
		switch {
		case f.Before != 0 && e.ID >= f.Before:
		case f.Actor != "" && e.Actor != f.Actor:
		case f.Server != "" && (e.Server == nil || *e.Server != f.Server):
		case f.Tool != "" && (e.Tool == nil || *e.Tool != f.Tool):
		case f.From != nil && e.At.Before(*f.From):
		case f.To != nil && !e.At.Before(*f.To):
		default:
			all = append(all, e)
		}
	}
	if len(all) <= limit {
		return all, 0, nil
	}
	return all[:limit], all[limit-1].ID, nil
}

// ListAuditFacets implements web.AuditStore.
func (s *Store) ListAuditFacets(context.Context) (actors, servers, tools []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.audits {
		actors = append(actors, e.Actor)
		if e.Server != nil {
			servers = append(servers, *e.Server)
		}
		if e.Tool != nil {
			tools = append(tools, *e.Tool)
		}
	}
	for _, l := range []*[]string{&actors, &servers, &tools} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	return actors, servers, tools, nil
}

// RunMetricsByStep implements web.AuditStore.
func (s *Store) RunMetricsByStep(_ context.Context, runID uuid.UUID) ([]store.StepUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[runID]; !ok {
		return nil, nil
	}
	return []store.StepUsage{
		{Kind: "explain", InputTokens: 4200, OutputTokens: 900, CacheReadTokens: 1200, CostUSD: "0.0310", Calls: 3, LatencyMS: 5400},
		{Kind: "investigate", InputTokens: 0, OutputTokens: 0, CostUSD: "0", Calls: 0, LatencyMS: 0},
		{Kind: "verify", InputTokens: 1800, OutputTokens: 300, CostUSD: "0.0110", Calls: 3, LatencyMS: 2100},
		{Kind: "other", InputTokens: 0, OutputTokens: 0, CostUSD: "0", Calls: 0, LatencyMS: 0},
	}, nil
}

// RunsSummaryStats implements web.AuditStore: the newest finished run.
func (s *Store) RunsSummaryStats(context.Context) (store.LastRunStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *store.CloseRun
	for _, r := range s.runs {
		if (r.Status != store.RunDone && r.Status != store.RunPartial) || r.StartedAt == nil {
			continue
		}
		if best == nil || r.StartedAt.After(*best.StartedAt) {
			r := r
			best = &r
		}
	}
	if best == nil {
		return store.LastRunStats{}, nil
	}
	out := store.LastRunStats{Found: true, RunID: best.ID, CompanyID: best.CompanyID, Month: best.Month}
	for _, f := range s.findingsOf(best.ID) {
		out.Total++
		if f.Verified {
			out.Verified++
		}
		if f.Status == "open" && f.Severity == "high" {
			out.OpenHigh++
		}
	}
	return out, nil
}

// canonical is how maker and checker names are compared.
func canonical(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
