package store

// What the verifier and cmd/audit read and write (CC-705): one finding by
// ID, the verified flag, clearing a rejected explanation, and whether a cited document passage exists for a
// company. Nothing here logs finding or document text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// GetFinding returns the finding with the given ID. A missing finding
// wraps ErrNotFound.
func (s *Store) GetFinding(ctx context.Context, id uuid.UUID) (Finding, error) {
	const query = `
		SELECT id, run_id, type, severity, title, amount_paise,
		       keys, evidence, explanation, action, citations, proposal,
		       verified, status
		FROM findings
		WHERE id = $1;
	`
	var f Finding
	var amt *int64
	var keysJSON, evidenceJSON, citationsJSON, proposalJSON []byte
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&f.ID, &f.RunID, &f.Type, &f.Severity, &f.Title, &amt,
		&keysJSON, &evidenceJSON, &f.Explanation, &f.Action, &citationsJSON, &proposalJSON,
		&f.Verified, &f.Status,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Finding{}, fmt.Errorf("%w: finding %s", ErrNotFound, id)
		}
		return Finding{}, fmt.Errorf("store: get finding %s: %w", id, err)
	}
	if amt != nil {
		p := money.Paise(*amt)
		f.AmountPaise = &p
	}
	for _, c := range []struct {
		name string
		raw  []byte
		dst  any
	}{
		{"keys", keysJSON, &f.Keys},
		{"evidence", evidenceJSON, &f.Evidence},
		{"citations", citationsJSON, &f.Citations},
	} {
		if len(c.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(c.raw, c.dst); err != nil {
			return Finding{}, fmt.Errorf("store: get finding %s: unmarshal %s: %w", id, c.name, err)
		}
	}
	if len(proposalJSON) > 0 {
		var p JournalProposal
		if err := json.Unmarshal(proposalJSON, &p); err != nil {
			return Finding{}, fmt.Errorf("store: get finding %s: unmarshal proposal: %w", id, err)
		}
		f.Proposal = &p
	}
	return f, nil
}

// SetFindingVerified sets the verified flag of a finding of the given run.
// The workflow sets it when the finding's explanation passes verification.
// A finding that is missing, or belongs to another run, wraps ErrNotFound.
func (s *Store) SetFindingVerified(ctx context.Context, runID, findingID uuid.UUID, verified bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE findings SET verified = $3 WHERE id = $1 AND run_id = $2;`, findingID, runID, verified)
	if err != nil {
		return fmt.Errorf("store: set finding %s verified: %w", findingID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s in run %s", ErrNotFound, findingID, runID)
	}
	return nil
}

// ClearFindingExplanation clears the explanation, action, citations and
// proposal of a finding of the given run, and its verified flag. The
// workflow calls it before it reopens an explain step whose explanation
// failed verification, so a rejected explanation or proposal never stays
// on the finding. Clearing twice is harmless. A finding that is missing,
// or belongs to another run, wraps ErrNotFound.
func (s *Store) ClearFindingExplanation(ctx context.Context, runID, findingID uuid.UUID) error {
	const query = `
		UPDATE findings
		SET explanation = NULL, action = NULL, citations = NULL, proposal = NULL, verified = false
		WHERE id = $1 AND run_id = $2;
	`
	tag, err := s.pool.Exec(ctx, query, findingID, runID)
	if err != nil {
		return fmt.Errorf("store: clear finding %s explanation: %w", findingID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s in run %s", ErrNotFound, findingID, runID)
	}
	return nil
}

// CitationExists reports whether doc_chunks holds a passage (docID,
// section) that the company may cite: its own (company_id = company) or a
// shared one (company_id IS NULL). The company filter is in the SQL.
func (s *Store) CitationExists(ctx context.Context, company, docID, section string) (bool, error) {
	if company == "" || docID == "" || section == "" {
		return false, errors.New("store: citation exists: empty company, doc_id or section")
	}
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM doc_chunks
			WHERE doc_id = $2 AND section = $3 AND (company_id = $1 OR company_id IS NULL)
		);
	`
	var ok bool
	if err := s.pool.QueryRow(ctx, query, company, docID, section).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: citation exists: %w", err)
	}
	return ok, nil
}
