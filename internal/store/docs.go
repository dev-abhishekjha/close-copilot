package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// DocChunk represents a chunked passage from a policy, contract, or close note.
type DocChunk struct {
	ID          int64           `json:"id"`
	DocID       string          `json:"doc_id"`
	DocType     string          `json:"doc_type"`             // policy|contract|close_note
	CompanyID   *string         `json:"company_id,omitempty"` // NULL = shared by all
	Section     *string         `json:"section,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Content     string          `json:"content"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"embedding"`
}

// InsertDocChunks inserts a slice of document chunks in a single transaction.
func (s *Store) InsertDocChunks(ctx context.Context, chunks []DocChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	return s.WithTx(ctx, func(tx pgx.Tx) error {
		const query = `
			INSERT INTO doc_chunks (
				doc_id, doc_type, company_id, section, title,
				content, content_hash, embedding
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
		`
		for _, c := range chunks {
			_, err := tx.Exec(ctx, query,
				c.DocID,
				c.DocType,
				c.CompanyID,
				c.Section,
				c.Title,
				c.Content,
				c.ContentHash,
				c.Embedding,
			)
			if err != nil {
				return fmt.Errorf("store: insert doc chunk for %s: %w", c.DocID, err)
			}
		}
		return nil
	})
}

// DeleteDocChunksByDocID deletes all chunks belonging to a document.
func (s *Store) DeleteDocChunksByDocID(ctx context.Context, docID string) error {
	const query = `DELETE FROM doc_chunks WHERE doc_id = $1;`
	_, err := s.pool.Exec(ctx, query, docID)
	if err != nil {
		return fmt.Errorf("store: delete doc chunks for %s: %w", docID, err)
	}
	return nil
}

// GetDocChunk retrieves a single document chunk by its database ID.
func (s *Store) GetDocChunk(ctx context.Context, id int64) (DocChunk, error) {
	const query = `
		SELECT id, doc_id, doc_type, company_id, section, title,
		       content, content_hash, embedding
		FROM doc_chunks
		WHERE id = $1;
	`
	var c DocChunk
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&c.ID,
		&c.DocID,
		&c.DocType,
		&c.CompanyID,
		&c.Section,
		&c.Title,
		&c.Content,
		&c.ContentHash,
		&c.Embedding,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DocChunk{}, fmt.Errorf("%w: doc chunk %d", ErrNotFound, id)
		}
		return DocChunk{}, fmt.Errorf("store: get doc chunk %d: %w", id, err)
	}
	return c, nil
}

// SearchDocChunksByVector performs cosine distance similarity search on embeddings.
// If companyID is provided, it matches chunks that are shared (company_id IS NULL)
// or specific to that company.
func (s *Store) SearchDocChunksByVector(ctx context.Context, queryEmbedding pgvector.Vector, companyID *string, limit int) ([]DocChunk, error) {
	if limit <= 0 {
		limit = 5
	}

	query := `
		SELECT id, doc_id, doc_type, company_id, section, title,
		       content, content_hash, embedding
		FROM doc_chunks
	`
	var args []any
	args = append(args, queryEmbedding)

	if companyID != nil {
		query += ` WHERE (company_id IS NULL OR company_id = $2)`
		args = append(args, *companyID)
		query += ` ORDER BY embedding <=> $1 ASC LIMIT $3;`
		args = append(args, limit)
	} else {
		query += ` ORDER BY embedding <=> $1 ASC LIMIT $2;`
		args = append(args, limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search doc chunks by vector: %w", err)
	}
	defer rows.Close()

	var out []DocChunk
	for rows.Next() {
		var c DocChunk
		if err := rows.Scan(
			&c.ID,
			&c.DocID,
			&c.DocType,
			&c.CompanyID,
			&c.Section,
			&c.Title,
			&c.Content,
			&c.ContentHash,
			&c.Embedding,
		); err != nil {
			return nil, fmt.Errorf("store: scan doc chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate doc chunks: %w", err)
	}
	return out, nil
}

// SearchDocChunksByText performs full-text search against the generated tsv tsvector column.
func (s *Store) SearchDocChunksByText(ctx context.Context, queryText string, companyID *string, limit int) ([]DocChunk, error) {
	if limit <= 0 {
		limit = 5
	}

	query := `
		SELECT id, doc_id, doc_type, company_id, section, title,
		       content, content_hash, embedding
		FROM doc_chunks
		WHERE tsv @@ plainto_tsquery('english', $1)
	`
	var args []any
	args = append(args, queryText)

	if companyID != nil {
		query += ` AND (company_id IS NULL OR company_id = $2)`
		args = append(args, *companyID)
		query += ` ORDER BY ts_rank(tsv, plainto_tsquery('english', $1)) DESC LIMIT $3;`
		args = append(args, limit)
	} else {
		query += ` ORDER BY ts_rank(tsv, plainto_tsquery('english', $1)) DESC LIMIT $2;`
		args = append(args, limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search doc chunks by text: %w", err)
	}
	defer rows.Close()

	var out []DocChunk
	for rows.Next() {
		var c DocChunk
		if err := rows.Scan(
			&c.ID,
			&c.DocID,
			&c.DocType,
			&c.CompanyID,
			&c.Section,
			&c.Title,
			&c.Content,
			&c.ContentHash,
			&c.Embedding,
		); err != nil {
			return nil, fmt.Errorf("store: scan doc chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate doc chunks: %w", err)
	}
	return out, nil
}
