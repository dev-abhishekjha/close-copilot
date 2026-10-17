package retrieval

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoCompany is returned when a listing is asked for without a company.
// It never falls back to "shared documents only".
var ErrNoCompany = errors.New("retrieval: company is required")

// ErrDocNotFound is returned when no visible chunk has the document id. A
// document of another company answers the same way as a missing one.
var ErrDocNotFound = errors.New("retrieval: document not found")

// listLimit caps ListDocuments.
const listLimit = 500

// DocSummary is one document in a listing. Title is the first chunk's title
// and Sections the chunk count. CompanyID is nil for shared policy.
type DocSummary struct {
	DocID, DocType, Title string
	CompanyID             *string
	Sections              int
}

// DocSection is one chunk of a document, in reading order.
type DocSection struct {
	Section, Content string
}

// Document is a whole document: its chunks in id order.
type Document struct {
	DocID, DocType, Title string
	CompanyID             *string
	Sections              []DocSection
}

// Lister reads documents from doc_chunks for humans. It calls no embedder and
// no model.
type Lister struct{ db Querier }

// NewLister builds a Lister over db (a *pgxpool.Pool works).
func NewLister(db Querier) *Lister { return &Lister{db: db} }

// listSQL returns the listing statement. The company predicate (and the
// optional type predicate) sit in the WHERE, before GROUP BY, ORDER BY and
// LIMIT. Parameters: $1 company, then $2 doc types when withTypes.
func listSQL(withTypes bool) string {
	where := "(company_id IS NULL OR company_id = $1)"
	if withTypes {
		where += " AND doc_type = ANY($2)"
	}
	return fmt.Sprintf(`SELECT doc_id, doc_type, company_id, (array_agg(title ORDER BY id))[1], count(*)
FROM doc_chunks
WHERE %s
GROUP BY doc_id, doc_type, company_id
ORDER BY doc_type, doc_id, company_id NULLS FIRST
LIMIT %d`, where, listLimit)
}

// getSQL returns the document statement. Parameters: $1 company, $2 doc id.
const getSQL = `SELECT doc_type, company_id, section, title, content
FROM doc_chunks
WHERE doc_id = $2 AND (company_id IS NULL OR company_id = $1)
ORDER BY id`

// ListDocuments returns the documents visible to company (its own and the
// shared policy), ordered by type then id. Empty docTypes means all types.
func (l *Lister) ListDocuments(ctx context.Context, company string, docTypes []string) ([]DocSummary, error) {
	if company == "" {
		return nil, ErrNoCompany
	}
	args := []any{company}
	if len(docTypes) > 0 {
		args = append(args, docTypes)
	}
	rows, err := l.db.Query(ctx, listSQL(len(docTypes) > 0), args...)
	if err != nil {
		return nil, fmt.Errorf("retrieval: list documents: %w", err)
	}
	defer rows.Close()
	var out []DocSummary
	for rows.Next() {
		var d DocSummary
		var title *string
		if err := rows.Scan(&d.DocID, &d.DocType, &d.CompanyID, &title, &d.Sections); err != nil {
			return nil, fmt.Errorf("retrieval: list documents: %w", err)
		}
		if title != nil {
			d.Title = *title
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("retrieval: list documents: %w", err)
	}
	return out, nil
}

// GetDocument returns the chunks of docID visible to company, in id order.
// No visible chunk is ErrDocNotFound.
func (l *Lister) GetDocument(ctx context.Context, company, docID string) (Document, error) {
	if company == "" {
		return Document{}, ErrNoCompany
	}
	rows, err := l.db.Query(ctx, getSQL, company, docID)
	if err != nil {
		return Document{}, fmt.Errorf("retrieval: get document: %w", err)
	}
	defer rows.Close()
	doc := Document{DocID: docID}
	for rows.Next() {
		var typ string
		var cid, section, title *string
		var content string
		if err := rows.Scan(&typ, &cid, &section, &title, &content); err != nil {
			return Document{}, fmt.Errorf("retrieval: get document: %w", err)
		}
		if len(doc.Sections) == 0 {
			doc.DocType, doc.CompanyID = typ, cid
			if title != nil {
				doc.Title = *title
			}
		}
		s := DocSection{Content: content}
		if section != nil {
			s.Section = *section
		}
		doc.Sections = append(doc.Sections, s)
	}
	if err := rows.Err(); err != nil {
		return Document{}, fmt.Errorf("retrieval: get document: %w", err)
	}
	if len(doc.Sections) == 0 {
		return Document{}, ErrDocNotFound
	}
	return doc, nil
}
