package fixture

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// Approvals is the in-memory web.ApprovalsBackend. Maker-checker holds: the
// approver may not be the proposer.
type Approvals struct{ s *Store }

var _ web.ApprovalsBackend = (*Approvals)(nil)

func refuse(kind error, msg string) error { return &web.ApprovalError{Kind: kind, Message: msg} }

// Propose implements web.ApprovalsBackend.
func (a Approvals) Propose(_ context.Context, actor string, findingID uuid.UUID) (store.JournalProposal, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.findings[findingID]
	switch {
	case !ok:
		return store.JournalProposal{}, refuse(web.ErrApprovalNotFound, "not found")
	case f.Status != "open" || !f.Verified:
		return store.JournalProposal{}, refuse(web.ErrApprovalConflict, "only a verified, open finding can be proposed")
	case f.Proposal == nil:
		return store.JournalProposal{}, refuse(web.ErrApprovalInvalid, "this finding has no proposed entry")
	}
	for _, p := range s.proposals {
		if p.FindingID != nil && *p.FindingID == findingID && p.Status != "rejected" {
			return store.JournalProposal{}, refuse(web.ErrApprovalConflict, "this finding already has a proposal")
		}
	}
	run := s.runs[f.RunID]
	p := store.JournalProposal{ID: uuid.New(), FindingID: &findingID, CompanyID: run.CompanyID,
		Payload: f.Proposal.Payload, Status: "proposed", Maker: actor, CreatedAt: s.tick()}
	s.proposals[p.ID] = p
	s.addAudit(store.AuditLogEntry{Actor: actor, Server: str("approvals"), Tool: str("propose_entry"),
		Args: raw(map[string]any{"finding": findingID.String()})})
	return p, nil
}

// Approve implements web.ApprovalsBackend.
func (a Approvals) Approve(_ context.Context, actor string, id uuid.UUID) (web.ApproveResult, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	switch {
	case !ok:
		return web.ApproveResult{}, refuse(web.ErrApprovalNotFound, "not found")
	case canonical(actor) == canonical(p.Maker):
		return web.ApproveResult{}, refuse(web.ErrApprovalForbidden, "you proposed this entry, so a different checker must approve it")
	case p.Status == "posted":
		return web.ApproveResult{ProposalID: id, Status: "posted", ERPName: *p.ERPName, AlreadyPosted: true}, nil
	case p.Status != "proposed":
		return web.ApproveResult{}, refuse(web.ErrApprovalConflict, "this proposal is not waiting for a decision")
	}
	now := s.tick()
	name := fmt.Sprintf("ACC-JV-2026-%05d", len(s.proposals)+10)
	p.Status, p.Checker, p.ERPName, p.DecidedAt, p.PostedAt = "posted", str(actor), &name, &now, &now
	s.proposals[id] = p
	if p.FindingID != nil {
		_ = s.setStatus(*p.FindingID, "accepted")
	}
	s.addAudit(store.AuditLogEntry{Actor: actor, Server: str("approvals"), Tool: str("approve_proposal"),
		Args: raw(map[string]any{"proposal": id.String(), "erp_name": name})})
	return web.ApproveResult{ProposalID: id, Status: "posted", ERPName: name}, nil
}

// Reject implements web.ApprovalsBackend.
func (a Approvals) Reject(_ context.Context, actor string, id uuid.UUID, reason string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[id]
	switch {
	case !ok:
		return refuse(web.ErrApprovalNotFound, "not found")
	case reason == "":
		return refuse(web.ErrApprovalInvalid, "a reason is required")
	case p.Status != "proposed":
		return refuse(web.ErrApprovalConflict, "this proposal is not waiting for a decision")
	}
	now := s.tick()
	p.Status, p.Checker, p.Reason, p.DecidedAt = "rejected", str(actor), &reason, &now
	s.proposals[id] = p
	s.addAudit(store.AuditLogEntry{Actor: actor, Server: str("approvals"), Tool: str("reject_proposal"),
		Args: raw(map[string]any{"proposal": id.String()})})
	return nil
}

// Pending implements web.ApprovalsBackend: oldest first.
func (a Approvals) Pending(context.Context) ([]store.JournalProposal, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.JournalProposal
	for _, p := range s.proposals {
		if p.Status == "proposed" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Get implements web.ApprovalsBackend.
func (a Approvals) Get(_ context.Context, id uuid.UUID) (store.JournalProposal, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	p, ok := a.s.proposals[id]
	if !ok {
		return store.JournalProposal{}, refuse(web.ErrApprovalNotFound, "not found")
	}
	return p, nil
}
