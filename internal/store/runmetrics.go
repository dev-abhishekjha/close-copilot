package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Step families of RunMetricsByStep, in display order.
const (
	stepFamilyOther = "other"
)

var stepFamilies = []string{StepKindExplain, StepKindInvestigate, StepKindVerify, stepFamilyOther}

// StepUsage is the model usage of one step family of a run. CostUSD is
// exact decimal text.
type StepUsage struct {
	Kind            string `json:"kind"` // explain | investigate | verify | other
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	CostUSD         string `json:"cost_usd"`
	Calls           int64  `json:"calls"`
	LatencyMS       int64  `json:"latency_ms"`
}

// stepFamily maps a run_steps.kind to its family.
func stepFamily(kind string) string {
	switch kind {
	case StepKindExplain, StepKindInvestigate, StepKindVerify:
		return kind
	}
	return stepFamilyOther
}

// sortStepUsage orders rows explain, investigate, verify, other.
func sortStepUsage(rows []StepUsage) []StepUsage {
	var out []StepUsage
	for _, fam := range stepFamilies {
		for _, r := range rows {
			if r.Kind == fam {
				out = append(out, r)
			}
		}
	}
	return out
}

// RunMetricsByStep sums the run's llm_calls per step family. Only
// families with at least one call appear.
func (s *Store) RunMetricsByStep(ctx context.Context, runID uuid.UUID) ([]StepUsage, error) {
	const query = `
		SELECT CASE WHEN s.kind IN ('explain', 'investigate', 'verify') THEN s.kind ELSE 'other' END AS fam,
		       coalesce(sum(c.input_tokens), 0)::bigint,
		       coalesce(sum(c.output_tokens), 0)::bigint,
		       coalesce(sum(c.cache_read_tokens), 0)::bigint,
		       round(coalesce(sum(c.cost_usd), 0), 4)::text,
		       count(*),
		       coalesce(sum(c.latency_ms), 0)::bigint
		FROM llm_calls c JOIN run_steps s ON s.id = c.step_id
		WHERE s.run_id = $1
		GROUP BY fam;
	`
	rows, err := s.pool.Query(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("store: run %s metrics: %w", runID, err)
	}
	defer rows.Close()
	var out []StepUsage
	for rows.Next() {
		var u StepUsage
		if err := rows.Scan(&u.Kind, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CostUSD, &u.Calls, &u.LatencyMS); err != nil {
			return nil, fmt.Errorf("store: scan run %s metrics: %w", runID, err)
		}
		if u.CostUSD, err = canonicalNumber(u.CostUSD); err != nil {
			return nil, fmt.Errorf("store: run %s metrics: cost: %w", runID, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate run %s metrics: %w", runID, err)
	}
	return sortStepUsage(out), nil
}

// LastRunStats describes the newest finished run for the runs page.
// Found is false when no run is done or partial.
type LastRunStats struct {
	Found     bool
	RunID     uuid.UUID
	CompanyID string
	Month     string
	Total     int // findings
	Verified  int
	OpenHigh  int // status open and severity high
}

// RunsSummaryStats returns the stats of the newest (by started_at) run
// that is done or partial.
func (s *Store) RunsSummaryStats(ctx context.Context) (LastRunStats, error) {
	const query = `
		SELECT r.id, r.company_id, r.month,
		       count(f.id)::int,
		       (count(f.id) FILTER (WHERE f.verified))::int,
		       (count(f.id) FILTER (WHERE f.status = 'open' AND f.severity = 'high'))::int
		FROM (
			SELECT id, company_id, month FROM close_runs
			WHERE status IN ('done', 'partial')
			ORDER BY started_at DESC NULLS LAST, id ASC
			LIMIT 1
		) r
		LEFT JOIN findings f ON f.run_id = r.id
		GROUP BY r.id, r.company_id, r.month;
	`
	st := LastRunStats{Found: true}
	err := s.pool.QueryRow(ctx, query).Scan(&st.RunID, &st.CompanyID, &st.Month, &st.Total, &st.Verified, &st.OpenHigh)
	if errors.Is(err, pgx.ErrNoRows) {
		return LastRunStats{}, nil
	}
	if err != nil {
		return LastRunStats{}, fmt.Errorf("store: runs summary stats: %w", err)
	}
	return st, nil
}
