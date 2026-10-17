package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Bounds for RecentRunTrend.
const (
	minRunTrend = 1
	maxRunTrend = 50
)

// RunTrendPoint is one finished run's cost, tokens and verified counts for
// the metrics trend charts.
type RunTrendPoint struct {
	RunID        uuid.UUID
	CompanyID    string
	Month        string
	StartedAt    time.Time
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
	Findings     int
	Verified     int
}

// clampRunTrendLimit keeps limit within 1..50.
func clampRunTrendLimit(n int) int { return min(max(n, minRunTrend), maxRunTrend) }

// RecentRunTrend returns the newest done or partial runs of any company,
// newest started_at first, at most limit rows (clamped to 1..50).
func (s *Store) RecentRunTrend(ctx context.Context, limit int) ([]RunTrendPoint, error) {
	const query = `
		SELECT r.id, r.company_id, r.month, r.started_at, coalesce(r.cost_usd, 0)::float8,
		       coalesce(r.input_tokens, 0), coalesce(r.output_tokens, 0),
		       count(f.id)::int,
		       (count(f.id) FILTER (WHERE f.verified))::int
		FROM (
			SELECT id, company_id, month, started_at, cost_usd, input_tokens, output_tokens
			FROM close_runs
			WHERE status IN ('done', 'partial') AND started_at IS NOT NULL
			ORDER BY started_at DESC, id ASC
			LIMIT $1
		) r
		LEFT JOIN findings f ON f.run_id = r.id
		GROUP BY r.id, r.company_id, r.month, r.started_at, r.cost_usd, r.input_tokens, r.output_tokens
		ORDER BY r.started_at DESC, r.id ASC;
	`
	rows, err := s.pool.Query(ctx, query, clampRunTrendLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: recent run trend: %w", err)
	}
	defer rows.Close()
	var out []RunTrendPoint
	for rows.Next() {
		var p RunTrendPoint
		if err := rows.Scan(&p.RunID, &p.CompanyID, &p.Month, &p.StartedAt, &p.CostUSD,
			&p.InputTokens, &p.OutputTokens, &p.Findings, &p.Verified); err != nil {
			return nil, fmt.Errorf("store: scan run trend: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate run trend: %w", err)
	}
	return out, nil
}
