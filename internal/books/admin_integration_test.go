//go:build integration

package books

// Integration test for post_approved_journal_entry (CC-504b) against the
// local erp.localhost site and a test Postgres. It WRITES to the shared
// ERPNext: run it only while holding tmp/erpnext.lock. It posts small
// reclassifications between two Sharma Traders expense accounts dated
// 2026-07-31 (outside the seeded months), each carrying a fresh proposal
// UUID in copilot_ext_id, and cancels then deletes every entry it created.
// Run with the bot keys in the environment, never on argv:
//
//	sh -c 'set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/books/... -run TestPostApprovedIntegration -count=1 -v'
//
// Postgres comes from TEST_DATABASE_URL, or a testcontainers pgvector
// container when it is unset.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const (
	intDebitAccount  = "Miscellaneous Expenses - STPL"
	intCreditAccount = "Administrative Expenses - STPL"
	intPostingDate   = "2026-07-31"
	intChecker       = "checker@example.test"
)

// adminTestStore opens a migrated Postgres for the test.
func adminTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start testcontainers postgres: %v", err)
		}
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(pg); err != nil {
				t.Logf("terminate postgres: %v", err)
			}
		})
		if url, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
			t.Fatalf("postgres connection string: %v", err)
		}
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// intApproved creates an approved, unposted proposal for amount paise.
func intApproved(t *testing.T, st *store.Store, amount money.Paise) uuid.UUID {
	t.Helper()
	checker := intChecker
	decided := time.Now().UTC()
	p := store.JournalProposal{
		ID:        uuid.New(),
		CompanyID: sharmaID,
		Payload: store.JournalPayload{
			PostingDate: intPostingDate,
			Remark:      "CC-504b integration test reclassification",
			Lines: []store.JournalLine{
				{Account: intDebitAccount, DebitPaise: amount},
				{Account: intCreditAccount, CreditPaise: amount},
			},
		},
		Status:    ProposalApproved,
		Maker:     "maker@example.test",
		Checker:   &checker,
		DecidedAt: &decided,
	}
	if err := st.CreateJournalProposal(t.Context(), p); err != nil {
		t.Fatalf("CreateJournalProposal: %v", err)
	}
	return p.ID
}

// entriesFor lists the Journal Entries carrying id in copilot_ext_id, any
// docstatus.
func entriesFor(t *testing.T, c *frappe.Client, id uuid.UUID) []jeRef {
	t.Helper()
	rows, err := frappe.List[jeRef](t.Context(), c, journalEntry, frappe.Query{
		Fields: []string{"name", "docstatus"}, Filters: [][]any{{seed.ExtIDField, "=", id.String()}},
	})
	if err != nil {
		t.Fatalf("list journal entries for %s: %v", id, err)
	}
	return rows
}

func TestPostApprovedIntegration(t *testing.T) {
	cfg := integrationConfig(t)
	bot, err := frappe.New(cfg, cfg.ERPAPIKey.Reveal(), cfg.ERPAPISecret)
	if err != nil {
		t.Fatalf("frappe.New (bot): %v", err)
	}
	st := adminTestStore(t)
	profiles, err := seed.LoadProfiles(intProfile)
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}

	// Clean up every entry the test created: cancel, then delete, with the
	// bot key. A failure is logged, not hidden.
	var (
		createdMu sync.Mutex
		created   = map[string]bool{}
	)
	track := func(name string) {
		createdMu.Lock()
		defer createdMu.Unlock()
		created[name] = true
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for name := range created {
			doc, err := frappe.Get[map[string]any](ctx, bot, journalEntry, name)
			if err != nil {
				t.Logf("CLEANUP: get %s: %v", name, err)
				continue
			}
			if ds, _ := intField(doc["docstatus"]); ds == 1 {
				if _, err := frappe.Cancel(ctx, bot, journalEntry, name); err != nil {
					t.Logf("CLEANUP FAILED: cancel %s: %v", name, err)
					continue
				}
			}
			if err := frappe.Delete(ctx, bot, journalEntry, name); err != nil {
				t.Logf("CLEANUP FAILED: delete %s: %v (cancelled, left in ERPNext)", name, err)
				continue
			}
			t.Logf("cleanup: cancelled and deleted %s", name)
		}
	})

	// The binary's two routes, with random tokens for this test.
	testCfg := config.Config{
		MCPTokenAgent: config.NewSecret(randomToken(t)),
		MCPTokenAdmin: config.NewSecret(randomToken(t)),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminServer := mcpkit.NewServer("close-copilot-books-admin", "test")
	mcpkit.EnableAudit(adminServer, "books-admin", st)
	RegisterAdminTools(adminServer, AdminDeps{
		Client: bot, Companies: ProfileCompanies(profiles), Proposals: st, Now: time.Now, Logger: log,
	})
	readServer := mcpkit.NewServer("close-copilot-books", "test")
	RegisterTools(readServer, ToolDeps{Client: bot, Companies: ProfileCompanies(profiles), Now: time.Now, Logger: log})
	ts := httptest.NewServer(mcpkit.BuildHandler(map[string]mcpkit.Route{
		"/mcp":           mcpkit.NewAgentRoute(readServer, testCfg),
		mcpkit.AdminPath: mcpkit.NewAdminRoute(adminServer, testCfg),
	}, log, mcpkit.WithActors(map[string]string{"/mcp": "agent", mcpkit.AdminPath: "admin"})))
	t.Cleanup(ts.Close)

	adminSession := func(t *testing.T) *mcp.ClientSession {
		t.Helper()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "approvals-test", Version: "0"}, nil).Connect(t.Context(),
			&mcp.StreamableClientTransport{
				Endpoint:   ts.URL + mcpkit.AdminPath,
				HTTPClient: &http.Client{Transport: bearer{token: testCfg.MCPTokenAdmin.Reveal()}},
				MaxRetries: -1,
			}, nil)
		if err != nil {
			t.Fatalf("connect /mcp-admin: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	post := func(t *testing.T, cs *mcp.ClientSession, id uuid.UUID) PostApprovedOutput {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolPostApprovedJournalEntry,
			Arguments: map[string]any{"proposal_id": id.String()}})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("post %s refused: %s", id, res.Content[0].(*mcp.TextContent).Text)
		}
		var out PostApprovedOutput
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode output: %v", err)
		}
		if out.ERPName != "" {
			track(out.ERPName)
		}
		return out
	}

	t.Run("posts once as a submitted, balanced entry", func(t *testing.T) {
		const amount = money.Paise(123)
		id := intApproved(t, st, amount)
		cs := adminSession(t)
		out := post(t, cs, id)
		if out.AlreadyPosted || out.Status != ProposalPosted || out.ERPName == "" {
			t.Fatalf("output %+v", out)
		}

		doc, err := frappe.Get[map[string]any](t.Context(), bot, journalEntry, out.ERPName)
		if err != nil {
			t.Fatalf("get %s: %v", out.ERPName, err)
		}
		if ds, _ := intField(doc["docstatus"]); ds != 1 {
			t.Errorf("docstatus %v, want 1", doc["docstatus"])
		}
		if doc[seed.ExtIDField] != id.String() || doc["company"] != stplCompany || doc["posting_date"] != intPostingDate {
			t.Errorf("entry: ext id %v company %v date %v", doc[seed.ExtIDField], doc["company"], doc["posting_date"])
		}
		td, err1 := paiseField(doc["total_debit"])
		tc, err2 := paiseField(doc["total_credit"])
		if err1 != nil || err2 != nil || td != amount || tc != amount {
			t.Errorf("totals debit %v credit %v, want %d paise each", doc["total_debit"], doc["total_credit"], amount)
		}
		gl, err := frappe.List[map[string]any](t.Context(), bot, "GL Entry", frappe.Query{
			Fields:  []string{"name", "account", "debit", "credit"},
			Filters: [][]any{{"voucher_type", "=", journalEntry}, {"voucher_no", "=", out.ERPName}, {"is_cancelled", "=", 0}},
		})
		if err != nil {
			t.Fatalf("GL entries: %v", err)
		}
		if len(gl) != 2 {
			t.Errorf("%d GL entries, want 2", len(gl))
		}

		row, err := st.GetJournalProposal(t.Context(), id)
		if err != nil || row.Status != ProposalPosted || row.ERPName == nil || *row.ERPName != out.ERPName || row.PostedAt == nil {
			t.Errorf("proposal row %+v, %v", row, err)
		}

		again := post(t, cs, id)
		if !again.AlreadyPosted || again.ERPName != out.ERPName {
			t.Errorf("second call %+v, want already_posted with %s", again, out.ERPName)
		}
		if n := len(entriesFor(t, bot, id)); n != 1 {
			t.Errorf("%d entries carry the proposal ID, want 1", n)
		}

		rows, err := st.ListAuditLogsByActor(t.Context(), intChecker)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Server != nil && *r.Server == "books-admin" && r.Tool != nil && *r.Tool == ToolPostApprovedJournalEntry &&
				strings.Contains(string(r.Args), id.String()) {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%d books-admin audit rows name the checker for %s, want 2", n, id)
		}
	})

	t.Run("three concurrent posts make one entry", func(t *testing.T) {
		id := intApproved(t, st, 77)
		outs := make([]PostApprovedOutput, 3)
		sessions := []*mcp.ClientSession{adminSession(t), adminSession(t), adminSession(t)}
		var wg sync.WaitGroup
		for i := range outs {
			wg.Go(func() { outs[i] = post(t, sessions[i], id) })
		}
		wg.Wait()
		fresh := 0
		for i, o := range outs {
			if o.ERPName == "" || o.ERPName != outs[0].ERPName {
				t.Errorf("call %d erp_name %q, want %q", i, o.ERPName, outs[0].ERPName)
			}
			if !o.AlreadyPosted {
				fresh++
			}
		}
		if fresh != 1 {
			t.Errorf("%d calls posted fresh, want exactly 1", fresh)
		}
		if n := len(entriesFor(t, bot, id)); n != 1 {
			t.Errorf("%d entries carry the proposal ID, want 1", n)
		}
	})

	t.Run("a draft left by a crashed call is submitted, not duplicated", func(t *testing.T) {
		id := intApproved(t, st, 55)
		row, err := st.GetJournalProposal(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := frappe.Insert(t.Context(), bot, journalEntry, journalBody(row, stplCompany))
		if err != nil {
			t.Fatalf("insert draft: %v", err)
		}
		name, _ := draft["name"].(string)
		track(name)
		// Frappe's list API returns drafts without a docstatus filter.
		if refs := entriesFor(t, bot, id); len(refs) != 1 || refs[0].DocStatus != 0 {
			t.Fatalf("list by ext id: %+v, want the one draft", refs)
		}
		out := post(t, adminSession(t), id)
		if out.ERPName != name {
			t.Errorf("posted %s, want the draft %s", out.ERPName, name)
		}
		refs := entriesFor(t, bot, id)
		if len(refs) != 1 || refs[0].DocStatus != 1 {
			t.Errorf("entries after: %+v, want the one draft, submitted", refs)
		}
	})

	t.Run("the agent token gets 401 on /mcp-admin", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+mcpkit.AdminPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := bearer{token: testCfg.MCPTokenAgent.Reveal()}.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("agent token on /mcp-admin: %d, want 401", resp.StatusCode)
		}
	})
}
