package store

import (
	"context"
	"fmt"
)

// RunSummary is a close run with its findings count, for the runs list.
type RunSummary struct {
	CloseRun
	Findings int `json:"findings"`
}

// Bounds for ListCloseRuns' limit.
const (
	minRunList = 1
	maxRunList = 500
)

// ListCloseRuns returns the newest runs first (started_at descending,
// nulls first so queued runs lead, then id), at most limit rows. limit is
// clamped to 1..500.
func (s *Store) ListCloseRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	limit = min(max(limit, minRunList), maxRunList)
	const query = `
		SELECT r.id, r.company_id, r.month, r.status, r.started_at, r.finished_at,
		       r.error, r.input_tokens, r.output_tokens, r.cache_read_tokens,
		       r.cost_usd, r.trace_id,
		       (SELECT count(*) FROM findings f WHERE f.run_id = r.id) AS findings
		FROM close_runs r
		ORDER BY r.started_at DESC NULLS FIRST, r.id ASC
		LIMIT $1;
	`
	rows, err := s.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list close runs: %w", err)
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		if err := rows.Scan(
			&r.ID, &r.CompanyID, &r.Month, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.Error, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
			&r.CostUSD, &r.TraceID, &r.Findings,
		); err != nil {
			return nil, fmt.Errorf("store: scan close run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate close runs: %w", err)
	}
	return out, nil
}
