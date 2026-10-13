package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// pipelineVersion is mixed into every document hash. Bump it when the
// chunking or the embedding model changes so documents are re-ingested.
const pipelineVersion = "chunk-v1/bge-small-en-v1.5"

// embedDims is the width of bge-small-en-v1.5 vectors and of doc_chunks.embedding.
const embedDims = 384

// Converter turns a PDF into Markdown. *Docling implements it.
type Converter interface {
	ConvertPDF(ctx context.Context, path string) (string, error)
}

// ChunkStore is the part of store.Store that ingestion needs.
type ChunkStore interface {
	DocHashes(ctx context.Context) (map[string]string, error)
	ReplaceDocChunks(ctx context.Context, docID string, chunks []store.DocChunk) error
}

// IngestDeps are the collaborators of Ingest.
type IngestDeps struct {
	Converter Converter
	Embedder  Embedder
	Store     ChunkStore
	Log       *slog.Logger
}

// IngestStats summarises one Ingest run.
type IngestStats struct {
	Documents int // PDFs found
	Skipped   int // unchanged, nothing called or written
	Converted int // documents sent to docling
	Chunks    int // chunk rows written
	Stale     int // documents in the table that are no longer in the corpus
	Duration  time.Duration
}

type docInfo struct {
	docID, docType string
	companyID      *string
}

// docFromPath derives the document identity from a path relative to the
// corpus root: shared/<id>.pdf, <company>/contracts/<id>.pdf or
// <company>/notes/<id>.pdf.
func docFromPath(rel string) (docInfo, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	stem := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	switch {
	case len(parts) == 2 && parts[0] == "shared":
		return docInfo{docID: stem, docType: "policy"}, nil
	case len(parts) == 3 && parts[0] != "shared" && parts[1] == "contracts":
		c := parts[0]
		return docInfo{docID: stem, docType: "contract", companyID: &c}, nil
	case len(parts) == 3 && parts[0] != "shared" && parts[1] == "notes":
		c := parts[0]
		return docInfo{docID: stem, docType: "close_note", companyID: &c}, nil
	}
	return docInfo{}, fmt.Errorf("retrieval: %s does not fit shared/*, <company>/contracts/* or <company>/notes/*", rel)
}

// Ingest walks corpusDir for PDFs, converts, chunks, embeds and stores each
// one, and skips documents whose hash is already stored. A failure on one
// document is logged and collected; the others still run.
func Ingest(ctx context.Context, deps IngestDeps, corpusDir string) (IngestStats, error) {
	start := time.Now()
	var stats IngestStats
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var pdfs []string
	err := filepath.WalkDir(corpusDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".pdf") {
			pdfs = append(pdfs, p) // WalkDir is lexical, so this is sorted
		}
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("retrieval: walk %s: %w", corpusDir, err)
	}

	hashes, err := deps.Store.DocHashes(ctx)
	if err != nil {
		return stats, fmt.Errorf("retrieval: load document hashes: %w", err)
	}

	seen := make(map[string]bool, len(pdfs))
	var errs []error
	for _, p := range pdfs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		stats.Documents++
		rel, _ := filepath.Rel(corpusDir, p)
		info, err := docFromPath(rel)
		if err != nil {
			errs = append(errs, err)
			log.Error("document failed", "path", rel, "err", err)
			continue
		}
		seen[info.docID] = true

		raw, err := os.ReadFile(p) //nolint:gosec // G304: p comes from walking the operator-supplied corpus directory
		if err != nil {
			err = fmt.Errorf("retrieval: read %s: %w", rel, err)
			errs = append(errs, err)
			log.Error("document failed", "doc_id", info.docID, "err", err)
			continue
		}
		sum := sha256.Sum256(append(raw, pipelineVersion...))
		hash := hex.EncodeToString(sum[:])
		if hashes[info.docID] == hash {
			stats.Skipped++
			log.Info("document", "doc_id", info.docID, "status", "skipped", "chunks", 0)
			continue
		}

		stats.Converted++
		n, err := ingestOne(ctx, deps, p, info, hash)
		if err != nil {
			errs = append(errs, fmt.Errorf("retrieval: ingest %s: %w", info.docID, err))
			log.Error("document failed", "doc_id", info.docID, "err", err)
			continue
		}
		stats.Chunks += n
		log.Info("document", "doc_id", info.docID, "status", "ingested", "chunks", n)
	}
	for id := range hashes {
		if !seen[id] {
			stats.Stale++
		}
	}
	if stats.Stale > 0 {
		log.Warn("documents in the table are not in the corpus; left alone", "count", stats.Stale)
	}
	stats.Duration = time.Since(start)
	return stats, errors.Join(errs...)
}

func ingestOne(ctx context.Context, deps IngestDeps, path string, info docInfo, hash string) (int, error) {
	md, err := deps.Converter.ConvertPDF(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("convert: %w", err)
	}
	title, chunks := ChunkMarkdown(md)
	if title == "" {
		title = info.docID
	}
	if len(chunks) == 0 {
		return 0, errors.New("no chunks produced")
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		if c.Section == "" {
			texts[i] = title + "\n" + c.Content
		} else {
			texts[i] = title + "\n" + c.Section + "\n" + c.Content
		}
	}
	vecs, err := deps.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != len(chunks) {
		return 0, fmt.Errorf("embed: got %d vectors for %d chunks", len(vecs), len(chunks))
	}
	rows := make([]store.DocChunk, len(chunks))
	for i, c := range chunks {
		if len(vecs[i]) != embedDims {
			return 0, fmt.Errorf("embed: vector %d has %d dimensions, want %d", i, len(vecs[i]), embedDims)
		}
		t := title
		row := store.DocChunk{
			DocID:       info.docID,
			DocType:     info.docType,
			CompanyID:   info.companyID,
			Title:       &t,
			Content:     c.Content,
			ContentHash: hash,
			Embedding:   pgvector.NewVector(vecs[i]),
		}
		if c.Section != "" {
			s := c.Section
			row.Section = &s
		}
		rows[i] = row
	}
	if err := deps.Store.ReplaceDocChunks(ctx, info.docID, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}
