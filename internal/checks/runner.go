package checks

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// Check is the interface implemented by each deterministic check worker.
type Check interface {
	Name() string
	Run(ctx context.Context, in Inputs) ([]Finding, error)
}

// Rules aliases company.Rules (config/rules.yaml) for check inputs.
type Rules = company.Rules

// Inputs holds the company, period and readers passed to each Check.
type Inputs struct {
	Company  string // company ID, e.g. "sharma"
	Month    string // YYYY-MM
	Books    BooksReader
	Evidence EvidenceReader
	Rules    Rules
	// BankAccounts are the ERPNext names of the company's bank accounts
	// (e.g. "HDFC Current 0001 - STPL"). The caller fills it from the
	// company profile with BankAccountsFor.
	BankAccounts []string
}

// BankAccountsFor returns the ERPNext names of the bank accounts named in
// the company profile (bank.account), for Inputs.BankAccounts.
func BankAccountsFor(p company.Profile) []string {
	return []string{company.ERPAccount(p.Bank.Account, p.Abbr)}
}

// FindingsStore is the persistence interface used by Runner to save findings.
// *store.Store implements FindingsStore.
type FindingsStore interface {
	CreateFindings(ctx context.Context, findings []Finding) error
}

// Runner executes checks concurrently and persists findings in one transaction.
type Runner struct {
	store FindingsStore
	limit int
}

// NewRunner creates a Runner with the default concurrency limit of 4.
func NewRunner(st FindingsStore) *Runner {
	return &Runner{
		store: st,
		limit: 4,
	}
}

// SetLimit overrides the concurrency limit of Runner (for tests or tuning).
func (r *Runner) SetLimit(limit int) {
	if limit > 0 {
		r.limit = limit
	}
}

// Run executes the given checks concurrently with errgroup (limit 4),
// associates all findings with runID, dedupes findings by type plus keys,
// and persists the deduped findings in a single transaction.
func (r *Runner) Run(ctx context.Context, runID uuid.UUID, in Inputs, checks ...Check) ([]Finding, error) {
	if len(checks) == 0 {
		return nil, nil
	}

	limit := r.limit
	if limit <= 0 {
		limit = 4
	}

	// Checks run under gctx, which errgroup cancels when Wait returns.
	// Persistence below must use the caller's ctx, not gctx.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	results := make([][]Finding, len(checks))

	for i, c := range checks {
		g.Go(func() error {
			findings, err := c.Run(gctx, in)
			if err != nil {
				return fmt.Errorf("check %s: %w", c.Name(), err)
			}
			results[i] = findings
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Flatten findings and assign runID, UUID, default status.
	var all []Finding
	for _, res := range results {
		for _, f := range res {
			f.RunID = runID
			if f.ID == uuid.Nil {
				f.ID = uuid.New()
			}
			if f.Status == "" {
				f.Status = StatusOpen
			}
			all = append(all, f)
		}
	}

	// Deduplicate findings by type plus keys.
	deduped := DedupeFindings(all)

	// Persist findings in a single transaction if a store is provided.
	if r.store != nil && len(deduped) > 0 {
		if err := r.store.CreateFindings(ctx, deduped); err != nil {
			return nil, fmt.Errorf("checks: persist findings for run %s: %w", runID, err)
		}
	}

	return deduped, nil
}

// DedupeFindings collapses findings that share identical DedupeKey.
// Evidence from duplicate findings is merged into the preserved finding.
func DedupeFindings(findings []Finding) []Finding {
	if len(findings) <= 1 {
		return findings
	}

	seen := make(map[string]int, len(findings))
	out := make([]Finding, 0, len(findings))

	for _, f := range findings {
		key := DedupeKey(f)
		if idx, exists := seen[key]; exists {
			out[idx].Evidence = mergeEvidence(out[idx].Evidence, f.Evidence)
			continue
		}
		seen[key] = len(out)
		out = append(out, f)
	}

	return out
}

func mergeEvidence(existing, extra []EvidenceRef) []EvidenceRef {
	if len(extra) == 0 {
		return existing
	}
	out := slices.Clone(existing)
	for _, e := range extra {
		if !containsEvidence(out, e) {
			out = append(out, e)
		}
	}
	return out
}

func containsEvidence(list []EvidenceRef, ref EvidenceRef) bool {
	for _, item := range list {
		if item.Server == ref.Server && item.Tool == ref.Tool && slices.Equal(item.IDs, ref.IDs) {
			return true
		}
	}
	return false
}
