package evidence_test

// search_documents tests (CC-805): fake searcher, no Postgres, no TEI, over
// the same scoped HTTP stack as the other evidence tools.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/retrieval"
)

type searchCall struct {
	query, company string
	docTypes       []string
	k              int
}

// fakeSearcher records its calls and returns hits.
type fakeSearcher struct {
	mu    sync.Mutex
	calls []searchCall
	hits  []retrieval.Hit
	err   error
}

func (f *fakeSearcher) Search(_ context.Context, query, company string, docTypes []string, k int) ([]retrieval.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, searchCall{query, company, docTypes, k})
	return f.hits, f.err
}

func (f *fakeSearcher) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSearcher) last() searchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func searchServer(t *testing.T, s evidence.DocSearcher) *httptest.Server {
	t.Helper()
	sc, err := mcpkit.NewScoper(scopeTestKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-evidence", Version: "search"}, nil)
	opts := []evidence.Option{evidence.WithScope(sc)}
	if s != nil {
		opts = append(opts, evidence.WithSearcher(s))
	}
	evidence.RegisterTools(srv, nil, opts...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(srv, contractToken)}, log,
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}), mcpkit.WithScoper(sc, "/mcp")))
	t.Cleanup(h.Close)
	return h
}

func sharmaSearch(t *testing.T, f *fakeSearcher) *mcp.ClientSession {
	t.Helper()
	return scopedSession(t, searchServer(t, f), runScope(t))
}

func searchArgs(kv ...any) map[string]any {
	m := map[string]any{"query": "rent escalation"}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func wantSearchErr(t *testing.T, cs *mcp.ClientSession, f *fakeSearcher, args map[string]any, contains string) {
	t.Helper()
	before := f.n()
	isErr, msg := toolResult(t, cs, "search_documents", args)
	if !isErr || !strings.Contains(msg, contains) {
		t.Fatalf("%v: IsError=%v %q, want a tool error containing %q", args, isErr, msg, contains)
	}
	if f.n() != before {
		t.Fatalf("%v: refused call reached the searcher", args)
	}
}

func TestSearchDocumentsOutOfScopeNeverSearches(t *testing.T) {
	f := &fakeSearcher{}
	cs := sharmaSearch(t, f)
	for _, c := range []string{"mehta", "nobody"} {
		wantOutOfScope(t, cs, "search_documents", searchArgs("company", c))
	}
	// A bad query under another company is still out_of_scope: scope first.
	wantOutOfScope(t, cs, "search_documents", map[string]any{"query": "", "company": "mehta", "k": 99})

	disc := scopedSession(t, searchServer(t, f), mintScope(t, mcpkit.DiscoveryScope(time.Now())))
	wantOutOfScope(t, disc, "search_documents", searchArgs())
	wantOutOfScope(t, disc, "search_documents", searchArgs("company", "sharma"))
	if f.n() != 0 {
		t.Fatalf("searcher called %d times, want 0", f.n())
	}

	srv := searchServer(t, f)
	if c, err := contractConnect(t, srv, contractToken); err == nil {
		_ = c.Close()
		t.Error("connected without a run scope")
	}
}

func TestSearchDocumentsCompany(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"omitted": searchArgs(),
		"blank":   searchArgs("company", ""),
		"same":    searchArgs("company", "sharma"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSearcher{}
			if isErr, msg := toolResult(t, sharmaSearch(t, f), "search_documents", args); isErr {
				t.Fatalf("tool error: %s", msg)
			}
			if f.n() != 1 || f.last().company != "sharma" {
				t.Fatalf("calls = %+v, want one call for sharma", f.calls)
			}
		})
	}
}

func TestSearchDocumentsDocTypes(t *testing.T) {
	f := &fakeSearcher{}
	cs := sharmaSearch(t, f)
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"policy"}, "policy"},
		{[]string{"contract"}, "contract"},
		{[]string{"close_note"}, "close_note"},
		{[]string{"policy", "contract", "policy"}, "policy,contract"},
		{[]string{"close_note", "policy", "contract"}, "close_note,policy,contract"},
		{nil, ""},
	} {
		args := searchArgs()
		if c.in != nil {
			args["doc_types"] = c.in
		}
		if isErr, msg := toolResult(t, cs, "search_documents", args); isErr {
			t.Fatalf("%v: %s", c.in, msg)
		}
		if got := strings.Join(f.last().docTypes, ","); got != c.want {
			t.Errorf("doc_types %v reached the searcher as %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"invoice", "", "POLICY", "policy,contract", "%"} {
		wantSearchErr(t, cs, f, searchArgs("doc_types", []string{bad}), "policy, contract, close_note")
		wantSearchErr(t, cs, f, searchArgs("doc_types", []string{"policy", bad}), "policy, contract, close_note")
	}
}

func TestSearchDocumentsBounds(t *testing.T) {
	f := &fakeSearcher{}
	cs := sharmaSearch(t, f)
	if isErr, msg := toolResult(t, cs, "search_documents", searchArgs()); isErr || f.last().k != 5 {
		t.Fatalf("default k: IsError=%v %q k=%d, want 5", isErr, msg, f.last().k)
	}
	for _, k := range []int{1, 10} {
		if isErr, msg := toolResult(t, cs, "search_documents", searchArgs("k", k)); isErr || f.last().k != k {
			t.Fatalf("k=%d: IsError=%v %q", k, isErr, msg)
		}
	}
	for _, k := range []int{11, -1} {
		wantSearchErr(t, cs, f, searchArgs("k", k), "k must be 1 to 10")
	}
	for _, q := range []string{"", "   \t\n", strings.Repeat("x", 501)} {
		wantSearchErr(t, cs, f, searchArgs("query", q), "query")
	}
	if isErr, msg := toolResult(t, cs, "search_documents", searchArgs("query", strings.Repeat("x", 500))); isErr {
		t.Fatalf("500-char query refused: %s", msg)
	}
}

func decodeOut(t *testing.T, cs *mcp.ClientSession, args map[string]any) []evidence.SearchDocumentsOutput {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_documents", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("search_documents: %v %v", err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if string(b) == "null" {
		t.Fatal("structured content is null, want an array")
	}
	var out []evidence.SearchDocumentsOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearchDocumentsSnippets(t *testing.T) {
	long := strings.Repeat("भाड़ा ₹९,००० देय है।  ", 400)
	f := &fakeSearcher{hits: []retrieval.Hit{
		{DocID: "a", DocType: "policy", Section: "s1", Title: "T", Content: long, RerankScore: 0.5},
		{DocID: "b", DocType: "contract", Section: "s2", Title: "U", Content: "  rent \n\n  due\t5th  ", RerankScore: -1.25},
		{DocID: "c", DocType: "policy", Content: strings.Repeat("a", 600)},
		{DocID: "d", DocType: "policy", Content: strings.Repeat("a", 601)},
	}}
	out := decodeOut(t, sharmaSearch(t, f), searchArgs())
	if len(out) != 4 {
		t.Fatalf("got %d hits, want 4", len(out))
	}
	r := []rune(out[0].Snippet)
	if len(r) != evidence.MaxSnippetChars || r[len(r)-1] != '…' {
		t.Errorf("long snippet: %d runes ending %q, want %d ending …", len(r), string(r[len(r)-1]), evidence.MaxSnippetChars)
	}
	if out[1].Snippet != "rent due 5th" || out[1].Score != "-1.25" || out[0].Score != "0.5" {
		t.Errorf("short hit = %+v", out[1])
	}
	if n := len([]rune(out[2].Snippet)); n != 600 || strings.Contains(out[2].Snippet, "…") {
		t.Errorf("exactly 600 runes must stay whole, got %d", n)
	}
	if n := len([]rune(out[3].Snippet)); n != 600 || !strings.HasSuffix(out[3].Snippet, "…") {
		t.Errorf("601 runes must be cut to 600 with …, got %d", n)
	}

	empty := decodeOut(t, sharmaSearch(t, &fakeSearcher{}), searchArgs())
	if empty == nil || len(empty) != 0 {
		t.Errorf("empty result = %#v, want []", empty)
	}
}

func TestSearchDocumentsDropsOtherCompanyHit(t *testing.T) {
	mehta, sharma := "mehta", "sharma"
	f := &fakeSearcher{hits: []retrieval.Hit{
		{DocID: "m", CompanyID: &mehta, Content: "mehta secret"},
		{DocID: "shared", CompanyID: nil, Content: "shared"},
		{DocID: "s", CompanyID: &sharma, Content: "sharma"},
	}}
	out := decodeOut(t, sharmaSearch(t, f), searchArgs())
	if len(out) != 2 || out[0].DocID != "shared" || out[1].DocID != "s" {
		t.Fatalf("hits = %+v, want the shared and sharma hits only", out)
	}
}

func TestSearchDocumentsUnconfiguredAndErrors(t *testing.T) {
	cs := scopedSession(t, searchServer(t, nil), runScope(t))
	isErr, msg := toolResult(t, cs, "search_documents", searchArgs())
	if !isErr || !strings.Contains(msg, "document search is not configured") || strings.Contains(msg, "panicked") {
		t.Fatalf("IsError=%v %q, want 'document search is not configured'", isErr, msg)
	}

	f := &fakeSearcher{err: context.DeadlineExceeded}
	isErr, msg = toolResult(t, sharmaSearch(t, f), "search_documents", searchArgs("query", "secret-query-text"))
	if !isErr || !strings.Contains(msg, "search documents") || strings.Contains(msg, "secret-query-text") {
		t.Fatalf("IsError=%v %q, want a wrapped error without the query", isErr, msg)
	}
}
