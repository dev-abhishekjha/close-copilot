//go:build integration

package approvals

// End-to-end maker-checker test (CC-1004) against the local erp.localhost
// site and a test Postgres. It WRITES to the shared ERPNext: run it only
// while holding tmp/erpnext.lock (it does not take the lock itself). It
// stands up the books /mcp-admin route as cmd/mcp-books does after CC-1007
// (ERPNext client from the copilot-poster pair), the approvals service and
// poster over it, and the web server with asha (maker) and ravi (checker).
// Every Journal Entry is dated 2026-07-31 (outside the seeded months),
// carries a fresh proposal UUID in copilot_ext_id and is cancelled, then
// deleted, with the seeder key at the end. Run with the keys in the
// environment, never on argv:
//
//	(set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/approvals/... -run TestMakerCheckerIntegration -count=1 -v)
//
// Postgres comes from TEST_DATABASE_URL, or a testcontainers pgvector
// container when it is unset.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web"
)

const (
	itCompanyID     = "sharma"
	itCompany       = "Sharma Traders Pvt Ltd"
	itDebitAccount  = "Miscellaneous Expenses - STPL"
	itCreditAccount = "Administrative Expenses - STPL"
	itPostingDate   = "2026-07-31"
	itJournalEntry  = "Journal Entry"
	itSessionKey    = "0123456789abcdef0123456789abcdef"
	itPassword      = "integration-test-password-1"
)

// itConfig is the ERPNext pairs from the environment plus random MCP
// tokens. It skips the test when the posting pair is missing.
func itConfig(t *testing.T) config.Config {
	t.Helper()
	if v, ok := os.LookupEnv(config.EnvERPPostAPIKey); !ok || v == "" {
		t.Skipf("maker-checker integration test skipped: %s not set (source tmp/erp-keys.env in a subshell)", config.EnvERPPostAPIKey)
	}
	cfg, err := config.Load(os.LookupEnv,
		config.EnvERPBaseURL, config.EnvERPSite,
		config.EnvERPSeedAPIKey, config.EnvERPSeedAPISecret,
		config.EnvERPPostAPIKey, config.EnvERPPostAPISecret)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return config.Config{
		ERPBaseURL:       cfg.ERPBaseURL,
		ERPSite:          cfg.ERPSite,
		ERPSeedAPIKey:    cfg.ERPSeedAPIKey,
		ERPSeedAPISecret: cfg.ERPSeedAPISecret,
		ERPPostAPIKey:    cfg.ERPPostAPIKey,
		ERPPostAPISecret: cfg.ERPPostAPISecret,
		MCPTokenAgent:    config.NewSecret(itRandom(t)),
		MCPTokenAdmin:    config.NewSecret(itRandom(t)),
	}
}

func itRandom(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// itStore opens a migrated Postgres. Its absence fails the test.
func itStore(t *testing.T) *store.Store {
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

// itCompanies resolves the synthetic Sharma company.
type itCompanies struct{}

func (itCompanies) ERPCompany(id string) (string, error) {
	if id == itCompanyID {
		return itCompany, nil
	}
	return "", fmt.Errorf("unknown company %q", id)
}

// itBackend adapts the service to web.ApprovalsBackend, as cmd/agent does.
type itBackend struct{ svc *Service }

func itErr(err error) error {
	var e *Error
	if err == nil || !errors.As(err, &e) {
		return err
	}
	kinds := []struct{ from, to error }{
		{ErrNotFound, web.ErrApprovalNotFound},
		{ErrInvalidProposal, web.ErrApprovalInvalid},
		{ErrInvalidReason, web.ErrApprovalInvalid},
		{ErrSelfApproval, web.ErrApprovalForbidden},
		{ErrNotProposable, web.ErrApprovalConflict},
		{ErrProposalExists, web.ErrApprovalConflict},
		{ErrNotPending, web.ErrApprovalConflict},
		{ErrPostFailed, web.ErrApprovalPostFailed},
	}
	for _, k := range kinds {
		if errors.Is(e, k.from) {
			return &web.ApprovalError{Kind: k.to, Message: e.Msg}
		}
	}
	return err
}

func (b itBackend) Propose(ctx context.Context, actor string, id uuid.UUID) (store.JournalProposal, error) {
	p, err := b.svc.Propose(ctx, actor, id)
	return p, itErr(err)
}

func (b itBackend) Approve(ctx context.Context, actor string, id uuid.UUID) (web.ApproveResult, error) {
	r, err := b.svc.Approve(ctx, actor, id)
	if err != nil {
		return web.ApproveResult{}, itErr(err)
	}
	return web.ApproveResult{ProposalID: r.ProposalID, Status: r.Status, ERPName: r.ERPName, AlreadyPosted: r.AlreadyPosted}, nil
}

func (b itBackend) Reject(ctx context.Context, actor string, id uuid.UUID, reason string) error {
	return itErr(b.svc.Reject(ctx, actor, id, reason))
}

func (b itBackend) Pending(ctx context.Context) ([]store.JournalProposal, error) {
	return b.svc.Pending(ctx)
}

func (b itBackend) Get(ctx context.Context, id uuid.UUID) (store.JournalProposal, error) {
	p, err := b.svc.Get(ctx, id)
	return p, itErr(err)
}

// itUsers is asha and ravi with bearer tokens; ashaRole is asha's role.
func itUsers(t *testing.T, ashaRole web.Role) web.Users {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(itPassword), web.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	tok := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	us, err := web.NewUsers(
		web.User{Name: "asha", Role: ashaRole, PasswordHash: string(h), TokenSHA256: tok("it-tok-asha")},
		web.User{Name: "ravi", Role: web.RoleChecker, PasswordHash: string(h), TokenSHA256: tok("it-tok-ravi")},
	)
	if err != nil {
		t.Fatal(err)
	}
	return us
}

// itFinding stores a run and an open, verified finding whose preview is a
// balanced Sharma entry of amount paise.
func itFinding(t *testing.T, st *store.Store, amount money.Paise, remark string) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	run := store.CloseRun{ID: uuid.New(), CompanyID: itCompanyID, Month: "2026-07", Status: store.RunDone}
	if err := st.CreateCloseRun(ctx, run); err != nil {
		t.Fatalf("CreateCloseRun: %v", err)
	}
	fid := uuid.New()
	f := store.Finding{
		ID: fid, RunID: run.ID, Type: "unrecorded_bank_charge", Severity: "low", Title: "CC-1004 integration finding",
		AmountPaise: &amount, Keys: map[string]string{}, Status: "open", Verified: true,
		Proposal: &store.JournalProposal{FindingID: &fid, CompanyID: itCompanyID, Status: StatusProposed, Payload: store.JournalPayload{
			PostingDate: itPostingDate, Remark: remark, Lines: []store.JournalLine{
				{Account: itDebitAccount, DebitPaise: amount},
				{Account: itCreditAccount, CreditPaise: amount},
			},
		}},
	}
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	return fid
}

type itResp struct {
	Status int
	Body   map[string]any
	Raw    string
}

func TestMakerCheckerIntegration(t *testing.T) {
	cfg := itConfig(t)
	seeder, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), cfg.ERPSeedAPISecret)
	if err != nil {
		t.Fatalf("frappe.New (seeder): %v", err)
	}
	posterClient, err := frappe.New(cfg, cfg.ERPPostAPIKey.Reveal(), cfg.ERPPostAPISecret)
	if err != nil {
		t.Fatalf("frappe.New (poster): %v", err)
	}
	st := itStore(t)

	// Every proposal ID the test makes; at the end each Journal Entry
	// carrying one is cancelled (if submitted) and deleted with the seeder
	// key. A failure is logged, not hidden.
	var (
		idsMu sync.Mutex
		ids   []string
	)
	trackID := func(id string) {
		idsMu.Lock()
		defer idsMu.Unlock()
		ids = append(ids, id)
	}
	entriesFor := func(ctx context.Context, id string) ([]map[string]any, error) {
		return frappe.List[map[string]any](ctx, seeder, itJournalEntry, frappe.Query{
			Fields:  []string{"name", "docstatus"},
			Filters: [][]any{{seed.ExtIDField, "=", id}},
		})
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, id := range ids {
			rows, err := entriesFor(ctx, id)
			if err != nil {
				t.Logf("CLEANUP FAILED: list entries of %s: %v", id, err)
				continue
			}
			for _, r := range rows {
				name := fmt.Sprint(r["name"])
				if fmt.Sprint(r["docstatus"]) == "1" {
					if _, err := frappe.Cancel(ctx, seeder, itJournalEntry, name); err != nil {
						t.Logf("CLEANUP FAILED: cancel %s: %v", name, err)
						continue
					}
				}
				if err := frappe.Delete(ctx, seeder, itJournalEntry, name); err != nil {
					t.Logf("CLEANUP FAILED: delete %s: %v", name, err)
					continue
				}
				t.Logf("cleanup: cancelled and deleted %s with the seeder key", name)
			}
		}
	})

	// The books /mcp-admin route as cmd/mcp-books builds it after CC-1007.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	adminServer := mcpkit.NewServer("close-copilot-books-admin", "test")
	mcpkit.EnableAudit(adminServer, "books-admin", st)
	books.RegisterAdminTools(adminServer, books.AdminDeps{
		Client: posterClient, Companies: itCompanies{}, Proposals: st, Now: time.Now, Logger: log,
	})
	routes := map[string]mcpkit.Route{mcpkit.AdminPath: mcpkit.NewAdminRoute(adminServer, cfg)}
	booksSrv := httptest.NewServer(mcpkit.BuildHandler(routes, log, mcpkit.WithActors(map[string]string{mcpkit.AdminPath: "admin"})))
	t.Cleanup(booksSrv.Close)

	// The approvals service and the web server.
	pcfg := cfg
	pcfg.BooksMCPURL = booksSrv.URL + "/mcp"
	poster, err := NewPoster(pcfg, log)
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}
	svc := New(Deps{Store: st, Poster: poster, Findings: st, Logger: log})
	srv, err := web.NewServer(web.Options{
		SessionKey: config.NewSecret(itSessionKey), ERPBaseURL: cfg.ERPBaseURL,
		Runs:      &web.RunsBackend{Store: st, Execute: func(context.Context, uuid.UUID) error { return nil }, Findings: st},
		Approvals: itBackend{svc: svc},
	}, itUsers(t, web.RoleMaker), log)
	if err != nil {
		t.Fatalf("web.NewServer: %v", err)
	}
	call := func(token, path, body string) itResp {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		out := itResp{Status: w.Code, Raw: w.Body.String()}
		_ = json.Unmarshal(w.Body.Bytes(), &out.Body)
		return out
	}
	countEntries := func(id string) int {
		t.Helper()
		rows, err := entriesFor(t.Context(), id)
		if err != nil {
			t.Fatalf("list entries of %s: %v", id, err)
		}
		return len(rows)
	}

	// Asha proposes; the maker and checker fields of the body are ignored.
	findingID := itFinding(t, st, 1004, "CC-1004 integration test: maker-checker post")
	res := call("it-tok-asha", "/api/findings/"+findingID.String()+"/proposals", `{"maker":"ravi","checker":"asha"}`)
	if res.Status != http.StatusCreated {
		t.Fatalf("propose = %d %s", res.Status, res.Raw)
	}
	proposalID, _ := res.Body["proposal_id"].(string)
	pid, err := uuid.Parse(proposalID)
	if err != nil {
		t.Fatalf("proposal id %q: %v", proposalID, err)
	}
	trackID(proposalID)
	row, err := st.GetJournalProposal(t.Context(), pid)
	if err != nil || row.Maker != "asha" || row.Status != StatusProposed || row.CompanyID != itCompanyID {
		t.Fatalf("proposal row %+v, %v", row, err)
	}
	if res := call("it-tok-asha", "/api/findings/"+findingID.String()+"/proposals", ""); res.Status != http.StatusConflict {
		t.Errorf("second propose = %d %s", res.Status, res.Raw)
	}

	t.Run("asha can't approve her own proposal", func(t *testing.T) {
		// As a maker, the role check refuses her.
		if res := call("it-tok-asha", "/api/proposals/"+proposalID+"/approve", ""); res.Status != http.StatusForbidden {
			t.Errorf("maker approve = %d %s", res.Status, res.Raw)
		}
		// Made a checker since, the service refuses her with the fixed message.
		if err := srv.SetUsers(itUsers(t, web.RoleChecker)); err != nil {
			t.Fatal(err)
		}
		res := call("it-tok-asha", "/api/proposals/"+proposalID+"/approve", `{"checker":"ravi"}`)
		if err := srv.SetUsers(itUsers(t, web.RoleMaker)); err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusForbidden || res.Body["error"] != "you proposed this entry, so a different checker must approve it" {
			t.Errorf("self approve = %d %s", res.Status, res.Raw)
		}
		if n := countEntries(proposalID); n != 0 {
			t.Errorf("%d Journal Entries after the refused approve, want 0", n)
		}
		if row, _ := st.GetJournalProposal(t.Context(), pid); row.Status != StatusProposed || row.Checker != nil {
			t.Errorf("row after refusal %+v", row)
		}
	})

	var erpName string
	t.Run("ravi approves and the entry is posted", func(t *testing.T) {
		res := call("it-tok-ravi", "/api/proposals/"+proposalID+"/approve", "")
		if res.Status != http.StatusOK || res.Body["status"] != "posted" {
			t.Fatalf("approve = %d %s", res.Status, res.Raw)
		}
		erpName, _ = res.Body["erp_name"].(string)
		if erpName == "" || !strings.Contains(fmt.Sprint(res.Body["erp_link"]), "/app/journal-entry/") {
			t.Errorf("approve body %v", res.Body)
		}
		doc, err := frappe.Get[map[string]any](t.Context(), seeder, itJournalEntry, erpName)
		if err != nil {
			t.Fatalf("get %s: %v", erpName, err)
		}
		if fmt.Sprint(doc["docstatus"]) != "1" || doc[seed.ExtIDField] != proposalID || doc["company"] != itCompany {
			t.Errorf("%s: docstatus %v ext id %v company %v", erpName, doc["docstatus"], doc[seed.ExtIDField], doc["company"])
		}
		if fmt.Sprint(doc["total_debit"]) != fmt.Sprint(doc["total_credit"]) {
			t.Errorf("%s unbalanced: %v / %v", erpName, doc["total_debit"], doc["total_credit"])
		}
		row, err := st.GetJournalProposal(t.Context(), pid)
		if err != nil || row.Status != StatusPosted || row.ERPName == nil || *row.ERPName != erpName ||
			row.Checker == nil || *row.Checker != "ravi" || row.PostedAt == nil {
			t.Errorf("row after post %+v, %v", row, err)
		}
	})

	t.Run("approving again posts nothing new", func(t *testing.T) {
		if res := call("it-tok-ravi", "/api/proposals/"+proposalID+"/approve", ""); res.Status != http.StatusOK || res.Body["erp_name"] != erpName {
			t.Errorf("second approve = %d %s", res.Status, res.Raw)
		}
		var wg sync.WaitGroup
		results := make([]itResp, 3)
		for i := range results {
			wg.Go(func() { results[i] = call("it-tok-ravi", "/api/proposals/"+proposalID+"/approve", "") })
		}
		wg.Wait()
		for i, r := range results {
			if r.Status != http.StatusOK || r.Body["erp_name"] != erpName {
				t.Errorf("concurrent approve %d = %d %s", i, r.Status, r.Raw)
			}
		}
		if n := countEntries(proposalID); n != 1 {
			t.Errorf("%d Journal Entries carry %s, want 1", n, proposalID)
		}
	})

	t.Run("concurrent first approvals post once", func(t *testing.T) {
		fid := itFinding(t, st, 1005, "CC-1004 integration test: concurrent approvals")
		res := call("it-tok-asha", "/api/findings/"+fid.String()+"/proposals", "")
		if res.Status != http.StatusCreated {
			t.Fatalf("propose = %d %s", res.Status, res.Raw)
		}
		id, _ := res.Body["proposal_id"].(string)
		trackID(id)
		var wg sync.WaitGroup
		results := make([]itResp, 3)
		for i := range results {
			wg.Go(func() { results[i] = call("it-tok-ravi", "/api/proposals/"+id+"/approve", "") })
		}
		wg.Wait()
		name := ""
		for i, r := range results {
			if r.Status != http.StatusOK {
				t.Errorf("approve %d = %d %s", i, r.Status, r.Raw)
				continue
			}
			got, _ := r.Body["erp_name"].(string)
			if name == "" {
				name = got
			}
			if got != name {
				t.Errorf("approve %d entry %q, want %q", i, got, name)
			}
		}
		if n := countEntries(id); n != 1 {
			t.Errorf("%d Journal Entries carry %s, want 1", n, id)
		}
	})

	t.Run("a rejected proposal never reaches ERPNext", func(t *testing.T) {
		fid := itFinding(t, st, 1006, "CC-1004 integration test: rejected")
		res := call("it-tok-asha", "/api/findings/"+fid.String()+"/proposals", "")
		if res.Status != http.StatusCreated {
			t.Fatalf("propose = %d %s", res.Status, res.Raw)
		}
		id, _ := res.Body["proposal_id"].(string)
		trackID(id)
		if res := call("it-tok-ravi", "/api/proposals/"+id+"/reject", ""); res.Status != http.StatusBadRequest {
			t.Errorf("reject without a reason = %d %s", res.Status, res.Raw)
		}
		if res := call("it-tok-ravi", "/api/proposals/"+id+"/reject", `{"reason":"wrong expense head"}`); res.Status != http.StatusOK {
			t.Fatalf("reject = %d %s", res.Status, res.Raw)
		}
		if res := call("it-tok-ravi", "/api/proposals/"+id+"/approve", ""); res.Status != http.StatusConflict {
			t.Errorf("approve after reject = %d %s", res.Status, res.Raw)
		}
		if n := countEntries(id); n != 0 {
			t.Errorf("%d Journal Entries carry the rejected %s", n, id)
		}
	})

	t.Run("the audit log names the users", func(t *testing.T) {
		logs, err := st.ListAuditLogs(t.Context(), 500)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{
			"asha approvals proposal.create created":                                    false,
			"asha approvals proposal.approve refused:self_approval":                     false,
			"ravi approvals proposal.approve posted":                                    false,
			"ravi books-admin " + books.ToolPostApprovedJournalEntry + " " + proposalID: false,
		}
		for _, e := range logs {
			if e.Server == nil || e.Tool == nil {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal(e.Args, &args)
			if args["proposal_id"] != proposalID {
				continue
			}
			if strings.Contains(string(e.Args), cfg.MCPTokenAdmin.Reveal()) || strings.Contains(string(e.Args), itDebitAccount) {
				t.Errorf("audit args carry a token or payload lines: %s", e.Args)
			}
			key := e.Actor + " " + *e.Server + " " + *e.Tool + " "
			if *e.Server == "books-admin" {
				key += proposalID
			} else {
				key += fmt.Sprint(args["result"])
			}
			if _, ok := want[key]; ok {
				want[key] = true
			}
		}
		for k, seen := range want {
			if !seen {
				t.Errorf("no audit row %q", k)
			}
		}
	})

	t.Run("the agent token gets 401 on /mcp-admin", func(t *testing.T) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, booksSrv.URL+mcpkit.AdminPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+cfg.MCPTokenAgent.Reveal())
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("agent token on /mcp-admin = %d, want 401", resp.StatusCode)
		}
	})
}
