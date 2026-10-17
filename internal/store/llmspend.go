package store

import (
	"context"
	"fmt"
	"time"
)

// LLMSpendToday returns the model cost recorded on the UTC day of now as a
// decimal string (for example "1.234500"; "0" for an empty day). It uses
// the same window as LLMBudgetExhausted and only reads.
func (s *Store) LLMSpendToday(ctx context.Context, now time.Time) (string, error) {
	day := now.UTC().Truncate(24 * time.Hour)
	const query = `
		SELECT coalesce(sum(cost_usd), 0)::text
		FROM llm_calls
		WHERE created_at >= $1 AND created_at < $2;
	`
	var spend string
	if err := s.pool.QueryRow(ctx, query, day, day.Add(24*time.Hour)).Scan(&spend); err != nil {
		return "", fmt.Errorf("store: LLM spend today: %w", err)
	}
	return spend, nil
}
