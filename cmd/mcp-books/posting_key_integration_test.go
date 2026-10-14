//go:build integration

package main

// Integration test for the dedicated posting key (CC-1007) against the
// local erp.localhost site and a test Postgres. It WRITES to the shared
// ERPNext: run it only while holding tmp/erpnext.lock (it does not take the
// lock itself). It checks what the copilot-poster key may do over raw HTTP,
// posts one Journal Entry through post_approved_journal_entry on the
// /mcp-admin route httpRoutes builds, and checks the read tools on /mcp
// still use the bot key. Every Journal Entry it creates is dated 2026-07-31
// (outside the seeded months), carries a fresh UUID in copilot_ext_id and
// is cancelled, then deleted, with the seeder key at the end. Run with the
// keys in the environment, never on argv:
//
//	(set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./cmd/mcp-books/... -run TestPostingKeyIntegration -count=1 -v)
//
// Postgres comes from TEST_DATABASE_URL, or a testcontainers pgvector
// container when it is unset.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const (
	pkCompanyID     = "sharma"
	pkCompany       = "Sharma Traders Pvt Ltd"
	pkDebitAccount  = "Miscellaneous Expenses - STPL"
	pkCreditAccount = "Administrative Expenses - STPL"
	pkPostingDate   = "2026-07-31"
	pkPoster        = "copilot-poster@example.com"
	pkBot           = "copilot-bot@example.com"
	pkJournalEntry  = "Journal Entry"
)

// postingKeyConfig is testBooksConfig with the ERPNext pairs from the
// environment and random MCP tokens and scope key.
func postingKeyConfig(t *testing.T) config.Config {
	t.Helper()
	if v, ok := os.LookupEnv(config.EnvERPPostAPIKey); !ok || v == "" {
		t.Skipf("posting key integration test skipped: %s not set (run deploy/erpnext/api-users.sh and source tmp/erp-keys.env in a subshell)", config.EnvERPPostAPIKey)
	}
	cfg, err := config.Load(os.LookupEnv,
		config.EnvERPBaseURL, config.EnvERPSite,
		config.EnvERPAPIKey, config.EnvERPAPISecret,
		config.EnvERPSeedAPIKey, config.EnvERPSeedAPISecret,
		config.EnvERPPostAPIKey, config.EnvERPPostAPISecret)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return config.Config{
		ERPBaseURL:       cfg.ERPBaseURL,
		ERPSite:          cfg.ERPSite,
		ERPAPIKey:        cfg.ERPAPIKey,
		ERPAPISecret:     cfg.ERPAPISecret,
		ERPSeedAPIKey:    cfg.ERPSeedAPIKey,
		ERPSeedAPISecret: cfg.ERPSeedAPISecret,
		ERPPostAPIKey:    cfg.ERPPostAPIKey,
		ERPPostAPISecret: cfg.ERPPostAPISecret,
		MCPTokenAgent:    config.NewSecret(pkRandom(t)),
		MCPTokenAdmin:    config.NewSecret(pkRandom(t)),
		MCPScopeKey:      config.NewSecret(pkRandom(t)),
	}
}

func pkRandom(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// pkStore opens a migrated Postgres. Its absence fails the test.
func pkStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
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
		if dsn, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
			t.Fatalf("postgres connection string: %v", err)
		}
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// erpAnswer is what ERPNext answered to one raw request.
type erpAnswer struct {
	Status  int
	ExcType string
	Data    json.RawMessage
}

// erpRaw sends one request to ERPNext with the key pair, the way
// internal/frappe does (Authorization: token key:secret, Host: ERP_SITE).
// It never logs the header.
func erpRaw(t *testing.T, cfg config.Config, key, secret config.Secret, method, path string, body any) erpAnswer {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, cfg.ERPBaseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = cfg.ERPSite
	req.Header.Set("Authorization", "token "+key.Reveal()+":"+secret.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	var parsed struct {
		ExcType string          `json:"exc_type"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return erpAnswer{Status: resp.StatusCode, ExcType: parsed.ExcType, Data: parsed.Data}
}

func resourcePath(doctype string, name ...string) string {
	p := "/api/resource/" + url.PathEscape(doctype)
	for _, n := range name {
		p += "/" + url.PathEscape(n)
	}
	return p
}

// wantRefused fails unless ERPNext answered 403 with exc_type PermissionError.
func wantRefused(t *testing.T, what string, a erpAnswer) {
	t.Helper()
	if a.Status != http.StatusForbidden || a.ExcType != "PermissionError" {
		t.Errorf("%s with the posting key: HTTP %d %q, want 403 PermissionError", what, a.Status, a.ExcType)
	}
}

// pkJournal is a balanced two-line Journal Entry on the Sharma company.
func pkJournal(extID string, amount money.Paise, remark string) map[string]any {
	return map[string]any{
		"voucher_type":  pkJournalEntry,
		"company":       pkCompany,
		"posting_date":  pkPostingDate,
		"user_remark":   remark,
		seed.ExtIDField: extID,
		"accounts": []map[string]any{
			{"account": pkDebitAccount, "debit_in_account_currency": json.Number(amount.Rupees())},
			{"account": pkCreditAccount, "credit_in_account_currency": json.Number(amount.Rupees())},
		},
	}
}

// docName reads "name" from a /api/resource answer.
func docName(t *testing.T, a erpAnswer) string {
	t.Helper()
	var d struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(a.Data, &d); err != nil || d.Name == "" {
		t.Fatalf("no document name in the answer (HTTP %d %q)", a.Status, a.ExcType)
	}
	return d.Name
}

func TestPostingKeyIntegration(t *testing.T) {
	cfg := postingKeyConfig(t)
	seeder, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), cfg.ERPSeedAPISecret)
	if err != nil {
		t.Fatalf("frappe.New (seeder): %v", err)
	}
	poster, err := frappe.New(cfg, cfg.ERPPostAPIKey.Reveal(), cfg.ERPPostAPISecret)
	if err != nil {
		t.Fatalf("frappe.New (poster): %v", err)
	}
	st := pkStore(t)

	// Every Journal Entry the test created is cancelled (if submitted) and
	// deleted with the seeder key; a failure is logged, not hidden.
	var (
		createdMu sync.Mutex
		created   []string
	)
	track := func(name string) {
		createdMu.Lock()
		defer createdMu.Unlock()
		created = append(created, name)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, name := range created {
			doc, err := frappe.Get[map[string]any](ctx, seeder, pkJournalEntry, name)
			if err != nil {
				t.Logf("CLEANUP: get %s: %v", name, err)
				continue
			}
			if fmt.Sprint(doc["docstatus"]) == "1" {
				if _, err := frappe.Cancel(ctx, seeder, pkJournalEntry, name); err != nil {
					t.Logf("CLEANUP FAILED: cancel %s: %v", name, err)
					continue
				}
			}
			if err := frappe.Delete(ctx, seeder, pkJournalEntry, name); err != nil {
				t.Logf("CLEANUP FAILED: delete %s: %v", name, err)
				continue
			}
			t.Logf("cleanup: deleted %s with the seeder key", name)
		}
	})

	t.Run("the posting key reads Journal Entries and the bot still does", func(t *testing.T) {
		path := resourcePath(pkJournalEntry) + "?limit_page_length=1"
		if a := erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret, http.MethodGet, path, nil); a.Status != http.StatusOK {
			t.Errorf("Journal Entry list with the posting key: HTTP %d %q, want 200", a.Status, a.ExcType)
		}
		if a := erpRaw(t, cfg, cfg.ERPAPIKey, cfg.ERPAPISecret, http.MethodGet, path, nil); a.Status != http.StatusOK {
			t.Errorf("Journal Entry list with the bot key: HTTP %d %q, want 200 (the standard roles must keep their rights)", a.Status, a.ExcType)
		}
	})

	t.Run("the posting key can't read other transactions", func(t *testing.T) {
		for _, dt := range []string{"Sales Invoice", "Purchase Invoice", "Payment Entry", "GL Entry", "Bank Transaction"} {
			a := erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret, http.MethodGet, resourcePath(dt)+"?limit_page_length=1", nil)
			wantRefused(t, "GET "+dt+" list", a)
		}
	})

	t.Run("the posting key can't create outside Journal Entry", func(t *testing.T) {
		probes := map[string]map[string]any{
			"Sales Invoice": {"company": pkCompany, "posting_date": pkPostingDate},
			"Payment Entry": {"company": pkCompany, "posting_date": pkPostingDate, "payment_type": "Pay"},
			"Account":       {"account_name": "CC-1007 probe " + uuid.NewString()[:8], "company": pkCompany},
		}
		for dt, body := range probes {
			a := erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret, http.MethodPost, resourcePath(dt), body)
			if a.Status == http.StatusOK {
				var d struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(a.Data, &d)
				t.Errorf("insert %s with the posting key succeeded (%q): delete it in the desk", dt, d.Name)
				continue
			}
			wantRefused(t, "insert "+dt, a)
		}
	})

	t.Run("the posting key creates and submits, but can't cancel or delete", func(t *testing.T) {
		a := erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret, http.MethodPost, resourcePath(pkJournalEntry),
			pkJournal(uuid.NewString(), 101, "CC-1007 integration test: submitted entry"))
		if a.Status != http.StatusOK {
			t.Fatalf("insert Journal Entry with the posting key: HTTP %d %q, want 200", a.Status, a.ExcType)
		}
		name := docName(t, a)
		track(name)
		if _, err := frappe.Submit(t.Context(), poster, pkJournalEntry, name); err != nil {
			t.Fatalf("submit %s with the posting key: %v", name, err)
		}
		wantRefused(t, "cancel a submitted Journal Entry", erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret,
			http.MethodPost, "/api/method/frappe.client.cancel", map[string]any{"doctype": pkJournalEntry, "name": name}))
		wantRefused(t, "delete a submitted Journal Entry", erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret,
			http.MethodDelete, resourcePath(pkJournalEntry, name), nil))

		draft := erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret, http.MethodPost, resourcePath(pkJournalEntry),
			pkJournal(uuid.NewString(), 102, "CC-1007 integration test: draft entry"))
		if draft.Status != http.StatusOK {
			t.Fatalf("insert draft Journal Entry with the posting key: HTTP %d %q, want 200", draft.Status, draft.ExcType)
		}
		draftName := docName(t, draft)
		track(draftName)
		wantRefused(t, "delete a draft Journal Entry", erpRaw(t, cfg, cfg.ERPPostAPIKey, cfg.ERPPostAPISecret,
			http.MethodDelete, resourcePath(pkJournalEntry, draftName), nil))
	})

	t.Run("the bot key's Journal Entry insert (recorded, not asserted)", func(t *testing.T) {
		a := erpRaw(t, cfg, cfg.ERPAPIKey, cfg.ERPAPISecret, http.MethodPost, resourcePath(pkJournalEntry),
			pkJournal(uuid.NewString(), 103, "CC-1007 integration test: bot-key probe"))
		if a.Status == http.StatusOK {
			track(docName(t, a))
			t.Logf("BOT-KEY PROBE: ERPNext lets %s insert a draft Journal Entry (HTTP 200); narrowing the bot's role is an owner follow-up", pkBot)
		} else {
			t.Logf("BOT-KEY PROBE: ERPNext refused %s's Journal Entry insert: HTTP %d %q", pkBot, a.Status, a.ExcType)
		}
	})

	// The binary's routes.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	routes, opts, err := httpRoutes(cfg, testCompanies, log, st)
	if err != nil {
		t.Fatalf("httpRoutes: %v", err)
	}
	ts := httptest.NewServer(mcpkit.BuildHandler(routes, log, opts...))
	t.Cleanup(ts.Close)

	t.Run("post_approved_journal_entry posts as copilot-poster", func(t *testing.T) {
		const amount = money.Paise(104)
		checker := "checker@example.test"
		decided := time.Now().UTC()
		p := store.JournalProposal{
			ID:        uuid.New(),
			CompanyID: pkCompanyID,
			Payload: store.JournalPayload{
				PostingDate: pkPostingDate,
				Remark:      "CC-1007 integration test: posted through /mcp-admin",
				Lines: []store.JournalLine{
					{Account: pkDebitAccount, DebitPaise: amount},
					{Account: pkCreditAccount, CreditPaise: amount},
				},
			},
			Status:    books.ProposalApproved,
			Maker:     "maker@example.test",
			Checker:   &checker,
			DecidedAt: &decided,
		}
		if err := st.CreateJournalProposal(t.Context(), p); err != nil {
			t.Fatalf("CreateJournalProposal: %v", err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "approvals-test", Version: "0"}, nil).Connect(t.Context(),
			&mcp.StreamableClientTransport{
				Endpoint:   ts.URL + mcpkit.AdminPath,
				HTTPClient: &http.Client{Transport: scopeTransport{token: cfg.MCPTokenAdmin.Reveal()}},
				MaxRetries: -1,
			}, nil)
		if err != nil {
			t.Fatalf("connect /mcp-admin: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: books.ToolPostApprovedJournalEntry,
			Arguments: map[string]any{"proposal_id": p.ID.String()}})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("post refused: %s", res.Content[0].(*mcp.TextContent).Text)
		}
		var out books.PostApprovedOutput
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil || out.ERPName == "" {
			t.Fatalf("decode output %s: %v", b, err)
		}
		track(out.ERPName)
		doc, err := frappe.Get[map[string]any](t.Context(), seeder, pkJournalEntry, out.ERPName)
		if err != nil {
			t.Fatalf("get %s: %v", out.ERPName, err)
		}
		if doc["owner"] != pkPoster {
			t.Errorf("%s owner %v, want %s", out.ERPName, doc["owner"], pkPoster)
		}
		if fmt.Sprint(doc["docstatus"]) != "1" || doc[seed.ExtIDField] != p.ID.String() {
			t.Errorf("%s: docstatus %v ext id %v, want 1 and %s", out.ERPName, doc["docstatus"], doc[seed.ExtIDField], p.ID)
		}
		t.Logf("posted %s as %v", out.ERPName, doc["owner"])
	})

	t.Run("the read tools on /mcp still work with the bot key", func(t *testing.T) {
		sc, err := mcpkit.Window(pkCompanyID, "run-cc1007-int", "2026-09", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		header, err := mcpkit.Mint([]byte(cfg.MCPScopeKey.Reveal()), sc)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent-test", Version: "0"}, nil).Connect(t.Context(),
			&mcp.StreamableClientTransport{
				Endpoint:   ts.URL + "/mcp",
				HTTPClient: &http.Client{Transport: scopeTransport{token: cfg.MCPTokenAgent.Reveal(), scope: header}},
				MaxRetries: -1,
			}, nil)
		if err != nil {
			t.Fatalf("connect /mcp: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: books.ToolGetTrialBalance,
			Arguments: map[string]any{"company": pkCompanyID, "from_date": "2026-09-01", "to_date": "2026-09-30"}})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Errorf("%s refused: %s", books.ToolGetTrialBalance, res.Content[0].(*mcp.TextContent).Text)
		}
	})
}
