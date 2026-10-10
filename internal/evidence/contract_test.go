package evidence_test

// Contract tests for the evidence MCP server (CC-505). They go through the
// whole HTTP stack an agent sees: mcpkit.BuildHandler with its bearer-auth
// middleware behind httptest, the SDK's Streamable HTTP client, and the
// JSON Schemas the SDK derives from the tool structs.
//
// TestContractEvidenceSchemas needs no database (listing tools never
// touches the store), so the schema goldens are checked by every
// go test run. TestContractEvidence calls the tools over a real Postgres;
// it runs with -tags=integration, where tools_integration_test.go sets
// contractStore, and skips otherwise. After a deliberate schema change run
//
//	go test ./internal/evidence -run TestContractEvidenceSchemas -update

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/schemas golden files")

// contractStore returns a migrated store with the company profiles seeded.
// It is set only in integration builds (tools_integration_test.go).
var contractStore func(t *testing.T) *store.Store

// contractToken is a made-up bearer token for the in-process server.
const contractToken = "contract-test-token"

// schemaDir holds one golden file per tool.
const schemaDir = "testdata/schemas"

// evidenceTools are the tools RegisterTools adds.
var evidenceTools = []string{"list_bank_lines", "list_gstr2b_entries"}

// bearerTransport adds a fixed Authorization header to every request.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// contractEvidence serves the evidence tools over st (nil is fine for
// listing) at /mcp behind bearer auth and returns the server and a
// connected session.
func contractEvidence(t *testing.T, st *store.Store) (*httptest.Server, *mcp.ClientSession) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-evidence", Version: "contract"}, nil)
	evidence.RegisterTools(s, st)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, contractToken)}, log))
	t.Cleanup(srv.Close)
	cs, err := contractConnect(t, srv, contractToken)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return srv, cs
}

// contractConnect connects the SDK client to srv's /mcp with token.
func contractConnect(t *testing.T, srv *httptest.Server, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "v0"}, nil)
	return client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: &bearerTransport{base: srv.Client().Transport, token: token}},
		MaxRetries: -1,
	}, nil)
}

func TestContractEvidenceSchemas(t *testing.T) {
	srv, cs := contractEvidence(t, nil)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: not annotated read-only", tool.Name)
		}
		if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
			t.Errorf("%s: annotated destructive", tool.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, evidenceTools) {
		t.Errorf("tools = %v, want exactly %v", names, evidenceTools)
	}
	checkSchemaGoldens(t, res.Tools, schemaDir)

	assertUnauthorized(t, srv)
	if cs, err := contractConnect(t, srv, "wrong-token"); err == nil {
		_ = cs.Close()
		t.Error("connect with a wrong token succeeded")
	}
}

func date(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func ptr[T any](v T) *T { return &v }

// syntheticGSTIN is a checksum-valid GSTIN built from a seeded synthetic
// PAN, never a real one.
func syntheticGSTIN(t *testing.T, state, key string) string {
	t.Helper()
	g, err := seed.GSTIN(state, seed.PAN(505, key, 'C'))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestContractEvidence(t *testing.T) {
	if contractStore == nil {
		t.Skip("needs Postgres: run with -tags=integration")
	}
	st := contractStore(t)
	ctx := t.Context()

	const co = "mehta"
	lines := []store.BankLine{
		{CompanyID: co, TxnID: "CT-0901-001", TxnDate: date("2026-09-01"), Narration: "NEFT RENT SEP", Ref: ptr("N0901"),
			AmountPaise: -15000000, BalancePaise: ptr(money.Paise(85000000)), SourceFile: "contract.csv"},
		{CompanyID: co, TxnID: "CT-0914-002", TxnDate: date("2026-09-14"), Narration: "SMS CHARGES INCL GST",
			AmountPaise: -118000, BalancePaise: ptr(money.Paise(84882000)), SourceFile: "contract.csv"},
		{CompanyID: co, TxnID: "CT-0920-003", TxnDate: date("2026-09-20"), Narration: "UPI COLLECTION", Ref: ptr("U0920"),
			AmountPaise: 2500000, BalancePaise: ptr(money.Paise(87382000)), SourceFile: "contract.csv"},
		{CompanyID: co, TxnID: "CT-1001-004", TxnDate: date("2026-10-01"), Narration: "NEFT RENT OCT",
			AmountPaise: -15000000, SourceFile: "contract.csv"}, // after the range
		{CompanyID: "sharma", TxnID: "CT-0905-999", TxnDate: date("2026-09-05"), Narration: "OTHER COMPANY",
			AmountPaise: -99999999, SourceFile: "contract.csv"}, // another company's line
	}
	if err := st.UpsertBankLines(ctx, lines); err != nil {
		t.Fatalf("UpsertBankLines: %v", err)
	}
	gstinA := syntheticGSTIN(t, "27", "contract-supplier-a")
	gstinB := syntheticGSTIN(t, "29", "contract-supplier-b")
	entries := []store.GSTR2BEntry{
		{CompanyID: co, Period: "2026-09", SupplierGSTIN: gstinA, SupplierName: ptr("Contract Supplier A"),
			InvoiceNo: "CA/26-27/0042", InvoiceNoNorm: "CA26270042", InvoiceDate: date("2026-09-05"),
			TaxablePaise: 1000000, IGSTPaise: 180000, ITCAvailable: true},
		{CompanyID: co, Period: "2026-09", SupplierGSTIN: gstinB,
			InvoiceNo: "B-0007", InvoiceNoNorm: "B0007", InvoiceDate: date("2026-09-10"),
			TaxablePaise: 2000050, CGSTPaise: 180005, SGSTPaise: 180005, ITCAvailable: false},
		{CompanyID: co, Period: "2026-08", SupplierGSTIN: gstinA,
			InvoiceNo: "CA/26-27/0031", InvoiceNoNorm: "CA26270031", InvoiceDate: date("2026-08-05"),
			TaxablePaise: 1000000, IGSTPaise: 180000, ITCAvailable: true}, // another period
	}
	if err := st.UpsertGSTR2BEntries(ctx, entries); err != nil {
		t.Fatalf("UpsertGSTR2BEntries: %v", err)
	}

	_, cs := contractEvidence(t, st)
	september := map[string]any{"company": co, "from_date": "2026-09-01", "to_date": "2026-09-30"}

	t.Run("list_bank_lines", func(t *testing.T) {
		var got []evidence.BankLineOutput
		call(t, cs, "list_bank_lines", september, &got)
		if ids := txnIDs(got); !slices.Equal(ids, []string{"CT-0901-001", "CT-0914-002", "CT-0920-003"}) {
			t.Fatalf("txn_ids = %v, want mehta's three September lines in date order", ids)
		}
		c := got[1]
		if c.Amount != -118000 || c.Date != "2026-09-14" || c.Narration != "SMS CHARGES INCL GST" || c.Ref != nil {
			t.Errorf("charge line = %+v, want -118000 paise on 2026-09-14 without a ref", c)
		}
		if got[0].Balance == nil || *got[0].Balance != 85000000 || got[0].Ref == nil || *got[0].Ref != "N0901" {
			t.Errorf("first line = %+v, want balance 85000000 paise and ref N0901", got[0])
		}
	})

	t.Run("list_bank_lines_min_amount", func(t *testing.T) {
		var got []evidence.BankLineOutput
		args := map[string]any{"company": co, "from_date": "2026-09-01", "to_date": "2026-09-30", "min_amount": 1000000}
		call(t, cs, "list_bank_lines", args, &got)
		if ids := txnIDs(got); !slices.Equal(ids, []string{"CT-0901-001", "CT-0920-003"}) {
			t.Errorf("min_amount 1000000: txn_ids = %v, want the two lines of at least 1000000 paise either way", ids)
		}
	})

	t.Run("list_gstr2b_entries", func(t *testing.T) {
		var got []evidence.GSTR2BEntryOutput
		call(t, cs, "list_gstr2b_entries", map[string]any{"company": co, "period": "2026-09"}, &got)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want the two of 2026-09: %+v", len(got), got)
		}
		var b *evidence.GSTR2BEntryOutput
		for i := range got {
			if got[i].SupplierGSTIN == gstinB {
				b = &got[i]
			}
		}
		if b == nil || b.Taxable != 2000050 || b.CGST != 180005 || b.SGST != 180005 || b.IGST != 0 || b.ITCAvailable || b.InvoiceDate != "2026-09-10" {
			t.Errorf("supplier B entry = %+v, want taxable 2000050, CGST and SGST 180005 paise, no ITC, dated 2026-09-10", b)
		}
	})

	t.Run("list_gstr2b_entries_supplier_gstin", func(t *testing.T) {
		var got []evidence.GSTR2BEntryOutput
		call(t, cs, "list_gstr2b_entries", map[string]any{"company": co, "period": "2026-09", "supplier_gstin": gstinA}, &got)
		if len(got) != 1 || got[0].SupplierGSTIN != gstinA || got[0].InvoiceNo != "CA/26-27/0042" || got[0].IGST != 180000 {
			t.Errorf("supplier_gstin filter = %+v, want only CA/26-27/0042 with IGST 180000 paise", got)
		}
	})

	t.Run("invalid_argument_is_tool_error", func(t *testing.T) {
		cases := []struct {
			name, tool string
			args       map[string]any
			want       string
		}{
			{"bad from_date", "list_bank_lines", map[string]any{"company": co, "from_date": "2026-9-01", "to_date": "2026-09-30"}, "from_date"},
			{"bad period", "list_gstr2b_entries", map[string]any{"company": co, "period": "2026-13"}, "period"},
			{"unknown property", "list_bank_lines", map[string]any{"company": co, "from_date": "2026-09-01", "to_date": "2026-09-30", "sql": "1=1"}, "sql"},
		}
		for _, c := range cases {
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
			if err != nil {
				t.Fatalf("%s: transport/protocol error %v, want a tool error", c.name, err)
			}
			if !res.IsError {
				t.Fatalf("%s: IsError false, want a tool error", c.name)
			}
			if msg := resultText(res); !strings.Contains(msg, c.want) {
				t.Errorf("%s: error %q, want it to mention %q", c.name, msg, c.want)
			}
		}
	})
}

func txnIDs(lines []evidence.BankLineOutput) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.TxnID)
	}
	return out
}

// call invokes a tool and decodes its structured output into out, failing
// on a transport or tool error.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", tool, resultText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal structured content: %v", tool, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, b, err)
	}
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// assertUnauthorized posts an initialize request without a token and
// wants HTTP 401 with a Bearer challenge.
func assertUnauthorized(t *testing.T, srv *httptest.Server) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"anon","version":"0"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}
	if ch := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Bearer") {
		t.Errorf("no token: WWW-Authenticate %q, want a Bearer challenge", ch)
	}
}

// checkSchemaGoldens compares each listed tool, as JSON with sorted keys
// and 2-space indent, with dir/<tool>.json, and fails on a golden for a
// tool that is no longer listed. With -update it rewrites the goldens.
// (A copy of the books package's helper; test files can't share code
// across packages.)
func checkSchemaGoldens(t *testing.T, tools []*mcp.Tool, dir string) {
	t.Helper()
	listed := map[string]bool{}
	for _, tool := range tools {
		listed[tool.Name+".json"] = true
		got := schemaJSON(t, tool)
		path := filepath.Join(dir, tool.Name+".json")
		if *update {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read golden (run with -update to create it): %v", tool.Name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: schema differs from %s (- golden, + listed); if the change is deliberate, rerun with -update and review the diff:\n%s",
				tool.Name, path, lineDiff(string(want), string(got)))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !listed[e.Name()] {
			if *update {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					t.Fatal(err)
				}
				continue
			}
			t.Errorf("golden %s has no listed tool; remove it or rerun with -update", e.Name())
		}
	}
}

// schemaJSON renders a listed tool (name, description, annotations, input
// and output schema) with sorted keys and 2-space indent.
func schemaJSON(t *testing.T, tool *mcp.Tool) []byte {
	t.Helper()
	raw, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("%s: marshal: %v", tool.Name, err)
	}
	var m map[string]any // maps marshal with sorted keys
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: unmarshal: %v", tool.Name, err)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("%s: indent: %v", tool.Name, err)
	}
	return append(out, '\n')
}

// lineDiff is a minimal line diff of want and got: the changed lines of a
// longest-common-subsequence alignment, each with its line number in the
// golden (-) or in the listing (+).
func lineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			i++
			j++
		case j == len(b) || (i < len(a) && lcs[i+1][j] >= lcs[i][j+1]):
			fmt.Fprintf(&sb, "golden %4d - %s\n", i+1, a[i])
			i++
		default:
			fmt.Fprintf(&sb, "listed %4d + %s\n", j+1, b[j])
			j++
		}
	}
	return sb.String()
}
