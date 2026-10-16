package web

// The proposal page (UI-09): one journal proposal with who proposed,
// decided and posted it. It only reads; the decision controls are the
// approvals list's, built by approvalRow, and post to the same routes,
// which enforce every rule.

import (
	"net/http"
	"time"

	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// handleProposalPage serves GET /proposals/{id} for any signed-in role.
func (s *Server) handleProposalPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	x, err := s.approvals.Get(r.Context(), id)
	if err != nil {
		status, msg := s.approvalFailed(r, "get", id, err)
		http.Error(w, msg, status)
		return
	}
	s.render(w, r, http.StatusOK, templates.Proposal(s.proposalPage(p, x)))
}

// proposalPage builds the page's view. Controls are shown only to a
// checker and only while the proposal awaits a decision (proposed, or
// approved with a failed post); which ones is approvalRow's to say.
func (s *Server) proposalPage(p *principal, x store.JournalProposal) templates.ProposalPage {
	page := templates.ProposalPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		Row:      s.approvalRow(x, canonicalName(p.user.Name)),
		Entry:    *s.proposalView(&x),
		Timeline: proposalTimeline(x),
	}
	if x.Checker != nil {
		page.Checker = *x.Checker
	}
	isChecker := p.user.Role == RoleChecker
	page.ShowControls = isChecker && (x.Status == "proposed" || x.Status == "approved")
	switch {
	case isChecker:
		page.BackHref, page.BackLabel = "/approvals", "Back to approvals"
	case x.FindingID != nil:
		page.BackHref, page.BackLabel = "/findings/"+x.FindingID.String(), "Back to finding"
	}
	return page
}

// proposalTimeline lists what happened to the proposal, oldest first. A
// step the status implies is listed even when its time is missing; the
// time is then left out, never guessed.
func proposalTimeline(x store.JournalProposal) []templates.TimelineEntry {
	at := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	checker := ""
	if x.Checker != nil && *x.Checker != "" {
		checker = "by " + *x.Checker
	}
	out := []templates.TimelineEntry{{Title: "Proposed", Actor: "by " + x.Maker, Time: at(&x.CreatedAt), State: "done"}}
	switch x.Status {
	case "approved", "posted":
		out = append(out, templates.TimelineEntry{Title: "Approved", Actor: checker, Time: at(x.DecidedAt), State: "done"})
	case "rejected":
		e := templates.TimelineEntry{Title: "Rejected", Actor: checker, Time: at(x.DecidedAt), State: "failed"}
		if x.Reason != nil {
			e.Detail = "Reason: " + *x.Reason
		}
		out = append(out, e)
	}
	if x.Status == "posted" {
		e := templates.TimelineEntry{Title: "Posted", Time: at(x.PostedAt), State: "done"}
		if x.ERPName != nil && *x.ERPName != "" {
			e.Detail = "Journal entry " + *x.ERPName
		}
		out = append(out, e)
	}
	return out
}
