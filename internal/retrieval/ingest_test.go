package retrieval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestDocFromPath(t *testing.T) {
	tests := []struct {
		rel     string
		id, typ string
		company string // "" = NULL
		wantErr bool
	}{
		{"shared/POL-001-accounting-policy.pdf", "POL-001-accounting-policy", "policy", "", false},
		{"sharma/contracts/CON-bharat-polymers.pdf", "CON-bharat-polymers", "contract", "sharma", false},
		{"mehta/notes/NOTE-mehta-2026-04.pdf", "NOTE-mehta-2026-04", "close_note", "mehta", false},
		{"stray.pdf", "", "", "", true},
		{"sharma/other/x.pdf", "", "", "", true},
		{"shared/sub/x.pdf", "", "", "", true},
	}
	for _, tt := range tests {
		got, err := docFromPath(tt.rel)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v", tt.rel, err)
			continue
		}
		if tt.wantErr {
			if !strings.Contains(err.Error(), tt.rel) {
				t.Errorf("%s: error does not name the path: %v", tt.rel, err)
			}
			continue
		}
		if got.docID != tt.id || got.docType != tt.typ {
			t.Errorf("%s: got %+v", tt.rel, got)
		}
		if (got.companyID == nil) != (tt.company == "") || (got.companyID != nil && *got.companyID != tt.company) {
			t.Errorf("%s: company = %v", tt.rel, got.companyID)
		}
	}
}

type fakeConv struct {
	calls int
	md    map[string]string // by base name
}

func (f *fakeConv) ConvertPDF(_ context.Context, path string) (string, error) {
	f.calls++
	return f.md[filepath.Base(path)], nil
}

type fakeEmb struct {
	calls int
	dims  int
	err   error
	texts []string
}

func (f *fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	f.texts = append(f.texts, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dims)
	}
	return out, nil
}

type fakeStore struct {
	rows   map[string][]store.DocChunk
	writes int
}

func (f *fakeStore) DocHashes(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for id, rows := range f.rows {
		out[id] = rows[0].ContentHash
	}
	return out, nil
}

func (f *fakeStore) ReplaceDocChunks(_ context.Context, id string, c []store.DocChunk) error {
	f.writes++
	f.rows[id] = c
	return nil
}

func writeCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newFixture() (*fakeConv, *fakeEmb, *fakeStore) {
	conv := &fakeConv{md: map[string]string{
		"POL-001.pdf":  "## POL Manual\n\n## 4.2 Spread\n\nspread it\n",
		"CON-a.pdf":    "## Contract A\n\n## 1. Parties\n\nparty text\n",
		"NOTE-m-1.pdf": "## Note\n\nintro\n\n## Calendar\n\ncal text\n",
	}}
	return conv, &fakeEmb{dims: 384}, &fakeStore{rows: map[string][]store.DocChunk{}}
}

var corpusFiles = map[string]string{
	"shared/POL-001.pdf":         "pdf-pol",
	"sharma/contracts/CON-a.pdf": "pdf-con",
	"mehta/notes/NOTE-m-1.pdf":   "pdf-note",
	"mehta/notes/NOTE-m-1.md":    "ignored",
	"render/render.go":           "ignored",
}

func TestIngestFirstAndSecondRun(t *testing.T) {
	dir := writeCorpus(t, corpusFiles)
	conv, emb, st := newFixture()
	deps := IngestDeps{Converter: conv, Embedder: emb, Store: st}

	stats, err := Ingest(t.Context(), deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 3 || stats.Converted != 3 || stats.Skipped != 0 || stats.Chunks != 4 {
		t.Fatalf("stats = %+v", stats)
	}
	if c := st.rows["POL-001"][0]; c.CompanyID != nil || c.DocType != "policy" || *c.Section != "§4.2 Spread" || *c.Title != "POL Manual" {
		t.Errorf("policy chunk = %+v", c)
	}
	if c := st.rows["CON-a"][0]; c.CompanyID == nil || *c.CompanyID != "sharma" {
		t.Errorf("contract company = %v", c.CompanyID)
	}
	note := st.rows["NOTE-m-1"]
	if note[0].Section != nil || *note[1].Section != "Calendar" || *note[1].CompanyID != "mehta" {
		t.Errorf("note chunks = %+v", note)
	}
	joined := strings.Join(emb.texts, "|")
	for _, want := range []string{"POL Manual\n§4.2 Spread\nspread it", "Note\nintro", "Note\nCalendar\ncal text"} {
		if !strings.Contains(joined, want) {
			t.Errorf("embedder did not see %q in %q", want, joined)
		}
	}

	convCalls, embCalls, writes := conv.calls, emb.calls, st.writes
	stats, err = Ingest(t.Context(), deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 3 || stats.Converted != 0 || stats.Chunks != 0 {
		t.Fatalf("second run stats = %+v", stats)
	}
	if conv.calls != convCalls || emb.calls != embCalls || st.writes != writes {
		t.Errorf("second run did work: conv %d emb %d writes %d", conv.calls-convCalls, emb.calls-embCalls, st.writes-writes)
	}
}

func TestIngestChangedPDFReplacesOnlyThatDocument(t *testing.T) {
	dir := writeCorpus(t, corpusFiles)
	conv, emb, st := newFixture()
	deps := IngestDeps{Converter: conv, Embedder: emb, Store: st}
	if _, err := Ingest(t.Context(), deps, dir); err != nil {
		t.Fatal(err)
	}
	writes := st.writes
	if err := os.WriteFile(filepath.Join(dir, "sharma", "contracts", "CON-a.pdf"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := Ingest(t.Context(), deps, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Converted != 1 || stats.Skipped != 2 || st.writes != writes+1 {
		t.Fatalf("stats = %+v writes %d", stats, st.writes-writes)
	}
}

func TestIngestWrongDimensionFailsOnlyThatDocument(t *testing.T) {
	dir := writeCorpus(t, corpusFiles)
	conv, _, st := newFixture()
	emb := &dimByText{}
	stats, err := Ingest(t.Context(), IngestDeps{Converter: conv, Embedder: emb, Store: st}, dir)
	if err == nil || !strings.Contains(err.Error(), "CON-a") || !strings.Contains(err.Error(), "383") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := st.rows["CON-a"]; ok {
		t.Error("bad document stored rows")
	}
	if len(st.rows["POL-001"]) == 0 || len(st.rows["NOTE-m-1"]) == 0 {
		t.Error("other documents were not stored")
	}
	if stats.Converted != 3 {
		t.Errorf("stats = %+v", stats)
	}
}

type dimByText struct{}

func (dimByText) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		n := 384
		if strings.HasPrefix(t, "Contract A") {
			n = 383
		}
		out[i] = make([]float32, n)
	}
	return out, nil
}

func TestIngestEmbedErrorKeepsOldChunks(t *testing.T) {
	dir := writeCorpus(t, corpusFiles)
	conv, emb, st := newFixture()
	if _, err := Ingest(t.Context(), IngestDeps{Converter: conv, Embedder: emb, Store: st}, dir); err != nil {
		t.Fatal(err)
	}
	old := st.rows["CON-a"]
	if err := os.WriteFile(filepath.Join(dir, "sharma", "contracts", "CON-a.pdf"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	emb.err = errors.New("tei down")
	writes := st.writes
	_, err := Ingest(t.Context(), IngestDeps{Converter: conv, Embedder: emb, Store: st}, dir)
	if err == nil || !strings.Contains(err.Error(), "tei down") {
		t.Fatalf("err = %v", err)
	}
	if st.writes != writes || &st.rows["CON-a"][0] != &old[0] {
		t.Error("old chunks were touched")
	}
}

func TestIngestStrayPDFErrors(t *testing.T) {
	dir := writeCorpus(t, map[string]string{"stray.pdf": "x", "shared/POL-001.pdf": "p"})
	conv, emb, st := newFixture()
	_, err := Ingest(t.Context(), IngestDeps{Converter: conv, Embedder: emb, Store: st}, dir)
	if err == nil || !strings.Contains(err.Error(), "stray.pdf") {
		t.Fatalf("err = %v", err)
	}
	if len(st.rows["POL-001"]) == 0 {
		t.Error("valid document was not ingested")
	}
}
