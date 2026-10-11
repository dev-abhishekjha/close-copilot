package store

// A finding's explanation columns (CC-704). The explainer stores its
// validated output as an explanation artifact first, then copies it here so
// the UI reads the findings table. Nothing here logs the text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// SetFindingExplanation sets a finding's explanation, suggested action,
// citations and proposed journal entry. A nil proposal clears the column;
// nil citations are stored as an empty list. Writing the same values again
// is harmless, so a re-run explain step may repeat it. The finding must
// belong to run runID of company companyID (CC-506): a finding of another
// run or company is ErrNotFound and is left unchanged.
func (s *Store) SetFindingExplanation(ctx context.Context, findingID, runID uuid.UUID, companyID, explanation, action string, citations []Citation, proposal *JournalProposal) error {
	if explanation == "" {
		return errors.New("store: set finding explanation: empty explanation")
	}
	if action == "" {
		return errors.New("store: set finding explanation: empty action")
	}
	if citations == nil {
		citations = []Citation{}
	}
	citationsJSON, err := json.Marshal(citations)
	if err != nil {
		return fmt.Errorf("store: marshal citations: %w", err)
	}
	var proposalJSON []byte
	if proposal != nil {
		if proposalJSON, err = json.Marshal(proposal); err != nil {
			return fmt.Errorf("store: marshal proposal: %w", err)
		}
	}
	const query = `
		UPDATE findings f
		SET explanation = $4, action = $5, citations = $6, proposal = $7
		FROM close_runs r
		WHERE f.id = $1 AND f.run_id = $2 AND r.id = f.run_id AND r.company_id = $3;
	`
	tag, err := s.pool.Exec(ctx, query, findingID, runID, companyID, explanation, action, citationsJSON, proposalJSON)
	if err != nil {
		return fmt.Errorf("store: set finding %s explanation: %w", findingID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s in run %s", ErrNotFound, findingID, runID)
	}
	return nil
}
