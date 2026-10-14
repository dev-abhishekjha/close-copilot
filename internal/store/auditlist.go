package store

import (
	"context"
	"fmt"
	"time"
)

// Bounds for AuditFilter.Limit.
const (
	defaultAuditPage = 50
	maxAuditPage     = 200
	maxAuditFacets   = 200
)

// AuditFilter selects one page of the audit log. Empty strings and nil
// times mean "any". Actor, Server and Tool match exactly; From is
// inclusive and To exclusive. Before is the last id of the previous page
// (zero for the first page).
type AuditFilter struct {
	Actor  string
	Server string
	Tool   string
	From   *time.Time
	To     *time.Time
	Limit  int
	Before int64
}

// auditLimit clamps a page size to 1..200; zero or less means 50.
func auditLimit(n int) int {
	if n <= 0 {
		return defaultAuditPage
	}
	return min(n, maxAuditPage)
}

// ListAuditPage returns one page of audit rows, newest first (at, then id
// descending). next is the id to pass as Before for the following page, or
// 0 when there are no more rows. Every filter is a SQL parameter.
func (s *Store) ListAuditPage(ctx context.Context, f AuditFilter) (rows []AuditLogEntry, next int64, err error) {
	limit := auditLimit(f.Limit)
	const query = `
		SELECT id, at, actor, server, tool, args, result_count, latency_ms, error
		FROM audit_log
		WHERE ($1 = '' OR actor = $1)
		  AND ($2 = '' OR server = $2)
		  AND ($3 = '' OR tool = $3)
		  AND ($4::timestamptz IS NULL OR at >= $4)
		  AND ($5::timestamptz IS NULL OR at < $5)
		  AND ($6::bigint <= 0 OR id < $6)
		ORDER BY at DESC, id DESC
		LIMIT $7;
	`
	r, err := s.pool.Query(ctx, query, f.Actor, f.Server, f.Tool, f.From, f.To, f.Before, limit+1)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list audit page: %w", err)
	}
	out, err := scanAuditLogs(r)
	if err != nil {
		return nil, 0, err
	}
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

// ListAuditFacets returns the distinct actors, servers and tools in the
// audit log, sorted, at most 200 each, for the page's filter dropdowns.
func (s *Store) ListAuditFacets(ctx context.Context) (actors, servers, tools []string, err error) {
	facet := func(col string) ([]string, error) {
		// col is one of three constants below, never user input.
		q := fmt.Sprintf(`SELECT DISTINCT %[1]s FROM audit_log WHERE %[1]s IS NOT NULL ORDER BY 1 LIMIT %[2]d;`, col, maxAuditFacets)
		rows, err := s.pool.Query(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("store: audit facet %s: %w", col, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, fmt.Errorf("store: scan audit facet %s: %w", col, err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: iterate audit facet %s: %w", col, err)
		}
		return out, nil
	}
	if actors, err = facet("actor"); err != nil {
		return nil, nil, nil, err
	}
	if servers, err = facet("server"); err != nil {
		return nil, nil, nil, err
	}
	if tools, err = facet("tool"); err != nil {
		return nil, nil, nil, err
	}
	return actors, servers, tools, nil
}
