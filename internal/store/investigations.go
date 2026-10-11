package store

// A finding's investigation result (CC-706). The investigator stores its
// answer as an explanation artifact and each tool result it read as a
// tool_result artifact, then records here the resolution type (in the
// finding's keys, as "resolution") and appends the tool results to the
// finding's evidence, so the verifier and cmd/audit can ground the answer
// in what the agent read. Nothing here logs keys, evidence or text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ResolutionKey is the finding key that holds an investigation's
// resolution type.
const ResolutionKey = "resolution"

// maxResolutionRunes caps a resolution type.
const maxResolutionRunes = 64

// SetFindingResolution sets keys.resolution of a finding of run runID of
// company companyID and appends evidence to the finding's evidence, in
// order. A ref whose artifact the finding's evidence (or an earlier ref of
// evidence) already names is skipped, so a re-run investigation never adds
// the same snapshot twice. A finding of another run or company is
// ErrNotFound and is left unchanged (CC-506).
func (s *Store) SetFindingResolution(ctx context.Context, runID, findingID uuid.UUID, companyID, resolution string, evidence []EvidenceRef) error {
	switch {
	case resolution == "" || utf8.RuneCountInString(resolution) > maxResolutionRunes:
		return fmt.Errorf("store: set finding resolution: resolution must be 1 to %d characters", maxResolutionRunes)
	case companyID == "":
		return errors.New("store: set finding resolution: empty company")
	}
	var refs []EvidenceRef
	seen := map[string]bool{}
	for i, e := range evidence {
		if e.Artifact == "" {
			return fmt.Errorf("store: set finding resolution: evidence %d names no artifact", i)
		}
		if seen[e.Artifact] {
			continue
		}
		seen[e.Artifact] = true
		if e.IDs == nil {
			e.IDs = []string{}
		}
		refs = append(refs, e)
	}
	if refs == nil {
		refs = []EvidenceRef{}
	}
	evidenceJSON, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("store: marshal evidence: %w", err)
	}
	// The appended refs keep their order; one whose artifact the finding
	// already names is left out.
	const query = `
		UPDATE findings f
		SET keys = (CASE WHEN jsonb_typeof(f.keys) = 'object' THEN f.keys ELSE '{}'::jsonb END)
		           || jsonb_build_object('resolution', $4::text),
		    evidence = (CASE WHEN jsonb_typeof(f.evidence) = 'array' THEN f.evidence ELSE '[]'::jsonb END)
		           || COALESCE((
		                SELECT jsonb_agg(e.value ORDER BY e.ord)
		                FROM jsonb_array_elements($5::jsonb) WITH ORDINALITY AS e(value, ord)
		                WHERE NOT EXISTS (
		                    SELECT 1
		                    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(f.evidence) = 'array' THEN f.evidence ELSE '[]'::jsonb END) AS x(value)
		                    WHERE x.value->>'artifact' = e.value->>'artifact')
		              ), '[]'::jsonb)
		FROM close_runs r
		WHERE f.id = $1 AND f.run_id = $2 AND r.id = f.run_id AND r.company_id = $3;
	`
	tag, err := s.pool.Exec(ctx, query, findingID, runID, companyID, resolution, string(evidenceJSON))
	if err != nil {
		return fmt.Errorf("store: set finding %s resolution: %w", findingID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s in run %s", ErrNotFound, findingID, runID)
	}
	return nil
}
