package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeQueryEmbedder struct {
	err  error
	seen []string
}

func (f *fakeQueryEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.seen = append(f.seen, texts...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, embedDims)
	}
	return out, nil
}

type fakeReranker struct {
	scores []float32 // by input index
	err    error
	calls  int
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, texts []string) ([]Scored, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Scored, len(texts))
	for i := range texts {
		out[i] = Scored{Index: i, Score: f.scores[i]}
	}
	return out, nil
}

type fakeSource struct {
	hits    []Hit
	gotMode SearchMode
	gotCo   string
	gotDT   []string
}

func (f *fakeSource) candidates(_ context.Context, mode SearchMode, _ []float32, _, company string, docTypes []string) ([]Hit, error) {
	f.gotMode, f.gotCo, f.gotDT = mode, company, docTypes
	return f.hits, nil
}

func mkHits(n int) []Hit {
	h := make([]Hit, n)
	for i := range h {
		h[i] = Hit{ChunkID: int64(i + 1), DocID: "D", Content: "c"}
	}
	return h
}

func TestSearchOrdering(t *testing.T) {
	tests := []struct {
		name    string
		mode    SearchMode
		n       int
		k       int
		scores  []float32
		wantIDs []int64
	}{
		{"default k, no rerank order kept", ModeHybrid, 8, 0, nil, []int64{1, 2, 3, 4, 5}},
		{"rerank reorders", ModeHybridRerank, 4, 2, []float32{0.1, 0.9, 0.5, 0.7}, []int64{2, 4}},
		{"rerank k larger than pool", ModeHybridRerank, 3, 10, []float32{1, 3, 2}, []int64{2, 3, 1}},
		{"vector mode skips rerank", ModeVector, 3, 2, nil, []int64{1, 2}},
		{"no candidates", ModeHybridRerank, 0, 5, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{hits: mkHits(tc.n)}
			rr := &fakeReranker{scores: tc.scores}
			s := &Searcher{src: src, embedder: &fakeQueryEmbedder{}, reranker: rr}
			got, err := s.SearchMode(context.Background(), tc.mode, "q", "sharma", nil, tc.k)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("want non-nil slice")
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d hits, want %d", len(got), len(tc.wantIDs))
			}
			for i, id := range tc.wantIDs {
				if got[i].ChunkID != id {
					t.Fatalf("hit %d = %d, want %d", i, got[i].ChunkID, id)
				}
			}
			if tc.mode != ModeHybridRerank && rr.calls != 0 {
				t.Fatal("reranker called outside hybrid+rerank")
			}
		})
	}
}

func TestSearchErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		company string
		query   string
		emb     error
		rr      error
		wantErr bool
		wantNil bool
	}{
		{"empty company", "", "q", nil, nil, true, false},
		{"embed fails", "sharma", "q", boom, nil, true, false},
		{"rerank fails", "sharma", "q", nil, boom, true, false},
		{"empty query", "sharma", "  ", nil, nil, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb := &fakeQueryEmbedder{err: tc.emb}
			s := &Searcher{src: &fakeSource{hits: mkHits(3)}, embedder: emb, reranker: &fakeReranker{scores: []float32{1, 2, 3}, err: tc.rr}}
			got, err := s.Search(context.Background(), tc.query, tc.company, nil, 5)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantNil && (len(got) != 0 || len(emb.seen) != 0) {
				t.Fatalf("empty query should return nothing without embedding: %v %v", got, emb.seen)
			}
		})
	}
}

func TestSearchPassesCompanyAndTypes(t *testing.T) {
	src := &fakeSource{}
	s := &Searcher{src: src, embedder: &fakeQueryEmbedder{}, reranker: &fakeReranker{}}
	if _, err := s.Search(context.Background(), "q", "mehta", []string{"contract"}, 0); err != nil {
		t.Fatal(err)
	}
	if src.gotCo != "mehta" || len(src.gotDT) != 1 || src.gotMode != ModeHybridRerank {
		t.Fatalf("source got %q %v %q", src.gotCo, src.gotDT, src.gotMode)
	}
}

func TestApplyRerankRejectsBadIndex(t *testing.T) {
	if _, err := applyRerank(mkHits(2), []Scored{{Index: 0, Score: 1}, {Index: 5, Score: 2}}); err == nil {
		t.Fatal("want error for out-of-range index")
	}
	if _, err := applyRerank(mkHits(2), []Scored{{Index: 0, Score: 1}}); err == nil {
		t.Fatal("want error for short result")
	}
}

// The tenant predicate must sit inside each ranking CTE, before ORDER BY and
// LIMIT, never in an outer WHERE.
func TestBuildSQLTenantFilterInsideCTEs(t *testing.T) {
	const pred = "(company_id IS NULL OR company_id = $2)"
	for _, withTypes := range []bool{false, true} {
		for _, mode := range []SearchMode{ModeVector, ModeHybrid, ModeHybridRerank} {
			sql := buildSQL(mode, withTypes)
			want := 2
			if mode == ModeVector {
				want = 1
			}
			if n := strings.Count(sql, pred); n != want {
				t.Fatalf("%s types=%v: predicate appears %d times, want %d", mode, withTypes, n, want)
			}
			// Each CTE body that selects from doc_chunks ends with LIMIT; the
			// predicate must precede the first LIMIT of its CTE.
			for _, name := range []string{"v AS (", "t AS ("} {
				i := strings.Index(sql, name)
				if i < 0 {
					continue
				}
				body := sql[i:]
				lim := strings.Index(body, "LIMIT")
				if p := strings.Index(body, pred); p < 0 || p > lim {
					t.Fatalf("%s %s: tenant predicate not inside the CTE before LIMIT", mode, name)
				}
			}
			if withTypes != strings.Contains(sql, "doc_type = ANY(") {
				t.Fatalf("%s: doc_type filter presence wrong", mode)
			}
			if withTypes && strings.Count(sql, "doc_type = ANY(") != want {
				t.Fatalf("%s: doc_type filter not in every CTE", mode)
			}
		}
	}
}
