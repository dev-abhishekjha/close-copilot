//go:build integration

package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgvector/pgvector-go"
	"go.yaml.in/yaml/v3"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// searchEnv opens the test database and the TEI clients. It never deletes
// rows that are not its own (TestIntegrationIngest owns DELETE FROM doc_chunks).
func searchEnv(t *testing.T) (*store.Store, *TEI) {
	t.Helper()
	cfg := itConfig(t, config.EnvTEIEmbedURL, config.EnvTEIRerankURL)
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv(config.EnvDatabaseURL)
	}
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL and DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	tei, err := NewTEI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st, tei
}

type seedDoc struct {
	id, docType string
	company     *string
	title, sect string
	content     string
}

func seed(t *testing.T, st *store.Store, tei *TEI, docs []seedDoc) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM doc_chunks WHERE doc_id LIKE 'test-cc804-%'`)
	})
	for _, d := range docs {
		vecs, err := tei.Embed(ctx, []string{d.title + "\n" + d.sect + "\n" + d.content})
		if err != nil {
			t.Fatal(err)
		}
		title, sect := d.title, d.sect
		row := store.DocChunk{
			DocID: d.id, DocType: d.docType, CompanyID: d.company, Title: &title, Section: &sect,
			Content: d.content, ContentHash: "test-cc804", Embedding: pgvector.NewVector(vecs[0]),
		}
		if err := st.ReplaceDocChunks(ctx, d.id, []store.DocChunk{row}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegrationSearchTenant(t *testing.T) {
	st, tei := searchEnv(t)
	sharma, mehta := "sharma", "mehta"
	seed(t, st, tei, []seedDoc{
		{"test-cc804-sharma", "contract", &sharma, "Zephyrite Couriers", "§3 Billing", "Zephyrite Couriers bills quarterly a fixed fee of rupees nine thousand."},
		{"test-cc804-mehta", "contract", &mehta, "Quillfeather Couriers", "§3 Billing", "Quillfeather Couriers bills quarterly a fixed fee of rupees nine thousand."},
		{"test-cc804-shared", "policy", nil, "Courier Policy", "§1 Couriers", "Courier invoices are booked on the date the service was delivered."},
	})
	s := NewSearcher(st.Pool(), tei, tei)
	ctx := context.Background()

	tests := []struct {
		name, company, query string
		docTypes             []string
		forbid               string
		wantShared           bool
	}{
		{"sharma asks for the mehta supplier", "sharma", "Quillfeather Couriers quarterly fixed fee", nil, "mehta", false},
		{"mehta asks for the sharma supplier", "mehta", "Zephyrite Couriers quarterly fixed fee", nil, "sharma", false},
		{"sharma with doc types", "sharma", "Quillfeather Couriers quarterly fixed fee", []string{"contract", "policy"}, "mehta", false},
		{"mehta with doc types", "mehta", "Zephyrite Couriers quarterly fixed fee", []string{"contract", "policy"}, "sharma", false},
		{"shared policy readable by sharma", "sharma", "courier invoices booked on the date the service was delivered", nil, "mehta", true},
		{"shared policy readable by mehta", "mehta", "courier invoices booked on the date the service was delivered", nil, "sharma", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []SearchMode{ModeVector, ModeHybrid, ModeHybridRerank} {
				hits, err := s.SearchMode(ctx, mode, tc.query, tc.company, tc.docTypes, 20)
				if err != nil {
					t.Fatal(err)
				}
				sawShared := false
				for _, h := range hits {
					if h.CompanyID != nil && *h.CompanyID == tc.forbid {
						t.Fatalf("%s: %s query returned a %s chunk (%s)", mode, tc.company, tc.forbid, h.DocID)
					}
					if h.CompanyID == nil && h.DocID == "test-cc804-shared" {
						sawShared = true
					}
				}
				if tc.wantShared && !sawShared {
					t.Fatalf("%s: shared policy chunk not returned", mode)
				}
			}
		})
	}

	if _, err := s.Search(ctx, "anything", "", nil, 5); err == nil {
		t.Fatal("empty company must be rejected")
	}
}

type devQuery struct {
	Company string `yaml:"company"`
	Query   string `yaml:"query"`
	DocID   string `yaml:"doc_id"`
	Section string `yaml:"section"`
}

func TestIntegrationSearchRecall(t *testing.T) {
	st, tei := searchEnv(t)
	ctx := context.Background()
	var n int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM doc_chunks WHERE doc_id = 'POL-001-accounting-policy'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Skip("no POL-001 chunks in doc_chunks: run cmd/ingest first")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "evals", "dev", "retrieval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Queries []devQuery `yaml:"queries"`
	}
	if err := yaml.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Queries) != 20 {
		t.Fatalf("dev set has %d queries, want 20", len(set.Queries))
	}
	s := NewSearcher(st.Pool(), tei, tei)
	recall := map[SearchMode]int{}
	modes := []SearchMode{ModeVector, ModeHybrid, ModeHybridRerank}
	for _, q := range set.Queries {
		for _, mode := range modes {
			hits, err := s.SearchMode(ctx, mode, q.Query, q.Company, nil, 5)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, h := range hits {
				if h.CompanyID != nil && *h.CompanyID != q.Company {
					t.Fatalf("%s: %s query returned a %s chunk", mode, q.Company, *h.CompanyID)
				}
				if h.DocID == q.DocID && h.Section == q.Section {
					found = true
				}
			}
			if found {
				recall[mode]++
			} else {
				t.Logf("miss %-13s %-6s %q (want %s %s)", mode, q.Company, q.Query, q.DocID, q.Section)
			}
		}
	}
	total := len(set.Queries)
	for _, mode := range modes {
		t.Logf("recall@5 %-13s = %d/%d = %.2f", mode, recall[mode], total, float64(recall[mode])/float64(total))
	}
	if recall[ModeHybridRerank]+2 < recall[ModeVector] {
		t.Fatalf("hybrid+rerank (%d) is well below vector only (%d)", recall[ModeHybridRerank], recall[ModeVector])
	}
}
