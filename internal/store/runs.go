package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// CloseRun represents a single financial period close run.
type CloseRun struct {
	ID              uuid.UUID  `json:"id"`
	CompanyID       string     `json:"company_id"`
	Month           string     `json:"month"`  // YYYY-MM
	Status          string     `json:"status"` // queued|running|done|partial|failed
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Error           *string    `json:"error,omitempty"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	CacheReadTokens int64      `json:"cache_read_tokens"`
	CostUSD         float64    `json:"cost_usd"`
	TraceID         *string    `json:"trace_id,omitempty"`
}

// Citation points to a specific document section.
type Citation struct {
	DocID   string `json:"doc_id"`
	Section string `json:"section"`
}

// EvidenceRef records the tool call and IDs that produced a finding.
type EvidenceRef struct {
	Server   string          `json:"server"` // "books" | "evidence"
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	IDs      []string        `json:"ids"`      // doc names, bank txn ids, chunk ids
	Artifact string          `json:"artifact"` // sha256 of the stored tool-result snapshot
}

// JournalLine represents one debit/credit line in a journal entry proposal.
type JournalLine struct {
	Account     string      `json:"account"`
	DebitPaise  money.Paise `json:"debit,omitempty"`
	CreditPaise money.Paise `json:"credit,omitempty"`
}

// JournalPayload is the content of an adjustment journal entry.
type JournalPayload struct {
	PostingDate string        `json:"posting_date"`
	Lines       []JournalLine `json:"lines"`
	Remark      string        `json:"remark"`
}

// JournalProposal represents a proposed adjusting entry awaiting checker approval.
type JournalProposal struct {
	ID        uuid.UUID      `json:"id"`
	FindingID *uuid.UUID     `json:"finding_id,omitempty"`
	CompanyID string         `json:"company_id"`
	Payload   JournalPayload `json:"payload"`
	Status    string         `json:"status"` // proposed|approved|posted|rejected
	Maker     string         `json:"maker"`
	Checker   *string        `json:"checker,omitempty"`
	Reason    *string        `json:"reason,omitempty"`
	ERPName   *string        `json:"erp_name,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	DecidedAt *time.Time     `json:"decided_at,omitempty"`
	PostedAt  *time.Time     `json:"posted_at,omitempty"`
}

// Finding represents an discrepancy or observation found during a close run.
type Finding struct {
	ID          uuid.UUID         `json:"id"`
	RunID       uuid.UUID         `json:"run_id"`
	Type        string            `json:"type"`
	Severity    string            `json:"severity"` // high | medium | low
	Title       string            `json:"title"`
	AmountPaise *money.Paise      `json:"amount_paise,omitempty"`
	Keys        map[string]string `json:"keys"`
	Evidence    []EvidenceRef     `json:"evidence"`
	Explanation *string           `json:"explanation,omitempty"`
	Action      *string           `json:"action,omitempty"`
	Citations   []Citation        `json:"citations,omitempty"`
	Proposal    *JournalProposal  `json:"proposal,omitempty"`
	Verified    bool              `json:"verified"`
	Status      string            `json:"status"` // open | needs_review | accepted | dismissed
}

// AuditLogEntry represents a recorded actor or tool action for compliance.
type AuditLogEntry struct {
	ID          int64           `json:"id"`
	At          time.Time       `json:"at"`
	Actor       string          `json:"actor"`
	Server      *string         `json:"server,omitempty"`
	Tool        *string         `json:"tool,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
	ResultCount *int            `json:"result_count,omitempty"`
	LatencyMS   *int            `json:"latency_ms,omitempty"`
	Error       *string         `json:"error,omitempty"`
}

// CreateCloseRun inserts a new close run record.
func (s *Store) CreateCloseRun(ctx context.Context, r CloseRun) error {
	const query = `
		INSERT INTO close_runs (
			id, company_id, month, status, started_at, finished_at,
			error, input_tokens, output_tokens, cache_read_tokens, cost_usd, trace_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
	`
	_, err := s.pool.Exec(ctx, query,
		r.ID,
		r.CompanyID,
		r.Month,
		r.Status,
		r.StartedAt,
		r.FinishedAt,
		r.Error,
		r.InputTokens,
		r.OutputTokens,
		r.CacheReadTokens,
		r.CostUSD,
		r.TraceID,
	)
	if err != nil {
		return fmt.Errorf("store: create close run %s: %w", r.ID, err)
	}
	return nil
}

// UpdateCloseRun updates an existing close run record.
func (s *Store) UpdateCloseRun(ctx context.Context, r CloseRun) error {
	const query = `
		UPDATE close_runs SET
			status = $2,
			started_at = $3,
			finished_at = $4,
			error = $5,
			input_tokens = $6,
			output_tokens = $7,
			cache_read_tokens = $8,
			cost_usd = $9,
			trace_id = $10
		WHERE id = $1;
	`
	tag, err := s.pool.Exec(ctx, query,
		r.ID,
		r.Status,
		r.StartedAt,
		r.FinishedAt,
		r.Error,
		r.InputTokens,
		r.OutputTokens,
		r.CacheReadTokens,
		r.CostUSD,
		r.TraceID,
	)
	if err != nil {
		return fmt.Errorf("store: update close run %s: %w", r.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: close run %s", ErrNotFound, r.ID)
	}
	return nil
}

// GetCloseRun retrieves a close run by ID.
func (s *Store) GetCloseRun(ctx context.Context, id uuid.UUID) (CloseRun, error) {
	const query = `
		SELECT id, company_id, month, status, started_at, finished_at,
		       error, input_tokens, output_tokens, cache_read_tokens, cost_usd, trace_id
		FROM close_runs
		WHERE id = $1;
	`
	var r CloseRun
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&r.ID,
		&r.CompanyID,
		&r.Month,
		&r.Status,
		&r.StartedAt,
		&r.FinishedAt,
		&r.Error,
		&r.InputTokens,
		&r.OutputTokens,
		&r.CacheReadTokens,
		&r.CostUSD,
		&r.TraceID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CloseRun{}, fmt.Errorf("%w: close run %s", ErrNotFound, id)
		}
		return CloseRun{}, fmt.Errorf("store: get close run: %w", err)
	}
	return r, nil
}

// CreateFinding inserts a finding and optionally its proposed journal entry.
func (s *Store) CreateFinding(ctx context.Context, f Finding) error {
	return s.CreateFindings(ctx, []Finding{f})
}

// CreateFindings inserts multiple findings in a single transaction.
func (s *Store) CreateFindings(ctx context.Context, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
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

			_, err = tx.Exec(ctx, query,
				f.ID,
				f.RunID,
				f.Type,
				f.Severity,
				f.Title,
				amt,
				keysJSON,
				evidenceJSON,
				f.Explanation,
				f.Action,
				citationsJSON,
				proposalJSON,
				f.Verified,
				status,
			)
			if err != nil {
				return fmt.Errorf("store: insert finding %s: %w", f.ID, err)
			}
		}
		return nil
	})
}

// ListFindingsByRun retrieves all findings for a given close run.
func (s *Store) ListFindingsByRun(ctx context.Context, runID uuid.UUID) ([]Finding, error) {
	const query = `
		SELECT id, run_id, type, severity, title, amount_paise,
		       keys, evidence, explanation, action, citations, proposal,
		       verified, status
		FROM findings
		WHERE run_id = $1
		ORDER BY id ASC;
	`
	rows, err := s.pool.Query(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list findings: %w", err)
	}
	defer rows.Close()

	var out []Finding
	for rows.Next() {
		var f Finding
		var amt *int64
		var keysJSON, evidenceJSON, citationsJSON, proposalJSON []byte
		if err := rows.Scan(
			&f.ID,
			&f.RunID,
			&f.Type,
			&f.Severity,
			&f.Title,
			&amt,
			&keysJSON,
			&evidenceJSON,
			&f.Explanation,
			&f.Action,
			&citationsJSON,
			&proposalJSON,
			&f.Verified,
			&f.Status,
		); err != nil {
			return nil, fmt.Errorf("store: scan finding: %w", err)
		}
		if amt != nil {
			p := money.Paise(*amt)
			f.AmountPaise = &p
		}
		if len(keysJSON) > 0 {
			if err := json.Unmarshal(keysJSON, &f.Keys); err != nil {
				return nil, fmt.Errorf("store: unmarshal keys: %w", err)
			}
		}
		if len(evidenceJSON) > 0 {
			if err := json.Unmarshal(evidenceJSON, &f.Evidence); err != nil {
				return nil, fmt.Errorf("store: unmarshal evidence: %w", err)
			}
		}
		if len(citationsJSON) > 0 {
			if err := json.Unmarshal(citationsJSON, &f.Citations); err != nil {
				return nil, fmt.Errorf("store: unmarshal citations: %w", err)
			}
		}
		if len(proposalJSON) > 0 {
			var prop JournalProposal
			if err := json.Unmarshal(proposalJSON, &prop); err != nil {
				return nil, fmt.Errorf("store: unmarshal proposal: %w", err)
			}
			f.Proposal = &prop
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate findings: %w", err)
	}
	return out, nil
}

// CreateJournalProposal inserts a new journal proposal record.
func (s *Store) CreateJournalProposal(ctx context.Context, p JournalProposal) error {
	payloadJSON, err := json.Marshal(p.Payload)
	if err != nil {
		return fmt.Errorf("store: marshal journal payload: %w", err)
	}

	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	const query = `
		INSERT INTO journal_proposals (
			id, finding_id, company_id, payload, status,
			maker, checker, reason, erp_name, created_at, decided_at, posted_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
	`
	_, err = s.pool.Exec(ctx, query,
		p.ID,
		p.FindingID,
		p.CompanyID,
		payloadJSON,
		p.Status,
		p.Maker,
		p.Checker,
		p.Reason,
		p.ERPName,
		createdAt,
		p.DecidedAt,
		p.PostedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create journal proposal %s: %w", p.ID, err)
	}
	return nil
}

// GetJournalProposal retrieves a journal proposal by ID.
func (s *Store) GetJournalProposal(ctx context.Context, id uuid.UUID) (JournalProposal, error) {
	const query = `
		SELECT id, finding_id, company_id, payload, status,
		       maker, checker, reason, erp_name, created_at, decided_at, posted_at
		FROM journal_proposals
		WHERE id = $1;
	`
	var p JournalProposal
	var payloadJSON []byte
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&p.ID,
		&p.FindingID,
		&p.CompanyID,
		&payloadJSON,
		&p.Status,
		&p.Maker,
		&p.Checker,
		&p.Reason,
		&p.ERPName,
		&p.CreatedAt,
		&p.DecidedAt,
		&p.PostedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JournalProposal{}, fmt.Errorf("%w: journal proposal %s", ErrNotFound, id)
		}
		return JournalProposal{}, fmt.Errorf("store: get journal proposal: %w", err)
	}
	if err := json.Unmarshal(payloadJSON, &p.Payload); err != nil {
		return JournalProposal{}, fmt.Errorf("store: unmarshal payload: %w", err)
	}
	return p, nil
}

// UpdateJournalProposal updates status and decision details on a journal proposal.
func (s *Store) UpdateJournalProposal(ctx context.Context, p JournalProposal) error {
	const query = `
		UPDATE journal_proposals SET
			status = $2,
			checker = $3,
			reason = $4,
			erp_name = $5,
			decided_at = $6,
			posted_at = $7
		WHERE id = $1;
	`
	tag, err := s.pool.Exec(ctx, query,
		p.ID,
		p.Status,
		p.Checker,
		p.Reason,
		p.ERPName,
		p.DecidedAt,
		p.PostedAt,
	)
	if err != nil {
		return fmt.Errorf("store: update journal proposal %s: %w", p.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: journal proposal %s", ErrNotFound, p.ID)
	}
	return nil
}

// InsertAuditLog records a compliance audit log entry and returns its assigned ID.
func (s *Store) InsertAuditLog(ctx context.Context, e AuditLogEntry) (int64, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}

	const query = `
		INSERT INTO audit_log (
			at, actor, server, tool, args, result_count, latency_ms, error
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id;
	`
	var id int64
	err := s.pool.QueryRow(ctx, query,
		at,
		e.Actor,
		e.Server,
		e.Tool,
		e.Args,
		e.ResultCount,
		e.LatencyMS,
		e.Error,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert audit log: %w", err)
	}
	return id, nil
}

// ListAuditLogs retrieves recent audit logs ordered from newest to oldest.
func (s *Store) ListAuditLogs(ctx context.Context, limit int) ([]AuditLogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	const query = `
		SELECT id, at, actor, server, tool, args, result_count, latency_ms, error
		FROM audit_log
		ORDER BY at DESC, id DESC
		LIMIT $1;
	`
	rows, err := s.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit logs: %w", err)
	}
	defer rows.Close()

	var out []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		if err := rows.Scan(
			&e.ID,
			&e.At,
			&e.Actor,
			&e.Server,
			&e.Tool,
			&e.Args,
			&e.ResultCount,
			&e.LatencyMS,
			&e.Error,
		); err != nil {
			return nil, fmt.Errorf("store: scan audit log: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit logs: %w", err)
	}
	return out, nil
}
