package store

// Journal proposals as the maker-checker service sees them (CC-1004): create
// one from a finding's preview, find the live one of a finding, and list
// proposals by status. Nothing here writes erp_name, posted_at or the posted
// status: only the admin MCP tool does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrProposalExists reports that a finding already has a live journal
// proposal (any status except rejected).
var ErrProposalExists = errors.New("store: finding already has a live journal proposal")

// MaxProposalList caps ListProposalsByStatus.
const MaxProposalList = 200

// oneLivePerFindingIndex is the unique index of migration 0006.
const oneLivePerFindingIndex = "journal_proposals_one_live_per_finding"

// uniqueViolation is the Postgres SQLSTATE of a unique-constraint violation.
const uniqueViolation = "23505"

const proposalColumns = `id, finding_id, company_id, payload, status,
	       maker, checker, reason, erp_name, created_at, decided_at, posted_at`

// CreateProposalFromFinding inserts p as a new journal proposal and returns
// the stored row. When p's finding already has a live proposal it returns
// that proposal with an error wrapping ErrProposalExists.
func (s *Store) CreateProposalFromFinding(ctx context.Context, p JournalProposal) (JournalProposal, error) {
	if err := checkNewProposal(p); err != nil {
		return JournalProposal{}, err
	}
	payloadJSON, err := json.Marshal(p.Payload)
	if err != nil {
		return JournalProposal{}, fmt.Errorf("store: marshal journal payload: %w", err)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO journal_proposals (
			id, finding_id, company_id, payload, status,
			maker, checker, reason, erp_name, created_at, decided_at, posted_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, NULL, NULL, NULL, $7, NULL, NULL)
		RETURNING ` + proposalColumns + `;
	`
	got, err := scanProposal(s.pool.QueryRow(ctx, query,
		p.ID, p.FindingID, p.CompanyID, payloadJSON, p.Status, p.Maker, p.CreatedAt))
	if err != nil {
		if isOneLiveViolation(err) {
			existing, ok, gerr := s.GetProposalByFinding(ctx, *p.FindingID)
			if gerr != nil {
				return JournalProposal{}, fmt.Errorf("%w: finding %s (lookup: %v)", ErrProposalExists, *p.FindingID, gerr)
			}
			if !ok {
				return JournalProposal{}, fmt.Errorf("%w: finding %s", ErrProposalExists, *p.FindingID)
			}
			return existing, fmt.Errorf("%w: finding %s has proposal %s", ErrProposalExists, *p.FindingID, existing.ID)
		}
		return JournalProposal{}, fmt.Errorf("store: create proposal %s: %w", p.ID, err)
	}
	return got, nil
}

// checkNewProposal refuses a proposal CreateProposalFromFinding must not
// insert: a nil ID or finding, a blank company or maker, or any status other
// than proposed.
func checkNewProposal(p JournalProposal) error {
	switch {
	case p.ID == uuid.Nil:
		return errors.New("store: create proposal: nil proposal id")
	case p.FindingID == nil || *p.FindingID == uuid.Nil:
		return errors.New("store: create proposal: nil finding id")
	case p.CompanyID == "":
		return errors.New("store: create proposal: empty company")
	case p.Maker == "":
		return errors.New("store: create proposal: empty maker")
	case p.Status != "proposed":
		return fmt.Errorf("store: create proposal: status %q, want proposed", p.Status)
	case p.Checker != nil || p.Reason != nil || p.ERPName != nil || p.DecidedAt != nil || p.PostedAt != nil:
		return errors.New("store: create proposal: decision fields must be empty")
	}
	return nil
}

// isOneLiveViolation reports whether err is a unique violation of the
// one-live-proposal-per-finding index.
func isOneLiveViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == oneLivePerFindingIndex
}

// GetProposalByFinding returns the live journal proposal of a finding: the
// one whose status is not rejected. ok is false when there is none.
func (s *Store) GetProposalByFinding(ctx context.Context, findingID uuid.UUID) (JournalProposal, bool, error) {
	const query = `
		SELECT ` + proposalColumns + `
		FROM journal_proposals
		WHERE finding_id = $1 AND status <> 'rejected'
		ORDER BY created_at DESC, id
		LIMIT 1;
	`
	p, err := scanProposal(s.pool.QueryRow(ctx, query, findingID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JournalProposal{}, false, nil
		}
		return JournalProposal{}, false, fmt.Errorf("store: proposal of finding %s: %w", findingID, err)
	}
	return p, true, nil
}

// ListProposalsByStatus returns up to limit journal proposals with the given
// status, oldest first. limit <= 0 or above MaxProposalList means
// MaxProposalList.
func (s *Store) ListProposalsByStatus(ctx context.Context, status string, limit int) ([]JournalProposal, error) {
	limit = proposalListLimit(limit)
	const query = `
		SELECT ` + proposalColumns + `
		FROM journal_proposals
		WHERE status = $1
		ORDER BY created_at ASC, id ASC
		LIMIT $2;
	`
	rows, err := s.pool.Query(ctx, query, status, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list proposals %q: %w", status, err)
	}
	defer rows.Close()
	out := []JournalProposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proposals %q: %w", status, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proposals %q: %w", status, err)
	}
	return out, nil
}

// proposalListLimit clamps a list limit to (0, MaxProposalList].
func proposalListLimit(limit int) int {
	if limit <= 0 || limit > MaxProposalList {
		return MaxProposalList
	}
	return limit
}
