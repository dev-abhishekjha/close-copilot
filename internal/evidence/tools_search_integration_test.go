//go:build integration

package evidence_test

// search_documents over real Postgres and TEI (CC-805): Sharma never sees
// Mehta. Own rows under test-cc805- doc ids, own cleanup. Run separately from
// the retrieval integration tests (TestIntegrationIngest wipes doc_chunks).

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/retrieval"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func teiConfig(t *testing.T) config.Config {
	t.Helper()
	var cfg config.Config
	cfg.TEIEmbedURL = os.Getenv(config.EnvTEIEmbedURL)
	cfg.TEIRerankURL = os.Getenv(config.EnvTEIRerankURL)
	for _, u := range []string{cfg.TEIEmbedURL, cfg.TEIRerankURL} {
		if u == "" {
			t.Skip("TEI_EMBED_URL and TEI_RERANK_URL not set")
		}
		hc := &http.Client{Timeout: 3 * time.Second} //nolint:forbidigo // test-only reachability probe
		resp, err := hc.Get(u)
		if err != nil {
			t.Skipf("%s is not reachable", u)
		}
		_ = resp.Body.Close()
	}
	return cfg
}

func TestIntegrationSearchDocuments(t *testing.T) {
	cfg := teiConfig(t)
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
	tei, err := retrieval.NewTEI(cfg)
	if err != nil {
		t.Fatal(err)
	}

	sharma, mehta := "sharma", "mehta"
	docs := []struct {
		id, docType string
		company     *string
		title, body string
	}{
		{"test-cc805-sharma", "contract", &sharma, "Zephyrite Couriers", "Zephyrite Couriers bills quarterly a fixed fee of rupees nine thousand."},
		{"test-cc805-mehta", "contract", &mehta, "Quillfeather Couriers", "Quillfeather Couriers bills quarterly a fixed fee of rupees nine thousand."},
		{"test-cc805-mehta-note", "close_note", &mehta, "Quillfeather note", "Quillfeather Couriers quarterly fee accrual is booked at month end."},
		{"test-cc805-shared", "policy", nil, "Courier Policy", "Courier invoices are booked on the date the service was delivered."},
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM doc_chunks WHERE doc_id LIKE 'test-cc805-%'`)
	})
	for _, d := range docs {
		vecs, err := tei.Embed(ctx, []string{d.title + "\n" + d.body})
		if err != nil {
			t.Fatal(err)
		}
		title, sect := d.title, "§1"
		if err := st.ReplaceDocChunks(ctx, d.id, []store.DocChunk{{
			DocID: d.id, DocType: d.docType, CompanyID: d.company, Title: &title, Section: &sect,
			Content: d.body, ContentHash: "test-cc805", Embedding: pgvector.NewVector(vecs[0]),
		}}); err != nil {
			t.Fatal(err)
		}
	}

	srv := searchServer(t, retrieval.NewSearcher(st.Pool(), tei, tei))
	cs := scopedSession(t, srv, runScope(t))

	for name, extra := range map[string][]any{
		"no filter":    nil,
		"with types":   {"doc_types", []string{"contract", "close_note", "policy"}},
		"contracts":    {"doc_types", []string{"contract"}},
		"shared first": {"k", 10},
	} {
		t.Run(name, func(t *testing.T) {
			args := searchArgs(append([]any{"query", "Quillfeather Couriers quarterly fixed fee"}, extra...)...)
			var ids []string
			for _, h := range decodeOut(t, cs, args) {
				ids = append(ids, h.DocID)
				if strings.HasPrefix(h.DocID, "test-cc805-mehta") {
					t.Errorf("sharma's scope returned mehta chunk %s", h.DocID)
				}
				if len([]rune(h.Snippet)) > 600 {
					t.Errorf("%s: snippet longer than 600", h.DocID)
				}
			}
			t.Logf("returned %v", ids)
		})
	}

	var ids []string
	for _, h := range decodeOut(t, cs, searchArgs("query", "courier invoices booked on the date the service was delivered", "k", 10)) {
		ids = append(ids, h.DocID)
	}
	if !slices.Contains(ids, "test-cc805-shared") {
		t.Errorf("shared policy not returned to sharma: %v", ids)
	}

	wantOutOfScope(t, cs, "search_documents", searchArgs("query", "Quillfeather Couriers quarterly fixed fee", "company", "mehta"))
}
