package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProposalSaveTimeout bounds the save and the commit of WithProposalLock.
// Both run detached from the caller's cancellation, so a caller cancelled
// after posting to ERPNext still records the post.
const ProposalSaveTimeout = 10 * time.Second

// WithProposalLock runs fn while one transaction holds the journal_proposals
// row id with SELECT ... FOR UPDATE, so a second caller for the same ID
// blocks until the first commits or rolls back, and then sees the saved
// row. fn gets the locked row and a save function that writes it within the
// same transaction: status, checker, reason, erp_name, decided_at and
// posted_at, never the payload. The transaction commits when fn returns nil and rolls
// back otherwise (also on a panic, which is re-raised).
//
// save and the commit run under context.WithoutCancel(ctx) with a
// ProposalSaveTimeout deadline. A missing row returns an error wrapping
// ErrNotFound without calling fn.
func (s *Store) WithProposalLock(ctx context.Context, id uuid.UUID, fn func(ctx context.Context, p JournalProposal, save func(ctx context.Context, p JournalProposal) error) error) (err error) {
	if fn == nil {
		return errors.New("store: proposal lock: nil fn")
	}
	if id == uuid.Nil {
		return errors.New("store: proposal lock: nil proposal id")
	}
	if s == nil || s.pool == nil {
		return errors.New("store: proposal lock: store is not open")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: proposal lock %s: begin: %w", id, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rctx, cancel := detached(ctx)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	p, err := scanProposal(tx.QueryRow(ctx, lockProposalSQL, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: journal proposal %s", ErrNotFound, id)
		}
		return fmt.Errorf("store: proposal lock %s: %w", id, err)
	}

	save := func(sctx context.Context, np JournalProposal) error {
		if np.ID != id {
			return fmt.Errorf("store: proposal lock %s: save of another proposal %s", id, np.ID)
		}
		dctx, cancel := detached(sctx)
		defer cancel()
		tag, err := tx.Exec(dctx, saveProposalSQL, np.ID, np.Status, np.Checker, np.Reason, np.ERPName, np.DecidedAt, np.PostedAt)
		if err != nil {
			return fmt.Errorf("store: proposal lock %s: save: %w", id, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("store: proposal lock %s: save touched %d rows", id, tag.RowsAffected())
		}
		return nil
	}

	if err := fn(ctx, p, save); err != nil {
		return err
	}
	cctx, cancel := detached(ctx)
	defer cancel()
	if err := tx.Commit(cctx); err != nil {
		return fmt.Errorf("store: proposal lock %s: commit: %w", id, err)
	}
	committed = true
	return nil
}

// detached is ctx without its cancellation, bounded by ProposalSaveTimeout.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ProposalSaveTimeout)
}

const lockProposalSQL = `
	SELECT id, finding_id, company_id, payload, status,
	       maker, checker, reason, erp_name, created_at, decided_at, posted_at
	FROM journal_proposals
	WHERE id = $1
	FOR UPDATE;
`

const saveProposalSQL = `
	UPDATE journal_proposals SET
		status = $2,
		checker = $3,
		reason = $4,
		erp_name = $5,
		decided_at = $6,
		posted_at = $7
	WHERE id = $1;
`

// scanProposal reads one journal_proposals row in lockProposalSQL's column
// order.
func scanProposal(row pgx.Row) (JournalProposal, error) {
	var p JournalProposal
	var payload []byte
	if err := row.Scan(&p.ID, &p.FindingID, &p.CompanyID, &payload, &p.Status,
		&p.Maker, &p.Checker, &p.Reason, &p.ERPName, &p.CreatedAt, &p.DecidedAt, &p.PostedAt); err != nil {
		return JournalProposal{}, err
	}
	if err := json.Unmarshal(payload, &p.Payload); err != nil {
		return JournalProposal{}, fmt.Errorf("unmarshal payload: %w", err)
	}
	return p, nil
}
