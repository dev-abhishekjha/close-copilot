package store

// Content-addressed artifacts and LLM call records (CC-709). An artifact is
// stored once per canonical value; GetArtifact re-hashes what it reads, so
// tampering or drift in the jsonb column is an error, never silent.
//
// Nothing here logs artifact content: prompts, responses and tool results
// can carry ledger, bank and document text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Artifact kinds from the shared spec.
const (
	ArtifactToolResult  = "tool_result"
	ArtifactRetrieval   = "retrieval"
	ArtifactPrompt      = "prompt"
	ArtifactResponse    = "response"
	ArtifactExplanation = "explanation"
	ArtifactReport      = "report"
	// ArtifactVerdict is the verifier's verdict on one explanation
	// (CC-705). It extends the shared spec's six kinds; the column has no
	// CHECK constraint, so no migration is needed.
	ArtifactVerdict = "verdict"
)

// ErrArtifactMismatch reports a stored artifact whose content no longer
// hashes to its address.
var ErrArtifactMismatch = errors.New("store: artifact content does not match its sha256")

// Artifact is one stored, hash-addressed value.
type Artifact struct {
	SHA256     string          `json:"sha256"`
	Kind       string          `json:"kind"`
	RunID      *uuid.UUID      `json:"run_id,omitempty"`
	ProducedBy *uuid.UUID      `json:"produced_by,omitempty"`
	Content    json.RawMessage `json:"content"` // canonical JSON
	CreatedAt  time.Time       `json:"created_at"`
}

// ErrStepRunMismatch reports an LLM call recorded against a step that does
// not belong to the given run.
var ErrStepRunMismatch = errors.New("store: step does not belong to the run")

// LLMCall is one model call: its prompt and response artifacts plus usage.
// The cost is an exact decimal string (such as "0.000123"), never a float;
// "" stores NULL. The store does not depend on internal/llm, so the MCP
// servers that use it never link an LLM client.
type LLMCall struct {
	ID              uuid.UUID `json:"id"`
	RunID           uuid.UUID `json:"run_id"`
	StepID          uuid.UUID `json:"step_id"`
	Model           string    `json:"model"`
	PromptSHA256    string    `json:"prompt_sha256"`
	ResponseSHA256  string    `json:"response_sha256"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	CacheReadTokens int64     `json:"cache_read_tokens"`
	CostUSD         string    `json:"cost_usd"`
	LatencyMS       int64     `json:"latency_ms"`
	CreatedAt       time.Time `json:"created_at"`
}

// ValidateArtifactKind reports whether kind is one of the shared spec's
// artifact kinds or a verdict.
func ValidateArtifactKind(kind string) error {
	switch kind {
	case ArtifactToolResult, ArtifactRetrieval, ArtifactPrompt, ArtifactResponse, ArtifactExplanation, ArtifactReport, ArtifactVerdict:
		return nil
	}
	return fmt.Errorf("store: invalid artifact kind %.40q", kind)
}

// nullUUID maps uuid.Nil to SQL NULL.
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// PutArtifact stores v as a canonical JSON artifact and returns its
// address, the lowercase hex sha256 of the canonical bytes. Storing the
// same value again is a no-op that returns the same address; the first
// row's kind, run and step are kept. A zero runID or stepID is stored as
// NULL.
func (s *Store) PutArtifact(ctx context.Context, kind string, runID, stepID uuid.UUID, v any) (string, error) {
	if err := ValidateArtifactKind(kind); err != nil {
		return "", err
	}
	b, sha, err := CanonicalHash(v)
	if err != nil {
		return "", fmt.Errorf("store: put %s artifact: %w", kind, err)
	}
	const query = `
		INSERT INTO artifacts (sha256, kind, run_id, produced_by, content)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (sha256) DO NOTHING;
	`
	if _, err := s.pool.Exec(ctx, query, sha, kind, nullUUID(runID), nullUUID(stepID), string(b)); err != nil {
		return "", fmt.Errorf("store: put %s artifact %s: %w", kind, sha, err)
	}
	return sha, nil
}

// GetArtifact reads the artifact at sha, re-canonicalises its content and
// checks that it hashes back to sha. A mismatch wraps ErrArtifactMismatch;
// a missing artifact wraps ErrNotFound.
func (s *Store) GetArtifact(ctx context.Context, sha string) (Artifact, error) {
	const query = `
		SELECT sha256, kind, run_id, produced_by, content::text, created_at
		FROM artifacts
		WHERE sha256 = $1;
	`
	var a Artifact
	var content string
	err := s.pool.QueryRow(ctx, query, sha).Scan(&a.SHA256, &a.Kind, &a.RunID, &a.ProducedBy, &content, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Artifact{}, fmt.Errorf("%w: artifact %.70q", ErrNotFound, sha)
		}
		return Artifact{}, fmt.Errorf("store: get artifact: %w", err)
	}
	b, got, err := CanonicalHash(json.RawMessage(content))
	if err != nil {
		return Artifact{}, fmt.Errorf("store: get artifact %s: %w", sha, err)
	}
	if got != sha {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactMismatch, sha)
	}
	a.Content = b
	return a, nil
}

// InsertLLMCall records one model call. Both artifacts must already exist
// (foreign keys), and the step must belong to c.RunID, else the error wraps
// ErrStepRunMismatch. The cost is inserted as numeric from its decimal
// text, so no float is involved. A zero ID is filled with a new UUID, which
// is returned.
func (s *Store) InsertLLMCall(ctx context.Context, c LLMCall) (uuid.UUID, error) {
	if c.StepID == uuid.Nil || c.RunID == uuid.Nil {
		return uuid.Nil, errors.New("store: insert llm call: empty run or step id")
	}
	if c.PromptSHA256 == "" || c.ResponseSHA256 == "" {
		return uuid.Nil, errors.New("store: insert llm call: prompt and response artifacts are required")
	}
	var cost *string
	if c.CostUSD != "" {
		n, err := canonicalNumber(c.CostUSD)
		if err != nil {
			return uuid.Nil, fmt.Errorf("store: insert llm call: cost: %w", err)
		}
		cost = &n
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	const query = `
		INSERT INTO llm_calls (
			id, step_id, model, prompt_sha256, response_sha256,
			input_tokens, output_tokens, cache_read_tokens, cost_usd, latency_ms
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10
		WHERE EXISTS (SELECT 1 FROM run_steps WHERE id = $2 AND run_id = $11);
	`
	tag, err := s.pool.Exec(ctx, query,
		c.ID,
		c.StepID,
		c.Model,
		c.PromptSHA256,
		c.ResponseSHA256,
		c.InputTokens,
		c.OutputTokens,
		c.CacheReadTokens,
		cost,
		c.LatencyMS,
		c.RunID,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: insert llm call for step %s: %w", c.StepID, err)
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, fmt.Errorf("%w: step %s, run %s", ErrStepRunMismatch, c.StepID, c.RunID)
	}
	return c.ID, nil
}

// ListLLMCalls returns the model calls recorded for a step, oldest first.
func (s *Store) ListLLMCalls(ctx context.Context, stepID uuid.UUID) ([]LLMCall, error) {
	const query = `
		SELECT c.id, s.run_id, c.step_id, c.model, c.prompt_sha256, c.response_sha256,
		       coalesce(c.input_tokens, 0), coalesce(c.output_tokens, 0), coalesce(c.cache_read_tokens, 0),
		       coalesce(c.cost_usd::text, ''), coalesce(c.latency_ms, 0), c.created_at
		FROM llm_calls c JOIN run_steps s ON s.id = c.step_id
		WHERE c.step_id = $1
		ORDER BY c.created_at, c.id;
	`
	rows, err := s.pool.Query(ctx, query, stepID)
	if err != nil {
		return nil, fmt.Errorf("store: list llm calls: %w", err)
	}
	defer rows.Close()
	var out []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(
			&c.ID, &c.RunID, &c.StepID, &c.Model, &c.PromptSHA256, &c.ResponseSHA256,
			&c.InputTokens, &c.OutputTokens, &c.CacheReadTokens,
			&c.CostUSD, &c.LatencyMS, &c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan llm call: %w", err)
		}
		if c.CostUSD != "" {
			// numeric(10,6) pads the scale ("0.000123" stays, "0.5"
			// reads back "0.500000"); return the shortest exact form.
			n, err := canonicalNumber(c.CostUSD)
			if err != nil {
				return nil, fmt.Errorf("store: scan llm call cost: %w", err)
			}
			c.CostUSD = n
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate llm calls: %w", err)
	}
	return out, nil
}
