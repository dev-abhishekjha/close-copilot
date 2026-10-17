//go:build integration

package retrieval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func listEnv(t *testing.T) *store.Store {
	t.Helper()
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
	return st
}

func seedList(t *testing.T, st *store.Store, docID, docType string, company *string, section, content string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM doc_chunks WHERE doc_id LIKE 'test-ui11-%'`)
	})
	_, err := st.Pool().Exec(ctx, `INSERT INTO doc_chunks (doc_id, doc_type, company_id, section, title, content, content_hash, embedding)
VALUES ($1, $2, $3, $4, $5, $6, 'test-ui11', $7)`,
		docID, docType, company, section, "Title "+docID, content, pgvector.NewVector(make([]float32, 384)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationListTenant(t *testing.T) {
	st := listEnv(t)
	sharma, mehta := "sharma", "mehta"
	seedList(t, st, "test-ui11-sharma", "contract", &sharma, "§1", "sharma text")
	seedList(t, st, "test-ui11-sharma", "contract", &sharma, "§2", "sharma more")
	seedList(t, st, "test-ui11-mehta", "contract", &mehta, "§1", "mehta text")
	seedList(t, st, "test-ui11-shared", "policy", nil, "§1", "policy text")
	seedList(t, st, "test-ui11-same", "close_note", &sharma, "§1", "sharma same")
	seedList(t, st, "test-ui11-same", "close_note", &mehta, "§1", "mehta same")
	l := NewLister(st.Pool())
	ctx := context.Background()

	docs, err := l.ListDocuments(ctx, "sharma", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]DocSummary{}
	for _, d := range docs {
		if d.CompanyID != nil && *d.CompanyID == "mehta" {
			t.Fatalf("sharma lists mehta document %s", d.DocID)
		}
		seen[d.DocID] = d
	}
	if seen["test-ui11-sharma"].Sections != 2 || seen["test-ui11-shared"].CompanyID != nil || seen["test-ui11-mehta"].DocID != "" {
		t.Errorf("listing %+v", seen)
	}
	if only, err := l.ListDocuments(ctx, "sharma", []string{"policy"}); err != nil {
		t.Fatal(err)
	} else {
		for _, d := range only {
			if d.DocType != "policy" {
				t.Errorf("type filter let %s through", d.DocType)
			}
		}
	}

	if _, err := l.GetDocument(ctx, "sharma", "test-ui11-mehta"); !errors.Is(err, ErrDocNotFound) {
		t.Errorf("mehta doc as sharma: %v", err)
	}
	d, err := l.GetDocument(ctx, "sharma", "test-ui11-sharma")
	if err != nil || len(d.Sections) != 2 || d.Sections[0].Section != "§1" {
		t.Errorf("sharma doc %+v %v", d, err)
	}
	for company, want := range map[string]string{"sharma": "sharma same", "mehta": "mehta same"} {
		d, err := l.GetDocument(ctx, company, "test-ui11-same")
		if err != nil || len(d.Sections) != 1 || d.Sections[0].Content != want {
			t.Errorf("%s same-id doc %+v %v", company, d, err)
		}
	}
	if _, err := l.GetDocument(ctx, "", "test-ui11-shared"); !errors.Is(err, ErrNoCompany) {
		t.Errorf("empty company: %v", err)
	}
}
