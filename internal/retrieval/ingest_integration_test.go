//go:build integration

package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestIntegrationIngest(t *testing.T) {
	cfg := itConfig(t, config.EnvTEIEmbedURL, config.EnvTEIRerankURL, config.EnvDoclingURL)
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
	defer st.Close()
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	tei, err := NewTEI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := NewDocling(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// A clean slate for the corpus's own documents only.
	if _, err := st.Pool().Exec(ctx, `DELETE FROM doc_chunks`); err != nil {
		t.Fatal(err)
	}
	deps := IngestDeps{Converter: doc, Embedder: tei, Store: st}
	corpus := filepath.Join("..", "..", "corpus")

	first, err := Ingest(ctx, deps, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if first.Documents == 0 || first.Chunks == 0 || first.Converted != first.Documents {
		t.Fatalf("first run = %+v", first)
	}
	before := count(t, st, `SELECT count(*) FROM doc_chunks`)

	second, err := Ingest(ctx, deps, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if second.Converted != 0 || second.Chunks != 0 || second.Skipped != second.Documents {
		t.Fatalf("second run = %+v", second)
	}
	if after := count(t, st, `SELECT count(*) FROM doc_chunks`); after != before {
		t.Fatalf("rows changed on the second run: %d -> %d", before, after)
	}

	checks := []struct {
		name, q string
		want    int
	}{
		{"embedding width", `SELECT count(*) FROM doc_chunks WHERE vector_dims(embedding) <> 384`, 0},
		{"NULL company only for the policy", `SELECT count(*) FROM doc_chunks WHERE company_id IS NULL AND doc_id <> 'POL-001-accounting-policy'`, 0},
		{"policy is shared", `SELECT count(*) FROM doc_chunks WHERE doc_id = 'POL-001-accounting-policy' AND company_id IS NOT NULL`, 0},
		{"contracts and notes have a company", `SELECT count(*) FROM doc_chunks WHERE doc_type <> 'policy' AND (company_id IS NULL OR company_id NOT IN ('sharma', 'mehta'))`, 0},
	}
	for _, c := range checks {
		if got := count(t, st, c.q); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if got := count(t, st, `SELECT count(*) FROM doc_chunks WHERE doc_id = 'POL-001-accounting-policy' AND section LIKE '§4.2 %'`); got < 1 {
		t.Error("§4.2 is not a section of the policy")
	}
}

func count(t *testing.T, st *store.Store, q string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
