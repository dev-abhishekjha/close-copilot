package retrieval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

const (
	// defaultK is how many hits Search returns when k is 0.
	defaultK = 5
	// rerankPool is how many fused candidates go to the cross-encoder.
	rerankPool = 20
	// branchLimit is how many rows each of the vector and full-text branches
	// contributes to the fusion.
	branchLimit = 40
)

// SearchMode selects how far the pipeline runs. It exists so the recall
// report can compare the stages; the public Search is always ModeHybridRerank.
type SearchMode string

const (
	// ModeVector ranks by embedding distance only.
	ModeVector SearchMode = "vector"
	// ModeHybrid fuses vector and full-text ranks with RRF.
	ModeHybrid SearchMode = "hybrid"
	// ModeHybridRerank fuses with RRF, then reranks the top candidates.
	ModeHybridRerank SearchMode = "hybrid+rerank"
)

// Hit is one retrieved chunk.
type Hit struct {
	ChunkID   int64
	DocID     string
	DocType   string
	CompanyID *string // nil for the shared policy
	Section   string
	Title     string
	Content   string
	// RRF is the fused score (the vector score alone in ModeVector).
	RRF float64
	// RerankScore is TEI's raw cross-encoder score; zero unless reranked.
	RerankScore float32
}

// Querier is the part of *pgxpool.Pool that Searcher needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// candidateSource returns up to rerankPool candidates, already filtered to the
// company (and doc types) and ordered best first.
type candidateSource interface {
	candidates(ctx context.Context, mode SearchMode, vec []float32, query, company string, docTypes []string) ([]Hit, error)
}

// Searcher runs hybrid search over doc_chunks. It calls no generative model:
// TEI embedding and reranking are local, non-generative services.
type Searcher struct {
	src      candidateSource
	embedder Embedder
	reranker Reranker
	log      *slog.Logger
}

// NewSearcher builds a Searcher over db (a *pgxpool.Pool works).
func NewSearcher(db Querier, e Embedder, r Reranker) *Searcher {
	return &Searcher{src: pgSource{db: db}, embedder: e, reranker: r, log: slog.New(slog.DiscardHandler)}
}

// Search returns the top k chunks (5 when k is 0) for query that company may
// read: its own and the shared policy (company_id IS NULL). The company, and
// docTypes when given, are filtered inside the SQL before any ranking, so the
// top k can never hold another company's chunk. An empty company is an error,
// never "all companies".
//
// Freshness: chunks of documents removed from corpus/ stay in doc_chunks and
// are not filtered here (search cannot tell them from live documents); they
// are still company-filtered, so this is staleness, not a leak.
func (s *Searcher) Search(ctx context.Context, query, company string, docTypes []string, k int) ([]Hit, error) {
	return s.SearchMode(ctx, ModeHybridRerank, query, company, docTypes, k)
}

// SearchMode is Search with an explicit mode, for the recall report.
func (s *Searcher) SearchMode(ctx context.Context, mode SearchMode, query, company string, docTypes []string, k int) ([]Hit, error) {
	switch mode {
	case ModeVector, ModeHybrid, ModeHybridRerank:
	default:
		return nil, fmt.Errorf("retrieval: unknown search mode %q", mode)
	}
	if company == "" {
		return nil, errors.New("retrieval: search needs a company")
	}
	if k <= 0 {
		k = defaultK
	}
	if strings.TrimSpace(query) == "" {
		return []Hit{}, nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("retrieval: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("retrieval: embed query: got %d vectors, want 1", len(vecs))
	}
	cands, err := s.src.candidates(ctx, mode, vecs[0], query, company, docTypes)
	if err != nil {
		return nil, fmt.Errorf("retrieval: candidates: %w", err)
	}
	if len(cands) > rerankPool {
		cands = cands[:rerankPool]
	}
	if mode == ModeHybridRerank && len(cands) > 0 {
		texts := make([]string, len(cands))
		for i, c := range cands {
			texts[i] = rerankText(c)
		}
		scored, err := s.reranker.Rerank(ctx, query, texts)
		if err != nil {
			return nil, fmt.Errorf("retrieval: rerank: %w", err)
		}
		cands, err = applyRerank(cands, scored)
		if err != nil {
			return nil, err
		}
	}
	if len(cands) > k {
		cands = cands[:k]
	}
	if s.log != nil {
		s.log.DebugContext(ctx, "search", "mode", string(mode), "company", company, "hits", len(cands))
	}
	if cands == nil {
		cands = []Hit{}
	}
	return cands, nil
}

func rerankText(h Hit) string {
	var b strings.Builder
	if h.Title != "" {
		b.WriteString(h.Title + "\n")
	}
	if h.Section != "" {
		b.WriteString(h.Section + "\n")
	}
	b.WriteString(h.Content)
	return b.String()
}

// applyRerank orders cands by rerank score, best first; ties keep the RRF order.
func applyRerank(cands []Hit, scored []Scored) ([]Hit, error) {
	if len(scored) != len(cands) {
		return nil, fmt.Errorf("retrieval: rerank returned %d scores for %d candidates", len(scored), len(cands))
	}
	out := make([]Hit, len(cands))
	seen := make([]bool, len(cands))
	for i, sc := range scored {
		if sc.Index < 0 || sc.Index >= len(cands) || seen[sc.Index] {
			return nil, fmt.Errorf("retrieval: rerank returned bad index %d", sc.Index)
		}
		seen[sc.Index] = true
		h := cands[sc.Index]
		h.RerankScore = sc.Score
		out[i] = h
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RerankScore > out[j].RerankScore })
	return out, nil
}

// buildSQL returns the query for mode. The tenant predicate and the optional
// doc_type predicate sit inside every ranking CTE, before ORDER BY ... LIMIT,
// so a ranked row of another company never exists. (An HNSW scan with a WHERE
// can return fewer than 40 rows; that is acceptable at this corpus size and the
// filter must not be loosened to work around it.)
//
// Parameters: $1 query vector, $2 company, then $3 query text (hybrid only),
// then the doc types when withTypes.
func buildSQL(mode SearchMode, withTypes bool) string {
	typeIdx := 3
	if mode != ModeVector {
		typeIdx = 4
	}
	where := "(company_id IS NULL OR company_id = $2)"
	if withTypes {
		where += fmt.Sprintf(" AND doc_type = ANY($%d)", typeIdx)
	}
	vCTE := fmt.Sprintf(`v AS (
  SELECT id, row_number() OVER (ORDER BY embedding <=> $1) AS r
  FROM doc_chunks
  WHERE %s
  ORDER BY embedding <=> $1
  LIMIT %d
)`, where, branchLimit)
	const final = `SELECT c.id, c.doc_id, c.doc_type, c.company_id, c.section, c.title, c.content, f.rrf
FROM fused f JOIN doc_chunks c ON c.id = f.id
ORDER BY f.rrf DESC, c.id`
	if mode == ModeVector {
		return fmt.Sprintf(`WITH %s, fused AS (
  SELECT id, (1.0 / (60 + r))::float8 AS rrf FROM v ORDER BY r LIMIT %d
)
%s`, vCTE, rerankPool, final)
	}
	tCTE := fmt.Sprintf(`t AS (
  SELECT id, row_number() OVER (ORDER BY ts_rank_cd(tsv, q) DESC) AS r
  FROM doc_chunks, websearch_to_tsquery('english', $3) AS q
  WHERE tsv @@ q AND %s
  ORDER BY ts_rank_cd(tsv, q) DESC
  LIMIT %d
)`, where, branchLimit)
	return fmt.Sprintf(`WITH %s, %s, fused AS (
  SELECT id, sum(1.0 / (60 + r))::float8 AS rrf
  FROM (SELECT id, r FROM v UNION ALL SELECT id, r FROM t) AS ranked
  GROUP BY id
  ORDER BY rrf DESC
  LIMIT %d
)
%s`, vCTE, tCTE, rerankPool, final)
}

type pgSource struct{ db Querier }

func (p pgSource) candidates(ctx context.Context, mode SearchMode, vec []float32, query, company string, docTypes []string) ([]Hit, error) {
	args := []any{pgvector.NewVector(vec), company}
	if mode != ModeVector {
		args = append(args, query)
	}
	if len(docTypes) > 0 {
		args = append(args, docTypes)
	}
	rows, err := p.db.Query(ctx, buildSQL(mode, len(docTypes) > 0), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var section, title *string
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.DocType, &h.CompanyID, &section, &title, &h.Content, &h.RRF); err != nil {
			return nil, err
		}
		if section != nil {
			h.Section = *section
		}
		if title != nil {
			h.Title = *title
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
