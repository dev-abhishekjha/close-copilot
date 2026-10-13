package agent

// Retrieval (CC-806). Retrieval is a workflow step, not a tool a model
// chooses: before a finding is explained, the workflow builds a query from
// the finding (buildQuery), runs evidence/search_documents through the MCP
// registry under the run's scope, and stores the top passages as a
// retrieval artifact. The explainer, whose only tool is emit_explanation,
// gets them as DOCUMENTS. No model that can call tools ever sees them: the
// investigator gets no Retriever and no document refs.
//
// This file is the only non-test file of the package that names the
// search tool (TestSearchDocumentsInvariantSourceScan). Nothing here logs
// queries or passage text.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/abhishekjha/close-copilot/internal/store"
)

const (
	// toolSearchDocuments is the evidence server's document search.
	toolSearchDocuments = "search_documents"
	// maxPassages is how many passages one finding keeps.
	maxPassages = 3
	// maxQueryRunes caps a retrieval query.
	maxQueryRunes = 300
)

// Passage is one retrieved document chunk.
type Passage struct {
	DocID   string `json:"doc_id"`
	DocType string `json:"doc_type"`
	Section string `json:"section"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	// SHA256 is the hex SHA-256 of Text.
	SHA256 string `json:"sha256"`
}

// RetrieveRequest is one finding's search. Company is the run's company,
// never a value from the finding.
type RetrieveRequest struct {
	Company string
	Month   string
	Finding store.Finding
	Query   string
}

// Retriever finds the passages that bear on one finding.
type Retriever interface {
	Retrieve(ctx context.Context, req RetrieveRequest) ([]Passage, error)
}

// RetrievalArtifact is the content of a retrieval artifact: the query and
// the passages, each with string doc_id and section fields, which the
// verifier collects (collectPassages).
type RetrievalArtifact struct {
	Query    string    `json:"query"`
	Passages []Passage `json:"passages"`
}

// MCPDocuments is the Retriever over evidence/search_documents. The
// server forces the company filter to the run scope on ctx (CC-506).
type MCPDocuments struct {
	Registry *Registry
}

var _ Retriever = (*MCPDocuments)(nil)

// searchArgs is search_documents' input.
type searchArgs struct {
	Query   string `json:"query"`
	Company string `json:"company"`
	K       int    `json:"k"`
}

// wireDocument is one row of search_documents' output (a bare array).
type wireDocument struct {
	DocID   string      `json:"doc_id"`
	DocType string      `json:"doc_type"`
	Section string      `json:"section"`
	Title   string      `json:"title"`
	Snippet string      `json:"snippet"`
	Score   json.Number `json:"score"`
}

// Retrieve searches the run company's documents and returns at most
// maxPassages passages, best first.
func (d *MCPDocuments) Retrieve(ctx context.Context, req RetrieveRequest) ([]Passage, error) {
	if req.Company == "" {
		return nil, errors.New("agent: retrieve: no company")
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, errors.New("agent: retrieve: empty query")
	}
	var rows []wireDocument
	if err := d.Registry.callJSON(ctx, ServerEvidence, toolSearchDocuments,
		searchArgs{Query: req.Query, Company: req.Company, K: maxPassages}, &rows); err != nil {
		return nil, err
	}
	if len(rows) > maxPassages {
		rows = rows[:maxPassages]
	}
	out := make([]Passage, 0, len(rows))
	for _, r := range rows {
		out = append(out, newPassage(r.DocID, r.DocType, r.Section, r.Title, r.Snippet))
	}
	return out, nil
}

// newPassage builds a passage and its text hash.
func newPassage(docID, docType, section, title, text string) Passage {
	sum := sha256.Sum256([]byte(text))
	return Passage{DocID: docID, DocType: docType, Section: section, Title: title, Text: text, SHA256: hex.EncodeToString(sum[:])}
}

// buildQuery is the search text for a finding: its type in words, then the
// supplier and the account when its keys carry them. Amounts, GSTINs and
// invoice numbers stay out: they add noise and are identifiers.
func buildQuery(f store.Finding) string {
	parts := []string{strings.ReplaceAll(strings.TrimSpace(f.Type), "_", " ")}
	for _, k := range []string{"supplier", "account"} {
		if v := strings.Join(strings.Fields(f.Keys[k]), " "); v != "" {
			parts = append(parts, v)
		}
	}
	q := strings.TrimSpace(strings.Join(parts, " "))
	if utf8.RuneCountInString(q) > maxQueryRunes {
		q = strings.TrimSpace(string([]rune(q)[:maxQueryRunes]))
	}
	return q
}
