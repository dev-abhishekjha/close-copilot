package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// Proposal statuses (journal_proposals.status).
const (
	StatusProposed = "proposed"
	StatusApproved = "approved"
	StatusPosted   = "posted"
	StatusRejected = "rejected"
)

// Limits on what a maker or checker may write.
const (
	MaxRemarkRunes  = 500
	MaxReasonRunes  = 500
	MaxAccountRunes = 140
	// pendingLimit caps each status list of Pending.
	pendingLimit = store.MaxProposalList
)

// Audit names: every action writes one audit_log row with server
// AuditServer and one of these tools.
const (
	AuditServer  = "approvals"
	ToolCreate   = "proposal.create"
	ToolApprove  = "proposal.approve"
	ToolReject   = "proposal.reject"
	resultPosted = "posted"
	resultError  = "error"
)

// Error kinds. Every error the service returns for a refused request is an
// *Error whose Kind is one of these, so callers map it with errors.Is. The
// web layer answers ErrNotFound with 404, ErrInvalidProposal and
// ErrInvalidReason with 400, ErrSelfApproval with 403, ErrNotProposable,
// ErrProposalExists and ErrNotPending with 409, and ErrPostFailed with 502.
var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidProposal = errors.New("the proposed entry is invalid")
	ErrInvalidReason   = errors.New("a reason is required: at most 500 characters, no control characters")
	ErrNotProposable   = errors.New("this finding can't be proposed")
	ErrProposalExists  = errors.New("this finding already has a journal proposal")
	ErrSelfApproval    = errors.New("you proposed this entry, so a different checker must approve it")
	ErrNotPending      = errors.New("this proposal is not pending")
	ErrPostFailed      = errors.New("posting to ERPNext failed")
)

// Error is a refused request: Kind is one of the Err* kinds, Msg a fixed,
// user-safe message, and Code a short machine-readable reason (for a post
// failure, the admin tool's refusal code). Msg and Code never carry
// payload lines, error text from below or a token.
type Error struct {
	Kind error
	Msg  string
	Code string
}

func (e *Error) Error() string { return e.Msg }

// Unwrap returns the kind.
func (e *Error) Unwrap() error { return e.Kind }

func refusal(kind error, code, msg string) *Error {
	if msg == "" {
		msg = kind.Error()
	}
	return &Error{Kind: kind, Msg: msg, Code: code}
}

// Fixed refusals of Propose.
var (
	errNoPreview       = refusal(ErrNotProposable, "no_preview", "this finding has no proposed entry")
	errNeedsReview     = refusal(ErrNotProposable, "needs_review", "this finding needs review before an entry can be proposed")
	errUnverified      = refusal(ErrNotProposable, "unverified", "this finding's explanation is not verified, so its entry can't be proposed")
	errAccepted        = refusal(ErrNotProposable, "accepted", "this finding is already accepted")
	errDismissed       = refusal(ErrNotProposable, "dismissed", "this finding is dismissed")
	errNotOpen         = refusal(ErrNotProposable, "not_open", "this finding is not open")
	errCompanyMismatch = refusal(ErrNotProposable, "company_mismatch", "the proposed entry's company differs from the run's company")
	errOtherFinding    = refusal(ErrNotProposable, "finding_mismatch", "the proposed entry belongs to another finding")
	errExists          = refusal(ErrProposalExists, "exists", "")
	errNotFound        = refusal(ErrNotFound, "not_found", "")
	errSelf            = refusal(ErrSelfApproval, "self_approval", "")
	errNotPending      = refusal(ErrNotPending, "not_pending", "")
	errOtherChecker    = refusal(ErrNotPending, "other_checker", "this proposal was already approved by another checker")
	errReason          = refusal(ErrInvalidReason, "invalid_reason", "")
	errNoActor         = errors.New("approvals: no authenticated user")
)

// Canonical is the one normalisation of a username for maker and checker:
// trimmed and lower-cased.
func Canonical(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ProposalStore is what the service needs of journal_proposals and the
// audit log. *store.Store satisfies it.
type ProposalStore interface {
	CreateProposalFromFinding(ctx context.Context, p store.JournalProposal) (store.JournalProposal, error)
	GetJournalProposal(ctx context.Context, id uuid.UUID) (store.JournalProposal, error)
	ListProposalsByStatus(ctx context.Context, status string, limit int) ([]store.JournalProposal, error)
	WithProposalLock(ctx context.Context, id uuid.UUID, fn func(ctx context.Context, p store.JournalProposal, save func(ctx context.Context, p store.JournalProposal) error) error) error
	InsertAuditLog(ctx context.Context, e store.AuditLogEntry) (int64, error)
}

// FindingLookup reads a finding and its run. *store.Store satisfies it.
type FindingLookup interface {
	GetFinding(ctx context.Context, id uuid.UUID) (store.Finding, error)
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
}

// Deps are what the service needs. Now, when nil, is time.Now; Logger,
// when nil, is slog.Default().
type Deps struct {
	Store    ProposalStore
	Poster   Poster
	Findings FindingLookup
	Now      func() time.Time
	Logger   *slog.Logger
}

// Service is the maker-checker workflow: a maker proposes a finding's
// entry, a different checker approves (which posts it through the admin
// MCP tool) or rejects it. The actor of every method is the authenticated
// username; the service never takes a maker or checker from anywhere else.
type Service struct {
	deps Deps
}

// New returns a service over deps.
func New(deps Deps) *Service { return &Service{deps: deps} }

// ApproveResult is a posted proposal.
type ApproveResult struct {
	ProposalID uuid.UUID
	Status     string
	ERPName    string
	// AlreadyPosted is true when the proposal was posted before this call.
	AlreadyPosted bool
}

func (s *Service) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.deps.Logger != nil {
		return s.deps.Logger
	}
	return slog.Default()
}

// Propose turns finding findingID's verified, open proposal preview into a
// pending journal proposal made by actor.
func (s *Service) Propose(ctx context.Context, actor string, findingID uuid.UUID) (store.JournalProposal, error) {
	maker := Canonical(actor)
	if maker == "" {
		return store.JournalProposal{}, errNoActor
	}
	args := map[string]any{"finding_id": findingID.String()}
	p, err := s.propose(ctx, maker, findingID)
	if p.ID != uuid.Nil {
		args["proposal_id"] = p.ID.String()
	}
	args["result"] = resultOf(err, "created")
	s.audit(ctx, maker, ToolCreate, p.ID, args)
	if err != nil {
		return store.JournalProposal{}, err
	}
	return p, nil
}

func (s *Service) propose(ctx context.Context, maker string, findingID uuid.UUID) (store.JournalProposal, error) {
	f, err := s.deps.Findings.GetFinding(ctx, findingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.JournalProposal{}, errNotFound
		}
		return store.JournalProposal{}, fmt.Errorf("approvals: propose: finding %s: %w", findingID, err)
	}
	if err := proposable(f); err != nil {
		return store.JournalProposal{}, err
	}
	run, err := s.deps.Findings.GetCloseRun(ctx, f.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.JournalProposal{}, errNotFound
		}
		return store.JournalProposal{}, fmt.Errorf("approvals: propose: run of finding %s: %w", findingID, err)
	}
	preview := f.Proposal
	if run.CompanyID == "" || preview.CompanyID != run.CompanyID {
		return store.JournalProposal{}, errCompanyMismatch
	}
	if preview.FindingID != nil && *preview.FindingID != findingID {
		return store.JournalProposal{}, errOtherFinding
	}
	if err := ValidatePayload(preview.Payload); err != nil {
		return store.JournalProposal{}, err
	}
	payload := preview.Payload
	payload.Remark = strings.TrimSpace(payload.Remark)
	payload.Lines = append([]store.JournalLine(nil), payload.Lines...)
	fid := findingID
	p := store.JournalProposal{
		ID:        uuid.New(),
		FindingID: &fid,
		CompanyID: run.CompanyID,
		Payload:   payload,
		Status:    StatusProposed,
		Maker:     maker,
		CreatedAt: s.now(),
	}
	created, err := s.deps.Store.CreateProposalFromFinding(ctx, p)
	if err != nil {
		if errors.Is(err, store.ErrProposalExists) {
			return store.JournalProposal{ID: created.ID}, errExists
		}
		return store.JournalProposal{}, fmt.Errorf("approvals: propose: %w", err)
	}
	return created, nil
}

// proposable refuses a finding whose preview may not become a proposal:
// none, or a finding that isn't open and verified.
func proposable(f store.Finding) error {
	if f.Proposal == nil {
		return errNoPreview
	}
	switch f.Status {
	case "open":
	case "needs_review":
		return errNeedsReview
	case "accepted":
		return errAccepted
	case "dismissed":
		return errDismissed
	default:
		return errNotOpen
	}
	if !f.Verified {
		return errUnverified
	}
	return nil
}

// Approve records actor as the checker of proposal id and posts it to
// ERPNext through the admin tool. The maker can't approve their own
// proposal. Approving a posted proposal returns its entry without posting
// again; an approved one whose post failed may be retried only by its
// checker.
func (s *Service) Approve(ctx context.Context, actor string, id uuid.UUID) (ApproveResult, error) {
	checker := Canonical(actor)
	if checker == "" {
		return ApproveResult{}, errNoActor
	}
	res, err := s.approve(ctx, checker, id)
	s.audit(ctx, checker, ToolApprove, id, map[string]any{
		"proposal_id": id.String(),
		"result":      resultOf(err, resultPosted),
	})
	if err != nil {
		return ApproveResult{}, err
	}
	return res, nil
}

func (s *Service) approve(ctx context.Context, checker string, id uuid.UUID) (ApproveResult, error) {
	var (
		needPost bool
		done     ApproveResult
	)
	// Step 1, under the row lock: decide and commit approved before any
	// ERPNext traffic. The admin tool takes the same lock, so it must be
	// released before the post.
	err := s.deps.Store.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		if Canonical(p.Maker) == checker {
			return errSelf
		}
		switch p.Status {
		case StatusPosted:
			done = ApproveResult{ProposalID: id, Status: StatusPosted, ERPName: deref(p.ERPName), AlreadyPosted: true}
			return nil
		case StatusProposed:
			now := s.now()
			c := checker
			p.Status, p.Checker, p.DecidedAt = StatusApproved, &c, &now
			needPost = true
			return save(ctx, p)
		case StatusApproved:
			if p.Checker == nil || Canonical(*p.Checker) != checker {
				return errOtherChecker
			}
			needPost = true
			return nil
		default:
			return errNotPending
		}
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ApproveResult{}, errNotFound
		}
		var e *Error
		if errors.As(err, &e) {
			return ApproveResult{}, e
		}
		return ApproveResult{}, fmt.Errorf("approvals: approve %s: %w", id, err)
	}
	if !needPost {
		return done, nil
	}

	// Step 2: the admin tool posts, re-checking checker != maker under its
	// own lock, and alone writes erp_name, posted_at and status posted.
	out, err := s.deps.Poster.Post(ctx, id)
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			return ApproveResult{}, &Error{Kind: ErrPostFailed, Code: ref.Code, Msg: "posting to ERPNext was refused: " + ref.Code}
		}
		s.log().ErrorContext(ctx, "approvals: post failed", "proposal_id", id.String(), "err", err)
		return ApproveResult{}, &Error{Kind: ErrPostFailed, Code: resultError, Msg: ErrPostFailed.Error() + "; the proposal stays approved and can be retried"}
	}
	res := ApproveResult{ProposalID: id, Status: StatusPosted, ERPName: out.ERPName, AlreadyPosted: out.AlreadyPosted}
	row, err := s.deps.Store.GetJournalProposal(ctx, id)
	if err != nil {
		s.log().WarnContext(ctx, "approvals: re-read after post failed; using the tool's entry name", "proposal_id", id.String(), "err", err)
		return res, nil
	}
	if row.ERPName != nil && *row.ERPName != "" {
		res.ERPName = *row.ERPName
	}
	if row.Status != StatusPosted {
		s.log().WarnContext(ctx, "approvals: proposal not posted after a successful post", "proposal_id", id.String(), "status", row.Status)
	}
	return res, nil
}

// Reject records actor as the checker who rejected proposal id, with a
// reason. Nothing reaches ERPNext.
func (s *Service) Reject(ctx context.Context, actor string, id uuid.UUID, reason string) error {
	checker := Canonical(actor)
	if checker == "" {
		return errNoActor
	}
	reason = strings.TrimSpace(reason)
	args := map[string]any{"proposal_id": id.String()}
	err := validText(reason, MaxReasonRunes, true)
	if err != nil {
		err = errReason
	} else {
		args["reason"] = reason
		err = s.reject(ctx, checker, id, reason)
	}
	args["result"] = resultOf(err, StatusRejected)
	s.audit(ctx, checker, ToolReject, id, args)
	return err
}

func (s *Service) reject(ctx context.Context, checker string, id uuid.UUID, reason string) error {
	err := s.deps.Store.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		if Canonical(p.Maker) == checker {
			return errSelf
		}
		if p.Status != StatusProposed {
			return errNotPending
		}
		now := s.now()
		c, r := checker, reason
		p.Status, p.Checker, p.Reason, p.DecidedAt = StatusRejected, &c, &r, &now
		return save(ctx, p)
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound
		}
		var e *Error
		if errors.As(err, &e) {
			return e
		}
		return fmt.Errorf("approvals: reject %s: %w", id, err)
	}
	return nil
}

// Pending returns the proposals awaiting a checker: proposed ones, and
// approved ones whose post failed and awaits a retry, oldest first.
func (s *Service) Pending(ctx context.Context) ([]store.JournalProposal, error) {
	var out []store.JournalProposal
	for _, status := range []string{StatusProposed, StatusApproved} {
		ps, err := s.deps.Store.ListProposalsByStatus(ctx, status, pendingLimit)
		if err != nil {
			return nil, fmt.Errorf("approvals: pending: %w", err)
		}
		out = append(out, ps...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// Get returns proposal id. A missing proposal is ErrNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (store.JournalProposal, error) {
	p, err := s.deps.Store.GetJournalProposal(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.JournalProposal{}, errNotFound
		}
		return store.JournalProposal{}, fmt.Errorf("approvals: get %s: %w", id, err)
	}
	return p, nil
}

// audit writes one audit_log row. A failed insert is logged, not returned:
// the action itself already happened.
func (s *Service) audit(ctx context.Context, actor, tool string, id uuid.UUID, args map[string]any) {
	raw, err := json.Marshal(args)
	if err != nil {
		s.log().ErrorContext(ctx, "approvals: audit args", "action", tool, "proposal_id", id.String(), "err", err)
		return
	}
	server, t := AuditServer, tool
	if _, err := s.deps.Store.InsertAuditLog(ctx, store.AuditLogEntry{
		At:     s.now(),
		Actor:  actor,
		Server: &server,
		Tool:   &t,
		Args:   raw,
	}); err != nil {
		s.log().ErrorContext(ctx, "approvals: audit insert failed", "action", tool, "proposal_id", id.String(), "err", err)
	}
}

// resultOf is an audit result: ok on success, refused:<code> for a
// refusal, error otherwise.
func resultOf(err error, ok string) string {
	if err == nil {
		return ok
	}
	var e *Error
	if errors.As(err, &e) && e.Code != "" && e.Code != resultError {
		return "refused:" + e.Code
	}
	return resultError
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Fixed refusals of ValidatePayload.
var (
	errDate         = refusal(ErrInvalidProposal, "invalid_posting_date", "the posting date must be a valid YYYY-MM-DD date")
	errFewLines     = refusal(ErrInvalidProposal, "invalid_lines", "the entry needs at least two lines")
	errAccount      = refusal(ErrInvalidProposal, "invalid_account", "every line needs an account name of at most 140 characters, without control characters")
	errLineAmounts  = refusal(ErrInvalidProposal, "invalid_amount", "every line needs exactly one positive debit or credit")
	errUnbalanced   = refusal(ErrInvalidProposal, "unbalanced", "total debits must equal total credits and be more than zero")
	errOverflow     = refusal(ErrInvalidProposal, "overflow", "the entry's totals are too large")
	errRemark       = refusal(ErrInvalidProposal, "invalid_remark", "the remark must be at most 500 characters, without control characters")
	errTooManyLines = refusal(ErrInvalidProposal, "invalid_lines", "the entry has too many lines")
)

const (
	// MaxPayloadLines caps the lines of one proposal.
	MaxPayloadLines = 100
	dateLayout      = "2006-01-02"
)

// ValidatePayload checks a journal payload before it becomes a proposal:
// a real YYYY-MM-DD posting date; at least two lines, each with a
// non-blank account (at most 140 runes, no control characters) and exactly
// one positive debit or credit; debits equal credits and are non-zero
// (int64 paise, overflow-checked); a trimmed remark of at most 500 runes
// without control characters. Errors are *Error of kind
// ErrInvalidProposal with a fixed message.
func ValidatePayload(p store.JournalPayload) error {
	d, err := time.Parse(dateLayout, p.PostingDate)
	if err != nil || d.Format(dateLayout) != p.PostingDate {
		return errDate
	}
	if len(p.Lines) < 2 {
		return errFewLines
	}
	if len(p.Lines) > MaxPayloadLines {
		return errTooManyLines
	}
	var debit, credit int64
	for _, l := range p.Lines {
		if strings.TrimSpace(l.Account) == "" || validText(l.Account, MaxAccountRunes, false) != nil {
			return errAccount
		}
		dr, cr := int64(l.DebitPaise), int64(l.CreditPaise)
		switch {
		case dr < 0 || cr < 0:
			return errLineAmounts
		case dr > 0 && cr > 0, dr == 0 && cr == 0:
			return errLineAmounts
		}
		if dr > math.MaxInt64-debit || cr > math.MaxInt64-credit {
			return errOverflow
		}
		debit += dr
		credit += cr
	}
	if debit == 0 || debit != credit {
		return errUnbalanced
	}
	if validText(strings.TrimSpace(p.Remark), MaxRemarkRunes, false) != nil {
		return errRemark
	}
	return nil
}

// validText checks s is valid UTF-8 of at most maxRunes runes without
// control characters, and non-empty when required.
func validText(s string, maxRunes int, required bool) error {
	if required && s == "" {
		return errors.New("empty")
	}
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8")
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return errors.New("too long")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("control character")
		}
	}
	return nil
}
