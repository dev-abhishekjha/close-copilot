package books

// Contract tests for the books MCP server (CC-505). Unlike tools_test.go,
// which talks to the tools over in-memory transports, these go through the
// whole HTTP stack an agent sees: mcpkit.BuildHandler with its bearer-auth
// middleware behind httptest, the SDK's Streamable HTTP client, and the
// JSON Schemas the SDK derives from the tool structs. Every tool's listing
// is snapshotted under testdata/schemas, so a schema change shows up in
// review; run
//
//	go test ./internal/books -run TestContractBooksSchemas -update
//
// to rewrite the goldens after a deliberate change.

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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

var update = flag.Bool("update", false, "rewrite testdata/schemas golden files")

// contractToken is a made-up bearer token for the in-process server.
const contractToken = "contract-test-token"

// schemaDir holds one golden file per tool.
const schemaDir = "testdata/schemas"

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

// contractServer serves s at /mcp behind bearer auth with contractToken
// and returns the base URL.
func contractServer(t *testing.T, s *mcp.Server) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := mcpkit.BuildHandler(map[string]mcpkit.Route{"/mcp": mcpkit.NewRoute(s, contractToken)}, log)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
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

// contractBooks starts the books server over a fake ERPNext holding f's
// documents and returns the HTTP server and a connected session.
func contractBooks(t *testing.T, f *fakeERP) (*httptest.Server, *mcp.ClientSession) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "close-copilot-books", Version: "contract"}, nil)
	RegisterTools(s, ToolDeps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client: f.client(),
		Companies: ProfileCompanies([]seed.Profile{
			{ID: toolCompanyID, ERPCompany: testCompany},
			{ID: "other", ERPCompany: otherCompany},
		}),
		Now: func() time.Time { return toolNow },
	})
	srv := contractServer(t, s)
	cs, err := contractConnect(t, srv, contractToken)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return srv, cs
}

// contractERP is one synthetic ledger that every books tool has
// something to say about.
func contractERP(t *testing.T) *fakeERP {
	f := trialBalanceERP(t)
	intra := []doc{
		taxRow("tax-c", "Input Tax CGST - TT", "9", "4500.00", "cgst"),
		taxRow("tax-s", "Input Tax SGST - TT", "9", "4500.00", "sgst"),
	}
	f.add("Purchase Invoice",
		purchaseDoc("PINV-0001", testCompany, "SUP-A", "2026-09-05", 1, "50000.00", "59000.00", intra),
		purchaseDoc("PINV-R6", testCompany, "SUP-RENT", "2026-06-05", 1, "50000.00", "59000.00", nil),
		purchaseDoc("PINV-R7", testCompany, "SUP-RENT", "2026-07-05", 1, "50000.00", "59000.00", nil),
		purchaseDoc("PINV-R8", testCompany, "SUP-RENT", "2026-08-05", 1, "50000.00", "59000.00", nil),
	)
	f.add("Sales Invoice", salesDoc("SINV-0001", testCompany, "CUST-1", "2026-09-02", 1, "1180.00", "0"))
	f.add("Payment Entry",
		paymentDoc("PE-R1", testCompany, "Receive", "CUST-1", "2026-09-02", 1, "1180.00", "Payment Gateway Clearing - TT",
			refRow("r1", "Sales Invoice", "SINV-0001", "1180.00")),
		paymentDoc("PE-0001", testCompany, "Pay", "SUP-A", "2026-09-10", 1, "59000.00", "Creditors - TT",
			refRow("p1", "Purchase Invoice", "PINV-0001", "59000.00")),
	)
	// One page and one entry of the other company's ledger, for the
	// next_cursor round trip; none of it touches the test company.
	for i := range GLPageSize + 1 {
		f.add("GL Entry", glDoc(fmt.Sprintf("GLE-O-%04d", i), otherCompany, "Bank - OT", "1.00", "0", "2026-09-10", 1, 0))
	}
	return f
}

func TestContractBooks(t *testing.T) {
	srv, cs := contractBooks(t, contractERP(t))

	t.Run("list_tools", func(t *testing.T) {
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
		want := slices.Sorted(slices.Values(ToolNames))
		slices.Sort(names)
		if len(names) != 7 || !slices.Equal(names, want) {
			t.Errorf("tools = %v, want exactly %v", names, want)
		}
	})

	t.Run(ToolGetTrialBalance, func(t *testing.T) {
		var got TrialBalanceOutput
		call(t, cs, ToolGetTrialBalance, september(), &got)
		if got.Totals.Debit != 11850 || got.Totals.Credit != 11850 || len(got.Rows) != 3 {
			t.Errorf("trial balance = %+v, want 3 rows and 11850 paise each side", got)
		}
		if got.FromDate != "2026-09-01" || got.ToDate != "2026-09-30" || got.Company != toolCompanyID {
			t.Errorf("trial balance header = %s %s..%s", got.Company, got.FromDate, got.ToDate)
		}
	})

	t.Run(ToolListGLEntries, func(t *testing.T) {
		args := with(september(), "company", "other")
		var first GLEntriesOutput
		call(t, cs, ToolListGLEntries, args, &first)
		if len(first.Entries) != GLPageSize || first.NextCursor == "" {
			t.Fatalf("page 1: %d entries, next_cursor %q; want %d and a cursor", len(first.Entries), first.NextCursor, GLPageSize)
		}
		if e := first.Entries[0]; e.Name != "GLE-O-0000" || e.Debit != 100 || e.PostingDate != "2026-09-10" {
			t.Errorf("first entry = %+v, want GLE-O-0000 with 100 paise on 2026-09-10", e)
		}
		var second GLEntriesOutput
		call(t, cs, ToolListGLEntries, with(args, "cursor", first.NextCursor), &second)
		want := fmt.Sprintf("GLE-O-%04d", GLPageSize)
		if len(second.Entries) != 1 || second.Entries[0].Name != want || second.NextCursor != "" {
			t.Errorf("page 2 = %d entries (%+v), next_cursor %q; want only %s and no cursor",
				len(second.Entries), second.Entries, second.NextCursor, want)
		}
	})

	t.Run(ToolListPurchaseInvoices, func(t *testing.T) {
		var got PurchaseInvoicesOutput
		call(t, cs, ToolListPurchaseInvoices, september(), &got)
		if len(got.Invoices) != 1 {
			t.Fatalf("got %d invoices, want 1: %+v", len(got.Invoices), got.Invoices)
		}
		if p := got.Invoices[0]; p.Name != "PINV-0001" || p.Taxable != 5000000 || p.CGST != 450000 || p.SGST != 450000 || p.GrandTotal != 5900000 {
			t.Errorf("PINV-0001 = %+v, want taxable 5000000, CGST and SGST 450000, grand 5900000 paise", p)
		}
	})

	t.Run(ToolListSalesInvoices, func(t *testing.T) {
		var got SalesInvoicesOutput
		call(t, cs, ToolListSalesInvoices, september(), &got)
		if len(got.Invoices) != 1 {
			t.Fatalf("got %d invoices, want 1", len(got.Invoices))
		}
		if s := got.Invoices[0]; s.GrandTotal != 118000 || s.CollectedVia != "Payment Gateway Clearing - TT" {
			t.Errorf("SINV-0001 = %+v, want grand 118000 paise collected via Payment Gateway Clearing - TT", s)
		}
	})

	t.Run(ToolListPayments, func(t *testing.T) {
		var got PaymentsOutput
		call(t, cs, ToolListPayments, september(), &got)
		if len(got.Payments) != 2 {
			t.Fatalf("got %d payments, want 2", len(got.Payments))
		}
		if p := got.Payments[1]; p.Name != "PE-0001" || p.Amount != 5900000 || len(p.Invoices) != 1 || p.Invoices[0].ReferenceName != "PINV-0001" {
			t.Errorf("PE-0001 = %+v, want 5900000 paise settling PINV-0001", p)
		}
	})

	t.Run(ToolGetAccountHistory, func(t *testing.T) {
		var got AccountHistoryOutput
		call(t, cs, ToolGetAccountHistory, map[string]any{"company": toolCompanyID, "account": "Bank - TT", "months": 2}, &got)
		want := []MonthTotal{
			{Month: "2026-08", Debit: 100000, Net: 100000},
			{Month: "2026-09", Credit: 11850, Net: -11850},
		}
		if got.ThroughMonth != "2026-09" || !reflect.DeepEqual(got.History, want) {
			t.Errorf("history = %+v, want through 2026-09 %+v", got, want)
		}
	})

	t.Run(ToolListRecurringSuppliers, func(t *testing.T) {
		var got RecurringSuppliersOutput
		call(t, cs, ToolListRecurringSuppliers, map[string]any{
			"company": toolCompanyID, "before_month": "2026-09", "lookback_months": 3, "min_occurrences": 3,
		}, &got)
		if len(got.Suppliers) != 1 || got.Suppliers[0].Supplier != "SUP-RENT" || got.Suppliers[0].MedianAmount != 5900000 {
			t.Errorf("suppliers = %+v, want SUP-RENT with median 5900000 paise", got.Suppliers)
		}
	})

	t.Run("invalid_argument_is_tool_error", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want string
		}{
			{"handler check", with(september(), "from_date", "2026-9-01"), "from_date must be a date in YYYY-MM-DD form"},
			{"schema check", with(september(), "order_by", "name desc"), "order_by"},
		}
		for _, c := range cases {
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolListGLEntries, Arguments: c.args})
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

	t.Run("auth", func(t *testing.T) {
		assertUnauthorized(t, srv)
		if cs, err := contractConnect(t, srv, "wrong-token"); err == nil {
			_ = cs.Close()
			t.Error("connect with a wrong token succeeded")
		}
	})
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

func TestContractBooksSchemas(t *testing.T) {
	_, cs := contractBooks(t, newFakeERP(t))
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	checkSchemaGoldens(t, res.Tools, schemaDir)
}

// checkSchemaGoldens compares each listed tool, as JSON with sorted keys
// and 2-space indent, with dir/<tool>.json, and fails on a golden for a
// tool that is no longer listed. With -update it rewrites the goldens.
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
