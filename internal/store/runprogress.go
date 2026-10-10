package store

// Run progress (CC-703). The close workflow records its run's state and
// usage here. Token and cost rollups happen in SQL, so the agent, which
// may not hold floating point (nofloat), never adds up money: costs stay
// numeric in Postgres and leave it only as decimal text.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// Close run statuses (close_runs.status). The workflow's finer states
// (preflight, checking, ...) are its run_steps.
const (
	RunQueued  = "queued"
	RunRunning = "running"
	RunDone    = "done"
	RunPartial = "partial"
	RunFailed  = "failed"
)

// ValidateRunStatus reports whether status is a close run status.
func ValidateRunStatus(status string) error {
	switch status {
	case RunQueued, RunRunning, RunDone, RunPartial, RunFailed:
		return nil
	}
	return fmt.Errorf("store: invalid run status %.40q", status)
}

// maxRunErrorLen caps the error text stored on a run.
const maxRunErrorLen = 2000

// SetRunState sets a close run's status and error text ("" clears it).
// Moving to running sets started_at if it was never set and clears
// finished_at, so a resumed run reads as in progress; a terminal status
// (done, partial, failed) sets finished_at.
func (s *Store) SetRunState(ctx context.Context, runID uuid.UUID, status, errText string) error {
	if err := ValidateRunStatus(status); err != nil {
		return err
	}
	var errVal *string
	if errText != "" {
		if len(errText) > maxRunErrorLen {
			errText = errText[:maxRunErrorLen]
		}
		errVal = &errText
	}
	const query = `
		UPDATE close_runs SET
			status = $2,
			error = $3,
			started_at = CASE WHEN $2 = 'running' THEN coalesce(started_at, now()) ELSE started_at END,
			finished_at = CASE
				WHEN $2 IN ('done', 'partial', 'failed') THEN now()
				WHEN $2 = 'running' THEN NULL
				ELSE finished_at
			END
		WHERE id = $1;
	`
	tag, err := s.pool.Exec(ctx, query, runID, status, errVal)
	if err != nil {
		return fmt.Errorf("store: set run %s state: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: close run %s", ErrNotFound, runID)
	}
	return nil
}

// RunUsage is a run's model usage: token counts and the cost as exact
// decimal text (such as "0.0123"), never a float.
type RunUsage struct {
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	CostUSD         string `json:"cost_usd"`
}

// RollupRunUsage sums the run's llm_calls (through run_steps) into the
// close_runs row's token and cost columns, and returns the totals. The
// cost is summed and rounded to close_runs' four decimals in SQL.
func (s *Store) RollupRunUsage(ctx context.Context, runID uuid.UUID) (RunUsage, error) {
	const query = `
		WITH u AS (
			SELECT coalesce(sum(c.input_tokens), 0)::bigint AS input_tokens,
			       coalesce(sum(c.output_tokens), 0)::bigint AS output_tokens,
			       coalesce(sum(c.cache_read_tokens), 0)::bigint AS cache_read_tokens,
			       round(coalesce(sum(c.cost_usd), 0), 4) AS cost_usd
			FROM llm_calls c JOIN run_steps s ON s.id = c.step_id
			WHERE s.run_id = $1
		)
		UPDATE close_runs r SET
			input_tokens = u.input_tokens,
			output_tokens = u.output_tokens,
			cache_read_tokens = u.cache_read_tokens,
			cost_usd = u.cost_usd
		FROM u
		WHERE r.id = $1
		RETURNING u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cost_usd::text;
	`
	var u RunUsage
	err := s.pool.QueryRow(ctx, query, runID).Scan(&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CostUSD)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunUsage{}, fmt.Errorf("%w: close run %s", ErrNotFound, runID)
		}
		return RunUsage{}, fmt.Errorf("store: roll up run %s usage: %w", runID, err)
	}
	cost, err := canonicalNumber(u.CostUSD)
	if err != nil {
		return RunUsage{}, fmt.Errorf("store: roll up run %s usage: cost: %w", runID, err)
	}
	u.CostUSD = cost
	return u, nil
}

// RunTokensUsed returns the model tokens (input plus output) recorded in
// the run's llm_calls so far, for the per-run token cap.
func (s *Store) RunTokensUsed(ctx context.Context, runID uuid.UUID) (int64, error) {
	const query = `
		SELECT coalesce(sum(coalesce(c.input_tokens, 0) + coalesce(c.output_tokens, 0)), 0)::bigint
		FROM llm_calls c JOIN run_steps s ON s.id = c.step_id
		WHERE s.run_id = $1;
	`
	var n int64
	if err := s.pool.QueryRow(ctx, query, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: run %s tokens: %w", runID, err)
	}
	return n, nil
}

// LLMBudgetExhausted reports whether the model cost recorded on the UTC
// day of now has reached cfg.LLMDailyBudgetUSD. The comparison is done in
// SQL on numeric values; the budget is passed as decimal text.
func (s *Store) LLMBudgetExhausted(ctx context.Context, cfg config.Config, now time.Time) (bool, error) {
	b := cfg.LLMDailyBudgetUSD
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
		return false, fmt.Errorf("store: daily LLM budget %v is not a non-negative number", b)
	}
	budget := strconv.FormatFloat(b, 'f', -1, 64)
	day := now.UTC().Truncate(24 * time.Hour)
	const query = `
		SELECT coalesce(sum(cost_usd), 0) >= $1::numeric
		FROM llm_calls
		WHERE created_at >= $2 AND created_at < $3;
	`
	var exhausted bool
	if err := s.pool.QueryRow(ctx, query, budget, day, day.Add(24*time.Hour)).Scan(&exhausted); err != nil {
		return false, fmt.Errorf("store: daily LLM budget: %w", err)
	}
	return exhausted, nil
}

// SetFindingStatus sets a finding's review status (open, needs_review,
// accepted or dismissed). The workflow marks a finding needs_review when
// its explanation fails verification.
func (s *Store) SetFindingStatus(ctx context.Context, findingID uuid.UUID, status string) error {
	switch status {
	case "open", "needs_review", "accepted", "dismissed":
	default:
		return fmt.Errorf("store: invalid finding status %.40q", status)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE findings SET status = $2 WHERE id = $1;`, findingID, status)
	if err != nil {
		return fmt.Errorf("store: set finding %s status: %w", findingID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s", ErrNotFound, findingID)
	}
	return nil
}

// MaxCallCostUSD bounds one model call's cost: llm_calls.cost_usd is
// numeric(10,6), so 10000 or more cannot be stored.
const MaxCallCostUSD = 10000

// ValidateCallCost checks a call's cost as decimal text before it is
// inserted: empty (unknown) is allowed; otherwise it must be a plain
// decimal number, not negative, and below MaxCallCostUSD.
func ValidateCallCost(cost string) error {
	if cost == "" {
		return nil
	}
	n, err := canonicalNumber(cost)
	if err != nil {
		return fmt.Errorf("store: call cost %.40q: %w", cost, err)
	}
	if n[0] == '-' {
		return fmt.Errorf("store: call cost %s is negative", n)
	}
	// The canonical form is a plain decimal with no leading zeros, so the
	// integer part's length gives its magnitude.
	intPart, _, _ := strings.Cut(n, ".")
	if len(intPart) >= len(strconv.Itoa(MaxCallCostUSD)) {
		return fmt.Errorf("store: call cost %s is not below %d", n, MaxCallCostUSD)
	}
	return nil
}
