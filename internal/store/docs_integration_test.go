//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func docChunk(doc, content, hash string, dims int) store.DocChunk {
	title := "T"
	return store.DocChunk{
		DocID: doc, DocType: "policy", Title: &title, Content: content,
		ContentHash: hash, Embedding: pgvector.NewVector(make([]float32, dims)),
	}
}

func TestIntegrationDocChunks(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)
	for _, id := range []string{"IT-A", "IT-B"} {
		if err := st.DeleteDocChunksByDocID(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = st.DeleteDocChunksByDocID(context.Background(), "IT-A")
		_ = st.DeleteDocChunksByDocID(context.Background(), "IT-B")
	})

	if err := st.ReplaceDocChunks(ctx, "IT-A", []store.DocChunk{docChunk("IT-A", "one", "h1", 384), docChunk("IT-A", "two", "h1", 384)}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceDocChunks(ctx, "IT-B", []store.DocChunk{docChunk("IT-B", "b", "hb", 384)}); err != nil {
		t.Fatal(err)
	}
	hashes, err := st.DocHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hashes["IT-A"] != "h1" || hashes["IT-B"] != "hb" {
		t.Fatalf("hashes = %v", hashes)
	}

	// Replace swaps the whole set.
	if err := st.ReplaceDocChunks(ctx, "IT-A", []store.DocChunk{docChunk("IT-A", "three", "h2", 384)}); err != nil {
		t.Fatal(err)
	}
	hashes, _ = st.DocHashes(ctx)
	if hashes["IT-A"] != "h2" {
		t.Fatalf("hash after replace = %q", hashes["IT-A"])
	}
	if n := countDoc(t, st, "IT-A"); n != 1 {
		t.Fatalf("IT-A rows = %d, want 1", n)
	}

	// A failed insert (wrong dimension) rolls the delete back.
	err = st.ReplaceDocChunks(ctx, "IT-A", []store.DocChunk{docChunk("IT-A", "bad", "h3", 383)})
	if err == nil {
		t.Fatal("want an error for a 383-dimension vector")
	}
	hashes, _ = st.DocHashes(ctx)
	if hashes["IT-A"] != "h2" || countDoc(t, st, "IT-A") != 1 {
		t.Fatalf("old chunks lost after failed replace: %v", hashes)
	}
}

func countDoc(t *testing.T, st *store.Store, doc string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM doc_chunks WHERE doc_id = $1`, doc).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
