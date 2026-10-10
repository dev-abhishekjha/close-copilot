package store

// Workflow steps (CC-709). Each step is one row in run_steps, unique per
// (run_id, kind, subject). Begin starts or restarts a step unless it is
// already done, so a resumed run re-runs exactly the steps that never
// finished; FinishWithFindings writes a step's findings in the same
// transaction that marks it done, so a resume can't duplicate them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Step statuses from the shared spec.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Step kinds from the shared spec. A check step's kind is
// StepKindCheckPrefix plus the check's name, such as "check.bankrec".
const (
	StepKindRouter      = "router"
	StepKindCheckPrefix = "check."
	StepKindRetrieve    = "retrieve"
	StepKindExplain     = "explain"
	StepKindVerify      = "verify"
	StepKindInvestigate = "investigate"
	StepKindSynthesize  = "synthesize"
)

// ErrStepDone reports an attempt to finish a step that is already done.
var ErrStepDone = errors.New("store: step already done")

// ErrStaleAttempt reports a finish from an attempt that is no longer the
// step's live one: a newer attempt has begun, or this one already ended.
var ErrStaleAttempt = errors.New("store: stale step attempt")

// ErrMissingArtifact reports an evidence or output ref that names no
// stored artifact.
var ErrMissingArtifact = errors.New("store: ref names no stored artifact")

// checkNameRe is the shape of the <name> in a check.<name> kind.
var checkNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Step is one row of run_steps.
type Step struct {
	ID         uuid.UUID       `json:"id"`
	RunID      uuid.UUID       `json:"run_id"`
	Kind       string          `json:"kind"`
	Subject    string          `json:"subject"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt"`
	InputRefs  []string        `json:"input_refs"`
	OutputRefs []string        `json:"output_refs"`
	Feedback   json.RawMessage `json:"feedback,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Error      *string         `json:"error,omitempty"`
}

// ValidateStepKind reports whether kind is one of the shared spec's step
// kinds: router, check.<name>, retrieve, explain, verify, investigate or
// synthesize.
func ValidateStepKind(kind string) error {
	switch kind {
	case StepKindRouter, StepKindRetrieve, StepKindExplain, StepKindVerify, StepKindInvestigate, StepKindSynthesize:
		return nil
	}
	if name, ok := strings.CutPrefix(kind, StepKindCheckPrefix); ok && checkNameRe.MatchString(name) {
		return nil
	}
	return fmt.Errorf("store: invalid step kind %.40q", kind)
}

// ValidateStepStatus reports whether status is one of the shared spec's
// step statuses.
func ValidateStepStatus(status string) error {
	switch status {
	case StepPending, StepRunning, StepDone, StepFailed, StepSkipped:
		return nil
	}
	return fmt.Errorf("store: invalid step status %.40q", status)
}

const stepColumns = `id, run_id, kind, subject, status, attempt, input_refs, output_refs,
	feedback, started_at, finished_at, error`

func scanStep(row pgx.Row) (Step, error) {
	var st Step
	var feedback []byte
	if err := row.Scan(
		&st.ID, &st.RunID, &st.Kind, &st.Subject, &st.Status, &st.Attempt,
		&st.InputRefs, &st.OutputRefs, &feedback, &st.StartedAt, &st.FinishedAt, &st.Error,
	); err != nil {
		return Step{}, err
	}
	if len(feedback) > 0 {
		st.Feedback = feedback
	}
	return st, nil
}

// Begin starts the step (runID, kind, subject), or restarts it with attempt
// incremented if it exists and is not done. When the step is already done
// it returns the stored step and done=true, and the caller skips it.
//
// A restart moves the step's output refs to the end of its input refs and
// clears output_refs, as ReopenStep does: what an earlier attempt produced
// (such as a failed verify attempt's verdict) stays reachable from
// run_steps. So input_refs holds the step's inputs and then, in order, the
// outputs of its earlier attempts.
//
// Two concurrent Begin calls on the same key leave exactly one row, in
// status running; the later call sees the higher attempt.
func (s *Store) Begin(ctx context.Context, runID uuid.UUID, kind, subject string) (Step, bool, error) {
	if runID == uuid.Nil {
		return Step{}, false, errors.New("store: begin step: empty run id")
	}
	if err := ValidateStepKind(kind); err != nil {
		return Step{}, false, err
	}
	if subject == "" {
		return Step{}, false, errors.New("store: begin step: empty subject")
	}
	const query = `
		INSERT INTO run_steps (id, run_id, kind, subject, status, attempt, started_at)
		VALUES ($1, $2, $3, $4, 'running', 1, now())
		ON CONFLICT (run_id, kind, subject) DO UPDATE
		  SET status = 'running', attempt = run_steps.attempt + 1, started_at = now(),
		      input_refs = run_steps.input_refs || run_steps.output_refs, output_refs = '{}',
		      finished_at = NULL, error = NULL
		  WHERE run_steps.status <> 'done'
		RETURNING ` + stepColumns + `;`
	st, err := scanStep(s.pool.QueryRow(ctx, query, uuid.New(), runID, kind, subject))
	if err == nil {
		return st, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Step{}, false, fmt.Errorf("store: begin step %s/%.40q: %w", kind, subject, err)
	}
	// No row back: the step exists and is done.
	const done = `SELECT ` + stepColumns + ` FROM run_steps WHERE run_id = $1 AND kind = $2 AND subject = $3;`
	st, err = scanStep(s.pool.QueryRow(ctx, done, runID, kind, subject))
	if err != nil {
		return Step{}, false, fmt.Errorf("store: begin step %s/%.40q: read done step: %w", kind, subject, err)
	}
	return st, true, nil
}

// Finish ends the given attempt of a step with a terminal status (done,
// failed or skipped), its output artifact refs and, for a failure, the
// error text ("" for none). It sets finished_at.
//
// Only the live attempt may finish a step: the row must still be running
// with step.Attempt. Finishing a done step returns ErrStepDone; finishing
// from an older attempt, or one already finished, returns ErrStaleAttempt.
// Every output ref must name a stored artifact, else ErrMissingArtifact. A
// step with findings is finished with FinishWithFindings.
func (s *Store) Finish(ctx context.Context, step Step, status string, outputRefs []string, errText string) error {
	if err := ValidateStepStatus(status); err != nil {
		return err
	}
	if status != StepDone && status != StepFailed && status != StepSkipped {
		return fmt.Errorf("store: finish step: status %q is not terminal", status)
	}
	if outputRefs == nil {
		outputRefs = []string{}
	}
	var errVal *string
	if errText != "" {
		errVal = &errText
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkArtifactsTx(ctx, tx, outputRefs); err != nil {
			return err
		}
		const query = `
			UPDATE run_steps
			SET status = $2, output_refs = $3, error = $4, finished_at = now()
			WHERE id = $1 AND attempt = $5 AND status = 'running';
		`
		tag, err := tx.Exec(ctx, query, step.ID, status, outputRefs, errVal, step.Attempt)
		if err != nil {
			return fmt.Errorf("store: finish step %s: %w", step.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return errNoRowMarked
		}
		return nil
	})
	if errors.Is(err, errNoRowMarked) {
		return s.notFinishable(ctx, step)
	}
	return err
}

// notFinishable explains why an UPDATE of a step attempt touched no row.
func (s *Store) notFinishable(ctx context.Context, step Step) error {
	st, err := s.GetStep(ctx, step.ID)
	if err != nil {
		return err
	}
	if st.Status == StepDone {
		return fmt.Errorf("%w: %s", ErrStepDone, step.ID)
	}
	return fmt.Errorf("%w: step %s attempt %d (current attempt %d, %s)", ErrStaleAttempt, step.ID, step.Attempt, st.Attempt, st.Status)
}

// FinishWithFindings inserts a step's findings and marks the attempt done
// in one transaction. If marking fails (the step is missing, done, or
// running a newer attempt), the findings are rolled back, so a resumed step
// never duplicates them.
//
// It enforces the CC-709 invariant that every finding can be rebuilt from
// stored artifacts: every finding needs an ID, the step's run ID, and at
// least one evidence ref; every ref's Artifact and every output ref must
// name a stored artifact, else the error wraps ErrMissingArtifact.
func (s *Store) FinishWithFindings(ctx context.Context, step Step, outputRefs []string, findings []Finding) error {
	if outputRefs == nil {
		outputRefs = []string{}
	}
	refs := slices.Clone(outputRefs)
	for _, f := range findings {
		if f.ID == uuid.Nil || f.RunID == uuid.Nil {
			return errors.New("store: finish step with findings: every finding needs an ID and a run ID")
		}
		if len(f.Evidence) == 0 {
			return fmt.Errorf("%w: finding %s has no evidence", ErrMissingArtifact, f.ID)
		}
		for i, ref := range f.Evidence {
			if ref.Artifact == "" {
				return fmt.Errorf("%w: finding %s evidence %d has no artifact", ErrMissingArtifact, f.ID, i)
			}
			refs = append(refs, ref.Artifact)
		}
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkArtifactsTx(ctx, tx, refs); err != nil {
			return err
		}
		if err := insertFindingsTx(ctx, tx, findings); err != nil {
			return err
		}
		const mark = `
			UPDATE run_steps
			SET status = 'done', output_refs = $2, error = NULL, finished_at = now()
			WHERE id = $1 AND attempt = $3 AND status = 'running'
			RETURNING run_id;
		`
		var runID uuid.UUID
		if err := tx.QueryRow(ctx, mark, step.ID, outputRefs, step.Attempt).Scan(&runID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoRowMarked
			}
			return fmt.Errorf("store: mark step %s done: %w", step.ID, err)
		}
		for _, f := range findings {
			if f.RunID != runID {
				return fmt.Errorf("store: finding %s belongs to run %s, not the step's run %s", f.ID, f.RunID, runID)
			}
		}
		return nil
	})
	if errors.Is(err, errNoRowMarked) {
		return s.notFinishable(ctx, step)
	}
	return err
}

// checkArtifactsTx fails with ErrMissingArtifact unless every sha names a
// stored artifact.
func checkArtifactsTx(ctx context.Context, tx pgx.Tx, shas []string) error {
	want := map[string]bool{}
	for _, sha := range shas {
		if sha == "" {
			return fmt.Errorf("%w: empty artifact ref", ErrMissingArtifact)
		}
		want[sha] = true
	}
	if len(want) == 0 {
		return nil
	}
	distinct := make([]string, 0, len(want))
	for sha := range want {
		distinct = append(distinct, sha)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT sha256) FROM artifacts WHERE sha256 = ANY($1)`, distinct).Scan(&n); err != nil {
		return fmt.Errorf("store: check artifacts: %w", err)
	}
	if n != len(distinct) {
		return fmt.Errorf("%w: %d of %d refs name no stored artifact", ErrMissingArtifact, len(distinct)-n, len(distinct))
	}
	return nil
}

// MaxFeedbackBytes caps the feedback ReopenStep stores.
const MaxFeedbackBytes = 16 << 10

// ReopenStep sets a done step back to pending, so the next Begin restarts
// it with attempt incremented, and stores feedback (a JSON value, such as
// the verifier's violations) in run_steps.feedback for that attempt. It
// moves the step's output refs to the end of its input refs (what the
// reopened attempt produced, and the feedback is about, stays reachable
// from run_steps; input_refs therefore also holds earlier attempts'
// outputs) and clears output_refs, finished_at and error.
//
// Only the attempt that finished the step may reopen it: the row must be
// done with step.Attempt, else the error wraps ErrStaleAttempt (or
// ErrNotFound when the step is missing). Feedback must be a JSON value of
// at most 16 KiB.
func (s *Store) ReopenStep(ctx context.Context, step Step, feedback json.RawMessage) error {
	if len(feedback) == 0 || !json.Valid(feedback) {
		return errors.New("store: reopen step: feedback must be a JSON value")
	}
	if len(feedback) > MaxFeedbackBytes {
		return fmt.Errorf("store: reopen step: feedback of %d bytes exceeds %d", len(feedback), MaxFeedbackBytes)
	}
	const query = `
		UPDATE run_steps
		SET status = 'pending', feedback = $2::jsonb, input_refs = input_refs || output_refs,
		    output_refs = '{}', finished_at = NULL, error = NULL
		WHERE id = $1 AND attempt = $3 AND status = 'done';
	`
	tag, err := s.pool.Exec(ctx, query, step.ID, string(feedback), step.Attempt)
	if err != nil {
		return fmt.Errorf("store: reopen step %s: %w", step.ID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	st, err := s.GetStep(ctx, step.ID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: reopen step %s attempt %d (current attempt %d, %s)", ErrStaleAttempt, step.ID, step.Attempt, st.Attempt, st.Status)
}

// errNoRowMarked is the internal signal that the step row was not updated.
var errNoRowMarked = errors.New("store: step not marked")

// GetStep returns the step with the given ID.
func (s *Store) GetStep(ctx context.Context, stepID uuid.UUID) (Step, error) {
	const query = `SELECT ` + stepColumns + ` FROM run_steps WHERE id = $1;`
	st, err := scanStep(s.pool.QueryRow(ctx, query, stepID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Step{}, fmt.Errorf("%w: step %s", ErrNotFound, stepID)
		}
		return Step{}, fmt.Errorf("store: get step %s: %w", stepID, err)
	}
	return st, nil
}

// ListSteps returns a run's steps ordered by kind and subject.
func (s *Store) ListSteps(ctx context.Context, runID uuid.UUID) ([]Step, error) {
	const query = `SELECT ` + stepColumns + ` FROM run_steps WHERE run_id = $1 ORDER BY kind, subject;`
	rows, err := s.pool.Query(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list steps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan step: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate steps: %w", err)
	}
	return out, nil
}

// insertFindingsTx inserts findings inside tx. It is the insert logic of
// CreateFindings (runs.go), taking the caller's transaction so a step's
// findings and its done mark commit together.
func insertFindingsTx(ctx context.Context, tx pgx.Tx, findings []Finding) error {
	const query = `
		INSERT INTO findings (
			id, run_id, type, severity, title, amount_paise,
			keys, evidence, explanation, action, citations, proposal,
			verified, status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);
	`
	for _, f := range findings {
		keysJSON, err := json.Marshal(f.Keys)
		if err != nil {
			return fmt.Errorf("store: marshal keys: %w", err)
		}
		evidenceJSON, err := json.Marshal(f.Evidence)
		if err != nil {
			return fmt.Errorf("store: marshal evidence: %w", err)
		}
		var citationsJSON []byte
		if f.Citations != nil {
			citationsJSON, err = json.Marshal(f.Citations)
			if err != nil {
				return fmt.Errorf("store: marshal citations: %w", err)
			}
		}
		var proposalJSON []byte
		if f.Proposal != nil {
			proposalJSON, err = json.Marshal(f.Proposal)
			if err != nil {
				return fmt.Errorf("store: marshal proposal: %w", err)
			}
		}
		var amt *int64
		if f.AmountPaise != nil {
			v := int64(*f.AmountPaise)
			amt = &v
		}
		status := f.Status
		if status == "" {
			status = "open"
		}
		if _, err := tx.Exec(ctx, query,
			f.ID, f.RunID, f.Type, f.Severity, f.Title, amt,
			keysJSON, evidenceJSON, f.Explanation, f.Action, citationsJSON, proposalJSON,
			f.Verified, status,
		); err != nil {
			return fmt.Errorf("store: insert finding %s: %w", f.ID, err)
		}
	}
	return nil
}
