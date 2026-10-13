package books

// The only write tool (CC-504b): post_approved_journal_entry, served on
// /mcp-admin only. It takes nothing but a proposal ID. The entry's lines,
// company and date come from the approved journal_proposals row in
// Postgres, never from the caller, so even a compromised caller can't post
// an arbitrary entry.
//
// The whole call runs inside WithProposalLock (a FOR UPDATE row lock), so
// two calls for one proposal can't both post. The Journal Entry carries
// the proposal ID in copilot_ext_id; that is how a call finds the draft or
// submitted entry a crashed call left behind and records it instead of
// posting twice. Frappe has no transactional link to Postgres, so the
// ext-ID lookup is the recovery path, not the lock.
//
// Every refusal is a tool error with a fixed message naming the rule. The
// full error goes to the logger; neither ever carries the payload lines.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ToolPostApprovedJournalEntry is the admin write tool, listed on
// /mcp-admin only.
const ToolPostApprovedJournalEntry = "post_approved_journal_entry"

// Proposal statuses (journal_proposals.status).
const (
	ProposalApproved = "approved"
	ProposalPosted   = "posted"
)

const (
	// AdminToolTimeout bounds one whole post call.
	AdminToolTimeout = 90 * time.Second
	// adminSaveTimeout bounds the save once ERPNext holds a submitted
	// entry; it runs detached from the call's cancellation.
	adminSaveTimeout = 10 * time.Second
	journalEntry     = "Journal Entry"
)

// ProposalStore is what the admin tool needs from Postgres;
// *store.Store satisfies it.
type ProposalStore interface {
	WithProposalLock(ctx context.Context, id uuid.UUID, fn func(ctx context.Context, p store.JournalProposal, save func(ctx context.Context, p store.JournalProposal) error) error) error
}

// AdminDeps are what the admin tool needs. Client is the books server's
// ERPNext client (the bot key). Now, when nil, is time.Now; Logger, when
// nil, is slog.Default().
type AdminDeps struct {
	Client    *frappe.Client
	Companies CompanyResolver
	Proposals ProposalStore
	Now       func() time.Time
	Logger    *slog.Logger
}

// PostApprovedInput is the tool's only input: the proposal ID.
type PostApprovedInput struct {
	ProposalID string `json:"proposal_id" jsonschema:"ID (UUID) of an approved journal proposal; the entry's lines, company and date are read from the approved proposal, never from the call"`
}

// PostApprovedOutput is the posted proposal.
type PostApprovedOutput struct {
	ProposalID    string `json:"proposal_id" jsonschema:"the proposal ID"`
	ERPName       string `json:"erp_name" jsonschema:"name of the submitted ERPNext Journal Entry"`
	Status        string `json:"status" jsonschema:"always posted"`
	AlreadyPosted bool   `json:"already_posted" jsonschema:"true when an earlier call had already recorded this post; nothing new was written to ERPNext"`
}

// RegisterAdminTools adds post_approved_journal_entry to s. Call it only
// for the /mcp-admin server, after mcpkit.EnableAudit.
func RegisterAdminTools(s *mcp.Server, deps AdminDeps) {
	registerAdmin(s, &admin{deps: deps, timeout: AdminToolTimeout})
}

func registerAdmin(s *mcp.Server, a *admin) {
	f := false
	mcpkit.AddTool(s, &mcp.Tool{
		Name: ToolPostApprovedJournalEntry,
		Description: "Posts one journal proposal that a checker other than its maker approved, as a submitted ERPNext Journal Entry. " +
			"It takes only the proposal ID; the lines (amounts in paise), company and posting date (YYYY-MM-DD) come from the approved proposal. " +
			"Calling it again for the same proposal returns the first call's entry with already_posted true and writes nothing.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &f, IdempotentHint: true},
	}, a.handle)
}

type admin struct {
	deps    AdminDeps
	timeout time.Duration
	// afterSubmit, when set (tests only), runs once ERPNext has accepted
	// a submit, before the result is checked and saved.
	afterSubmit func()
}

// refusal is a tool error with a fixed, model-safe message. err, when
// set, is the full cause for the log only.
type refusal struct {
	code, msg string
	err       error
}

func (r *refusal) Error() string { return r.code + ": " + r.msg }
func (r *refusal) Unwrap() error { return r.err }

func refuse(code, msg string) *refusal { return &refusal{code: code, msg: msg} }

func refuseErr(code, msg string, err error) *refusal { return &refusal{code: code, msg: msg, err: err} }

// Refusal codes. Each names the rule a call broke.
const (
	CodeInvalidProposalID = "invalid_proposal_id"
	CodeNotFound          = "not_found"
	CodeInvalidState      = "invalid_state"
	CodeNotApproved       = "not_approved"
	CodeNoChecker         = "no_checker"
	CodeSelfApproved      = "self_approved"
	CodeNotDecided        = "not_decided"
	CodeInvalidDate       = "invalid_posting_date"
	CodeInvalidLines      = "invalid_lines"
	CodeUnbalanced        = "unbalanced"
	CodeUnknownCompany    = "unknown_company"
	CodeERPConflict       = "erp_conflict"
	CodeERPMismatch       = "erp_mismatch"
	CodeERPPermission     = "erp_permission"
	CodeERPUnavailable    = "erp_unavailable"
	CodeStoreUnavailable  = "store_unavailable"
)

func (a *admin) logger() *slog.Logger {
	if a.deps.Logger != nil {
		return a.deps.Logger
	}
	return slog.Default()
}

func (a *admin) now() time.Time {
	if a.deps.Now != nil {
		return a.deps.Now()
	}
	return time.Now()
}

func (a *admin) handle(ctx context.Context, _ *mcp.CallToolRequest, in PostApprovedInput) (*mcp.CallToolResult, PostApprovedOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	log := a.logger()

	id, ok := parseProposalID(in.ProposalID)
	if !ok {
		return nil, PostApprovedOutput{}, refuse(CodeInvalidProposalID, "proposal_id must be a UUID")
	}
	var (
		out       PostApprovedOutput
		submitted string // a Journal Entry ERPNext holds as submitted for this proposal
	)
	err := a.deps.Proposals.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		o, sub, err := a.postLocked(ctx, p, save)
		out, submitted = o, sub
		return err
	})
	if err != nil {
		var r *refusal
		switch {
		case errors.As(err, &r):
		case errors.Is(err, store.ErrNotFound):
			r = refuseErr(CodeNotFound, "no journal proposal has this ID", err)
		default:
			r = refuseErr(CodeStoreUnavailable, "the proposal store failed; try again later", err)
		}
		if submitted != "" {
			// ERPNext holds a submitted entry the row doesn't record yet;
			// a retry finds it by copilot_ext_id and records it.
			log.Error("admin: journal entry submitted but not recorded", "proposal_id", id.String(), "erp_name", submitted, "code", r.code, "error", err)
		} else {
			log.Warn("admin: post refused", "proposal_id", id.String(), "code", r.code, "error", err)
		}
		return nil, PostApprovedOutput{}, r
	}
	// On record before the audit row: if the audit insert then fails, the
	// caller sees an error although the entry is posted.
	log.Info("admin: journal entry posted", "proposal_id", out.ProposalID, "erp_name", out.ERPName, "already_posted", out.AlreadyPosted)
	return nil, out, nil
}

// parseProposalID accepts only the canonical lowercase hyphenated form.
func parseProposalID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}

// postLocked runs under the row lock. It returns the output, and the name
// of a Journal Entry ERPNext holds as submitted for p when one is known
// (for the log if the save then fails).
func (a *admin) postLocked(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) (PostApprovedOutput, string, error) {
	if p.Checker != nil && strings.TrimSpace(*p.Checker) != "" {
		mcpkit.SetAuditActor(ctx, strings.TrimSpace(*p.Checker))
	}
	out := PostApprovedOutput{ProposalID: p.ID.String(), Status: ProposalPosted}

	// 1. Already recorded.
	if p.ERPName != nil && *p.ERPName != "" {
		out.ERPName, out.AlreadyPosted = *p.ERPName, true
		switch p.Status {
		case ProposalPosted:
			return out, "", nil
		case ProposalApproved:
			if err := checkChecker(p); err != nil {
				return PostApprovedOutput{}, "", err
			}
			p.Status = ProposalPosted
			if p.PostedAt == nil {
				now := a.now().UTC()
				p.PostedAt = &now
			}
			if err := a.save(ctx, save, p); err != nil {
				return PostApprovedOutput{}, "", err
			}
			return out, "", nil
		default:
			return PostApprovedOutput{}, "", refuse(CodeInvalidState, "the proposal names an ERPNext entry but is neither approved nor posted; a human must check it")
		}
	}
	// 2. Approved only.
	if p.Status != ProposalApproved {
		return PostApprovedOutput{}, "", refuse(CodeNotApproved, "the proposal is not approved")
	}
	// 3. Maker-checker.
	if err := checkChecker(p); err != nil {
		return PostApprovedOutput{}, "", err
	}
	// 4. The payload, before any ERPNext call.
	company, err := a.validatePayload(p)
	if err != nil {
		return PostApprovedOutput{}, "", err
	}
	// 5. Find what an earlier call left in ERPNext, or post.
	name, err := a.ensureSubmitted(ctx, p, company)
	if err != nil {
		return PostApprovedOutput{}, name, err
	}
	// 6. Record.
	now := a.now().UTC()
	p.Status, p.ERPName, p.PostedAt = ProposalPosted, &name, &now
	if err := a.save(ctx, save, p); err != nil {
		return PostApprovedOutput{}, name, err
	}
	out.ERPName = name
	return out, name, nil
}

// save writes p detached from the call's cancellation: once ERPNext holds
// a submitted entry, a cancelled caller must not leave the row approved.
func (a *admin) save(ctx context.Context, save func(context.Context, store.JournalProposal) error, p store.JournalProposal) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), adminSaveTimeout)
	defer cancel()
	if err := save(sctx, p); err != nil {
		return refuseErr(CodeStoreUnavailable, "the proposal store failed; try again later", err)
	}
	return nil
}

// checkChecker requires a non-blank checker other than the maker (after
// trimming and case folding) and a decision time.
func checkChecker(p store.JournalProposal) error {
	if p.Checker == nil || strings.TrimSpace(*p.Checker) == "" {
		return refuse(CodeNoChecker, "the proposal has no checker")
	}
	if strings.EqualFold(strings.TrimSpace(*p.Checker), strings.TrimSpace(p.Maker)) {
		return refuse(CodeSelfApproved, "the checker is the maker; a proposal needs a second person's approval")
	}
	if p.DecidedAt == nil {
		return refuse(CodeNotDecided, "the proposal has no decision time")
	}
	return nil
}

// validatePayload checks the row's payload and resolves its company.
func (a *admin) validatePayload(p store.JournalProposal) (string, error) {
	pl := p.Payload
	if d, err := time.Parse(time.DateOnly, pl.PostingDate); err != nil || d.Format(time.DateOnly) != pl.PostingDate {
		return "", refuse(CodeInvalidDate, "the proposal's posting_date is not YYYY-MM-DD")
	}
	if len(pl.Lines) < 2 {
		return "", refuse(CodeInvalidLines, "the proposal needs at least two lines")
	}
	var dr, cr money.Paise
	for _, l := range pl.Lines {
		if strings.TrimSpace(l.Account) == "" || strings.TrimSpace(l.Account) != l.Account {
			return "", refuse(CodeInvalidLines, "every line needs an account")
		}
		if l.DebitPaise < 0 || l.CreditPaise < 0 || (l.DebitPaise > 0) == (l.CreditPaise > 0) {
			return "", refuse(CodeInvalidLines, "every line needs exactly one of debit or credit, positive")
		}
		var ok bool
		if dr, ok = addPaise(dr, l.DebitPaise); !ok {
			return "", refuse(CodeUnbalanced, "the proposal's totals overflow")
		}
		if cr, ok = addPaise(cr, l.CreditPaise); !ok {
			return "", refuse(CodeUnbalanced, "the proposal's totals overflow")
		}
	}
	if dr != cr || dr == 0 {
		return "", refuse(CodeUnbalanced, "the proposal's debits and credits don't balance")
	}
	if a.deps.Companies == nil {
		return "", refuse(CodeUnknownCompany, "the proposal's company has no profile")
	}
	company, err := a.deps.Companies.ERPCompany(p.CompanyID)
	if err != nil {
		return "", refuseErr(CodeUnknownCompany, "the proposal's company has no profile", err)
	}
	return company, nil
}

// addPaise adds two non-negative amounts, reporting overflow.
func addPaise(a, b money.Paise) (money.Paise, bool) {
	if b > 0 && a > money.Paise(math.MaxInt64)-b {
		return 0, false
	}
	return a + b, true
}

// jeRef is a Journal Entry list row.
type jeRef struct {
	Name      string `json:"name"`
	DocStatus int    `json:"docstatus"`
}

// ensureSubmitted returns the name of a submitted Journal Entry for p:
// one an earlier call left behind (submitting it when still a draft), or
// a new one. It returns the name with an error only when ERPNext holds a
// submitted entry that isn't recorded.
func (a *admin) ensureSubmitted(ctx context.Context, p store.JournalProposal, company string) (string, error) {
	c := a.deps.Client
	extID := p.ID.String()
	rows, err := frappe.List[jeRef](ctx, c, journalEntry, frappe.Query{
		Fields:  []string{"name", "docstatus"},
		Filters: [][]any{{seed.ExtIDField, "=", extID}},
		Limit:   2,
	})
	if err != nil {
		return "", erpError(err)
	}
	var name string
	switch len(rows) {
	case 0:
		doc, err := frappe.Insert(ctx, c, journalEntry, journalBody(p, company))
		if err != nil {
			return "", erpError(err)
		}
		name, _ = doc["name"].(string)
		if name == "" {
			return "", refuseErr(CodeERPUnavailable, "ERPNext failed the request; see the server log", errors.New("insert returned no name"))
		}
	case 1:
		name = rows[0].Name
		if rows[0].DocStatus == 2 {
			return "", refuseErr(CodeERPConflict, "ERPNext holds a cancelled entry for this proposal; a human must check it", fmt.Errorf("journal entry %s is cancelled", name))
		}
		doc, err := frappe.Get[map[string]any](ctx, c, journalEntry, name)
		if err != nil {
			return "", erpError(err)
		}
		ds, err := checkJournalEntry(doc, p, company)
		if err != nil {
			return "", err
		}
		switch ds {
		case 1:
			return name, nil
		case 0:
		default:
			return "", refuseErr(CodeERPConflict, "ERPNext holds a cancelled entry for this proposal; a human must check it", fmt.Errorf("journal entry %s has docstatus %d", name, ds))
		}
	default:
		return "", refuseErr(CodeERPConflict, "ERPNext holds more than one entry for this proposal; a human must check them", fmt.Errorf("%d journal entries carry %s", len(rows), extID))
	}

	doc, err := frappe.Submit(ctx, c, journalEntry, name)
	if err != nil {
		return "", erpError(err)
	}
	if a.afterSubmit != nil {
		a.afterSubmit()
	}
	ds, err := checkJournalEntry(doc, p, company)
	if err == nil && ds != 1 {
		err = refuseErr(CodeERPMismatch, "the ERPNext entry differs from the approved proposal; a human must check it", fmt.Errorf("docstatus %d after submit", ds))
	}
	if err != nil {
		a.logger().Error("admin: submitted journal entry differs from the approved proposal", "proposal_id", extID, "erp_name", name, "error", err)
		return "", err
	}
	return name, nil
}

// journalBody is the draft Journal Entry for p, amounts as rupee strings.
func journalBody(p store.JournalProposal, company string) map[string]any {
	rows := make([]map[string]any, len(p.Payload.Lines))
	for i, l := range p.Payload.Lines {
		rows[i] = map[string]any{
			"account":                    l.Account,
			"debit_in_account_currency":  json.Number(l.DebitPaise.Rupees()),
			"credit_in_account_currency": json.Number(l.CreditPaise.Rupees()),
		}
	}
	return map[string]any{
		"voucher_type":  journalEntry,
		"company":       company,
		"posting_date":  p.Payload.PostingDate,
		"user_remark":   p.Payload.Remark,
		seed.ExtIDField: p.ID.String(),
		"accounts":      rows,
	}
}

// lineKey is one Journal Entry row as compared with a proposal line.
type lineKey struct {
	account       string
	debit, credit money.Paise
}

func compareLineKey(a, b lineKey) int {
	if c := strings.Compare(a.account, b.account); c != 0 {
		return c
	}
	if a.debit != b.debit {
		if a.debit < b.debit {
			return -1
		}
		return 1
	}
	switch {
	case a.credit < b.credit:
		return -1
	case a.credit > b.credit:
		return 1
	}
	return 0
}

func mismatch(format string, args ...any) error {
	return refuseErr(CodeERPMismatch, "the ERPNext entry differs from the approved proposal; a human must check it", fmt.Errorf(format, args...))
}

// checkJournalEntry checks an ERPNext Journal Entry against p: voucher type,
// company, posting date, external ID, and the rows as a multiset of
// (account, debit, credit) equal to the payload's lines. It returns the
// entry's docstatus. The detail in a mismatch names fields, never amounts
// or accounts.
func checkJournalEntry(doc map[string]any, p store.JournalProposal, company string) (int, error) {
	if s, _ := doc["voucher_type"].(string); s != journalEntry {
		return 0, mismatch("voucher_type differs")
	}
	if s, _ := doc["company"].(string); s != company {
		return 0, mismatch("company differs")
	}
	if s, _ := doc["posting_date"].(string); s != p.Payload.PostingDate {
		return 0, mismatch("posting_date differs")
	}
	if s, _ := doc[seed.ExtIDField].(string); s != p.ID.String() {
		return 0, mismatch("%s differs", seed.ExtIDField)
	}
	ds, ok := intField(doc["docstatus"])
	if !ok {
		return 0, mismatch("docstatus unreadable")
	}
	raw, _ := doc["accounts"].([]any)
	if len(raw) != len(p.Payload.Lines) {
		return 0, mismatch("entry has %d rows, proposal %d lines", len(raw), len(p.Payload.Lines))
	}
	got := make([]lineKey, 0, len(raw))
	for i, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			return 0, mismatch("row %d unreadable", i)
		}
		acct, _ := row["account"].(string)
		dr, err1 := paiseField(row["debit_in_account_currency"])
		cr, err2 := paiseField(row["credit_in_account_currency"])
		if err1 != nil || err2 != nil {
			return 0, mismatch("row %d amounts unreadable", i)
		}
		if dr < 0 || cr < 0 || (dr > 0) == (cr > 0) {
			return 0, mismatch("row %d needs exactly one of debit or credit", i)
		}
		// In company currency too, when ERPNext sends it.
		for field, want := range map[string]money.Paise{"debit": dr, "credit": cr} {
			if v, present := row[field]; present {
				if amt, err := paiseField(v); err != nil || amt != want {
					return 0, mismatch("row %d %s differs from %s_in_account_currency", i, field, field)
				}
			}
		}
		got = append(got, lineKey{account: acct, debit: dr, credit: cr})
	}
	want := make([]lineKey, len(p.Payload.Lines))
	for i, l := range p.Payload.Lines {
		want[i] = lineKey{account: l.Account, debit: l.DebitPaise, credit: l.CreditPaise}
	}
	slices.SortFunc(got, compareLineKey)
	slices.SortFunc(want, compareLineKey)
	if !slices.Equal(got, want) {
		return 0, mismatch("rows differ from the proposal's lines")
	}
	return ds, nil
}

// paiseField reads an ERPNext amount (json.Number of rupees) as paise.
func paiseField(v any) (money.Paise, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("amount is %T, not a number", v)
	}
	return money.FromJSONNumber(n)
}

// intField reads a small integer such as docstatus.
func intField(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > 2 {
		return 0, false
	}
	return int(i), true
}

// erpError classes an ERPNext failure into a fixed refusal.
func erpError(err error) error {
	if frappe.IsPermission(err) {
		return refuseErr(CodeERPPermission, "ERPNext refused the bot's permission for this entry", err)
	}
	return refuseErr(CodeERPUnavailable, "ERPNext failed the request; see the server log", err)
}
