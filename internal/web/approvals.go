package web

// Maker-checker approvals (CC-1004): a maker turns a finding's proposed
// entry into a journal proposal; a different checker approves it, which
// posts it to ERPNext, or rejects it with a reason. The maker and checker
// are always the authenticated user (principalFrom), never a request
// field. The backend enforces separation of duties; the page only hides
// buttons.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// ApprovalsBackend is the maker-checker service as the web layer sees it.
// actor is always the authenticated username. cmd/agent adapts
// internal/approvals to it, so web never imports that package (nor the
// admin token behind it).
type ApprovalsBackend interface {
	// Propose turns the finding's proposed entry into a pending proposal.
	Propose(ctx context.Context, actor string, findingID uuid.UUID) (store.JournalProposal, error)
	// Approve approves and posts the proposal to ERPNext.
	Approve(ctx context.Context, actor string, id uuid.UUID) (ApproveResult, error)
	// Reject rejects the proposal with a reason; nothing reaches ERPNext.
	Reject(ctx context.Context, actor string, id uuid.UUID, reason string) error
	// Pending lists the proposals awaiting a checker, oldest first.
	Pending(ctx context.Context) ([]store.JournalProposal, error)
	// Get returns one proposal.
	Get(ctx context.Context, id uuid.UUID) (store.JournalProposal, error)
}

// ApproveResult is a posted proposal.
type ApproveResult struct {
	ProposalID    uuid.UUID
	Status        string
	ERPName       string
	AlreadyPosted bool
}

// Kinds of approval refusal. A backend returns an error that matches one of
// them with errors.Is (an *ApprovalError carries a user-safe message);
// anything else is a 500.
var (
	ErrApprovalInvalid    = errors.New("invalid request")              // 400
	ErrApprovalForbidden  = errors.New("forbidden")                    // 403
	ErrApprovalNotFound   = errors.New(notFoundMsg)                    // 404
	ErrApprovalConflict   = errors.New("conflict")                     // 409
	ErrApprovalPostFailed = errors.New("posting to ERPNext failed")    // 502
	errApprovalsOff       = errors.New("approvals are not configured") // 501
	errApprovalInternal   = errors.New("internal error")               // 500
)

// ApprovalError is a refused approval request: Kind is one of the
// ErrApproval* kinds and Message a fixed, user-safe explanation.
type ApprovalError struct {
	Kind    error
	Message string
}

func (e *ApprovalError) Error() string { return e.Message }

// Unwrap returns the kind.
func (e *ApprovalError) Unwrap() error { return e.Kind }

// approvalsBodyLimit caps the body of every approvals request.
const approvalsBodyLimit = 4 << 10

// approveWriteTimeout is how long an approve may take to answer: the
// post's own deadline is 100 s, longer than the server's write timeout.
const approveWriteTimeout = 110 * time.Second

// capBody caps the request body at n bytes before anything reads it (the
// CSRF check parses a form body).
func capBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

// wantsJSON reports whether to answer JSON: an /api/ path or an Accept
// header asking for it.
func wantsJSON(r *http.Request) bool {
	if isAPI(r) {
		return true
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err == nil && mt == "application/json" {
			return true
		}
	}
	return false
}

// approvalsRoute answers 501 until Options.Approvals is set; the role check
// has run by then.
func (s *Server) approvalsRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.approvals == nil {
			if wantsJSON(r) {
				writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
				return
			}
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		h(w, r)
	})
}

// approvalStatus maps a backend error to its status code and the message
// the user sees.
func approvalStatus(err error) (int, string) {
	msg := func(kind error) string {
		var e *ApprovalError
		if errors.As(err, &e) && e.Message != "" {
			return e.Message
		}
		return kind.Error()
	}
	switch {
	case errors.Is(err, ErrApprovalInvalid):
		return http.StatusBadRequest, msg(ErrApprovalInvalid)
	case errors.Is(err, ErrApprovalForbidden):
		return http.StatusForbidden, msg(ErrApprovalForbidden)
	case errors.Is(err, ErrApprovalNotFound):
		return http.StatusNotFound, msg(ErrApprovalNotFound)
	case errors.Is(err, ErrApprovalConflict):
		return http.StatusConflict, msg(ErrApprovalConflict)
	case errors.Is(err, ErrApprovalPostFailed):
		return http.StatusBadGateway, msg(ErrApprovalPostFailed)
	case errors.Is(err, errApprovalsOff):
		return http.StatusNotImplemented, errApprovalsOff.Error()
	}
	return http.StatusInternalServerError, errApprovalInternal.Error()
}

// approvalFailed logs an unexpected error (refusals are on the audit log
// already) and returns the status and message to show.
func (s *Server) approvalFailed(r *http.Request, action string, id uuid.UUID, err error) (int, string) {
	status, msg := approvalStatus(err)
	if status == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "web: approvals", "action", action, "id", id.String(), "err", err)
	}
	return status, msg
}

// pathUUID parses the {id} path value; ok is false (and 404 written) when
// it isn't a UUID.
func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		if wantsJSON(r) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundMsg})
		} else {
			http.Error(w, notFoundMsg, http.StatusNotFound)
		}
		return uuid.Nil, false
	}
	return id, true
}

// --- propose ---

// handleProposalCreate serves POST /api/findings/{id}/proposals and the
// finding page's form, POST /findings/{id}/proposals. The maker is the
// signed-in user; any body is ignored.
func (s *Server) handleProposalCreate(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	findingID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	prop, err := s.approvals.Propose(r.Context(), p.user.Name, findingID)
	if wantsJSON(r) {
		if err != nil {
			status, msg := s.approvalFailed(r, "propose", findingID, err)
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"proposal_id": prop.ID.String(), "status": prop.Status})
		return
	}
	if err != nil {
		status, msg := s.approvalFailed(r, "propose", findingID, err)
		s.findingError(w, r, p, status, msg)
		return
	}
	http.Redirect(w, r, "/findings/"+findingID.String(), http.StatusSeeOther)
}

// findingError shows msg on the finding page when it can be loaded, as
// plain text otherwise.
func (s *Server) findingError(w http.ResponseWriter, r *http.Request, p *principal, status int, msg string) {
	if s.runs == nil || s.runs.b.Findings == nil || status == http.StatusNotFound {
		http.Error(w, msg, status)
		return
	}
	f, run, ok := s.loadFinding(w, r)
	if !ok {
		return
	}
	s.renderFinding(w, r, status, p, f, run, msg, "")
}

// --- approve ---

// handleProposalApprove serves POST /api/proposals/{id}/approve and the
// approvals page's form. The checker is the signed-in user.
func (s *Server) handleProposalApprove(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	// The post may outlast the server's write timeout. A recorder in tests
	// can't set deadlines; that is fine.
	_ = http.NewResponseController(w).SetWriteDeadline(s.now().Add(approveWriteTimeout))
	res, err := s.approvals.Approve(r.Context(), p.user.Name, id)
	if wantsJSON(r) {
		if err != nil {
			status, msg := s.approvalFailed(r, "approve", id, err)
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		link, _ := erpURL(s.erpBase, "journal-entry", res.ERPName)
		writeJSON(w, http.StatusOK, map[string]any{
			"proposal_id": id.String(), "status": res.Status, "erp_name": res.ERPName,
			"erp_link": link, "already_posted": res.AlreadyPosted,
		})
		return
	}
	if err != nil {
		status, msg := s.approvalFailed(r, "approve", id, err)
		s.renderApprovals(w, r, status, p, msg, uuid.Nil)
		return
	}
	http.Redirect(w, r, "/approvals?posted="+id.String(), http.StatusSeeOther)
}

// --- reject ---

type rejectRequest struct {
	Reason string `json:"reason"`
}

// handleProposalReject serves POST /api/proposals/{id}/reject (JSON body
// {"reason": ...} or a form) and the approvals page's form. The checker is
// the signed-in user; other fields are ignored.
func (s *Server) handleProposalReject(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	reason, err := rejectReason(r)
	if err == nil {
		err = s.approvals.Reject(r.Context(), p.user.Name, id, reason)
	}
	if wantsJSON(r) {
		if err != nil {
			status, msg := s.approvalFailed(r, "reject", id, err)
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"proposal_id": id.String(), "status": "rejected"})
		return
	}
	if err != nil {
		status, msg := s.approvalFailed(r, "reject", id, err)
		s.renderApprovals(w, r, status, p, msg, uuid.Nil)
		return
	}
	http.Redirect(w, r, "/approvals", http.StatusSeeOther)
}

// rejectReason reads the reason of a JSON or form body. The backend checks
// it; an unreadable or oversized body is a 400.
func rejectReason(r *http.Request) (string, error) {
	bad := &ApprovalError{Kind: ErrApprovalInvalid, Message: "the body must carry a reason of at most 500 characters"}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "application/json" {
		var in rejectRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			return "", bad
		}
		return in.Reason, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", bad
	}
	return r.PostFormValue("reason"), nil
}

// --- page ---

// handleApprovalsPage serves GET /approvals: the proposals awaiting a
// checker. ?posted=<id> names a proposal just posted, to show its entry.
func (s *Server) handleApprovalsPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	posted, _ := uuid.Parse(r.URL.Query().Get("posted"))
	s.renderApprovals(w, r, http.StatusOK, p, "", posted)
}

func (s *Server) renderApprovals(w http.ResponseWriter, r *http.Request, status int, p *principal, msg string, posted uuid.UUID) {
	ctx := r.Context()
	list, err := s.approvals.Pending(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "web: approvals: pending", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	me := canonicalName(p.user.Name)
	page := templates.ApprovalsPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf, Error: msg,
	}
	for _, x := range list {
		page.Rows = append(page.Rows, s.approvalRow(x, me))
	}
	if posted != uuid.Nil {
		if x, err := s.approvals.Get(ctx, posted); err == nil && x.Status == "posted" && x.ERPName != nil {
			page.Posted = &templates.PostedNotice{ProposalID: shortID(x.ID), ERPName: *x.ERPName, ID: x.ID.String()}
			if link, ok := erpURL(s.erpBase, "journal-entry", *x.ERPName); ok {
				page.Posted.ERPLink = link
			}
		}
	}
	s.render(w, r, status, templates.Approvals(page))
}

// canonicalName is how the backend compares maker and checker: trimmed and
// lower-cased. Here it only decides which buttons to show.
func canonicalName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func shortID(id uuid.UUID) string { return id.String()[:8] }

func (s *Server) approvalRow(x store.JournalProposal, me string) templates.ApprovalRow {
	row := templates.ApprovalRow{
		ID: x.ID.String(), ShortID: shortID(x.ID), Company: x.CompanyID, PostingDate: x.Payload.PostingDate,
		Remark: x.Payload.Remark, Maker: x.Maker, Status: x.Status,
		Created: x.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"),
	}
	if x.FindingID != nil {
		row.FindingID = x.FindingID.String()
	}
	var debit int64
	for _, l := range x.Payload.Lines {
		debit += int64(l.DebitPaise)
		row.Lines = append(row.Lines, templates.ProposalLine{Account: l.Account, Debit: l.DebitPaise.Format(), Credit: l.CreditPaise.Format()})
	}
	row.Total = money.Paise(debit).Format()
	// The totals line and balance notice come from the finding page's view.
	pv := s.proposalView(&x)
	row.TotalCredit, row.Balanced, row.Difference = pv.TotalCredit, pv.Balanced, pv.Difference
	row.Own = canonicalName(x.Maker) == me
	checker := ""
	if x.Checker != nil {
		checker = *x.Checker
	}
	switch x.Status {
	case "proposed":
		row.CanApprove, row.CanReject = !row.Own, !row.Own
	case "approved":
		row.StatusNote = "approved, posting failed; retry by " + checker
		row.CanApprove = !row.Own && canonicalName(checker) == me
	}
	return row
}
