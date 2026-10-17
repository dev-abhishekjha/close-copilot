package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// FindingsStore is the slice of *store.Store the finding pages use.
type FindingsStore interface {
	GetFinding(ctx context.Context, id uuid.UUID) (store.Finding, error)
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
	ListSteps(ctx context.Context, runID uuid.UUID) ([]store.Step, error)
	GetArtifact(ctx context.Context, sha string) (store.Artifact, error)
	SetFindingStatus(ctx context.Context, findingID uuid.UUID, status string) error
	InsertAuditLog(ctx context.Context, e store.AuditLogEntry) (int64, error)
	// GetProposalByFinding returns the finding's live journal proposal
	// (any status but rejected); ok is false when there is none (CC-1004).
	GetProposalByFinding(ctx context.Context, findingID uuid.UUID) (store.JournalProposal, bool, error)
}

var _ FindingsStore = (*store.Store)(nil)

const maxReasonRunes = 500

// liveProposalMsg refuses accept and dismiss of a finding with a live
// journal proposal.
const liveProposalMsg = "this finding has a live proposal, so it can't be accepted or dismissed"

// findingsHTML answers 501 until both Options.Runs and Runs.Findings are
// set; the role check has run by then.
func (s *Server) findingsHTML(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runs == nil || s.runs.b.Findings == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		h(w, r)
	})
}

func (s *Server) findingErr(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "web: finding", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// loadFinding reads the finding and its run named by the {id} path value.
// ok is false when a response (404 or 500) has been written already.
func (s *Server) loadFinding(w http.ResponseWriter, r *http.Request) (store.Finding, store.CloseRun, bool) {
	fs := s.runs.b.Findings
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return store.Finding{}, store.CloseRun{}, false
	}
	f, err := fs.GetFinding(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return store.Finding{}, store.CloseRun{}, false
	}
	if err != nil {
		s.findingErr(w, r, err)
		return store.Finding{}, store.CloseRun{}, false
	}
	run, err := fs.GetCloseRun(r.Context(), f.RunID)
	if errors.Is(err, store.ErrNotFound) {
		s.log.ErrorContext(r.Context(), "web: finding has no run", "finding", f.ID.String(), "run", f.RunID.String())
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return store.Finding{}, store.CloseRun{}, false
	}
	if err != nil {
		s.findingErr(w, r, err)
		return store.Finding{}, store.CloseRun{}, false
	}
	return f, run, true
}

func decided(status string) bool {
	return status == "accepted" || status == "dismissed"
}

func (s *Server) handleFindingPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	f, run, ok := s.loadFinding(w, r)
	if !ok {
		return
	}
	s.renderFinding(w, r, http.StatusOK, p, f, run, "", "")
}

func (s *Server) renderFinding(w http.ResponseWriter, r *http.Request, status int, p *principal, f store.Finding, run store.CloseRun, msg, reason string) {
	ctx := r.Context()
	fs := s.runs.b.Findings
	page := templates.FindingPage{
		User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf,
		ID: f.ID.String(), RunID: run.ID.String(), Company: run.CompanyID, Month: run.Month,
		Type: f.Type, Severity: f.Severity, Title: f.Title, Status: f.Status, Verified: f.Verified,
	}
	if f.AmountPaise != nil {
		page.Amount = f.AmountPaise.Format()
	}
	if f.Explanation != nil {
		page.HasExplanation = true
		page.Explanation = *f.Explanation
		if f.Action != nil {
			page.Action = *f.Action
		}
	}
	if f.Status == "needs_review" || (!f.Verified && f.Explanation != nil) {
		page.Review = s.reviewBanner(r, fs, f)
	}
	for _, c := range f.Citations {
		page.Citations = append(page.Citations, templates.CitationView{DocID: c.DocID, Section: c.Section})
	}

	snaps := map[string]loadedSnapshot{}
	_, investigated := f.Keys["resolution"]
	for _, ref := range f.Evidence {
		var snap *snapshot
		var lerr error
		count := "n/a"
		if ref.Artifact != "" {
			l, seen := snaps[ref.Artifact]
			if !seen {
				l = s.loadSnapshot(ctx, fs, f.ID, ref.Artifact)
				snaps[ref.Artifact] = l
			}
			snap, lerr = l.snap, l.err
			if lerr == nil && snap != nil && snap.Server == ref.Server && snap.Tool == ref.Tool {
				count = itoaInt(len(allRecords(snap.Result)))
			}
		}
		page.Evidence = append(page.Evidence, evidenceBlock(s.erpBase, ref, snap, lerr))
		if investigated {
			page.Trail = append(page.Trail, templates.TrailLine{
				Call: ref.Server + "." + ref.Tool, Args: compactArgs(ref.Args), Rows: count,
			})
		}
	}
	page.HasTrail = investigated

	// Once a journal proposal exists it replaces the explainer's preview:
	// its status, maker and, when posted, the ERPNext entry. A failed
	// lookup hides the create form; the server refuses a second proposal
	// anyway.
	live, hasLive, lerr := fs.GetProposalByFinding(ctx, f.ID)
	if lerr != nil {
		s.log.WarnContext(ctx, "web: live proposal lookup", "finding", f.ID.String(), "err", lerr)
	}
	switch {
	case hasLive:
		page.Proposal = s.proposalView(&live)
	case f.Proposal != nil:
		page.Proposal = s.proposalView(f.Proposal)
	}
	if p.user.Role == RoleMaker && !decided(f.Status) {
		// A live proposal holds the finding: accept and dismiss wait until
		// it is rejected (decideFinding refuses them too).
		page.Actions.CanDecide = lerr == nil && !hasLive
		page.Actions.CreateButton = s.approvals != nil && lerr == nil && !hasLive &&
			f.Proposal != nil && f.Verified && f.Status == "open"
	}
	page.Actions.Error = msg
	page.Actions.Reason = reason
	s.render(w, r, status, templates.Finding(page))
}

func itoaInt(n int) string { return fmt.Sprint(n) }

type loadedSnapshot struct {
	snap *snapshot
	err  error
}

// loadSnapshot reads one tool-result artifact. A failure is logged without
// the content and shown as "snapshot unavailable".
func (s *Server) loadSnapshot(ctx context.Context, fs FindingsStore, finding uuid.UUID, sha string) loadedSnapshot {
	a, err := fs.GetArtifact(ctx, sha)
	var snap snapshot
	if err == nil {
		snap, err = decodeSnapshot(a.Content)
	}
	if err != nil {
		short := sha
		if len(short) > 12 {
			short = short[:12]
		}
		s.log.WarnContext(ctx, "web: snapshot unavailable", "finding", finding.String(), "artifact", short, "kind", errKind(err))
		return loadedSnapshot{err: err}
	}
	return loadedSnapshot{snap: &snap}
}

func errKind(err error) string {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrArtifactMismatch):
		return "mismatch"
	}
	return "unreadable"
}

// verdict is the part of the verifier's verdict artifact the page reads.
type verdict struct {
	Pass       bool `json:"pass"`
	Violations []struct {
		Code       string `json:"code"`
		Field      string `json:"field"`
		ValuePaise *int64 `json:"value_paise"`
		Detail     string `json:"detail"`
	} `json:"violations"`
}

const noReasons = "the verifier recorded no reasons"

func (s *Server) reviewBanner(r *http.Request, fs FindingsStore, f store.Finding) *templates.ReviewBanner {
	ctx := r.Context()
	b := &templates.ReviewBanner{Fallback: noReasons}
	steps, err := fs.ListSteps(ctx, f.RunID)
	if err != nil {
		s.log.WarnContext(ctx, "web: list steps", "finding", f.ID.String(), "err", err)
		return b
	}
	var step *store.Step
	for i := range steps {
		st := &steps[i]
		if st.Kind == store.StepKindVerify && st.Subject == f.ID.String() && (step == nil || st.Attempt >= step.Attempt) {
			step = st
		}
	}
	if step == nil {
		return b
	}
	if step.Status == store.StepSkipped {
		b.Fallback = "not verified: step skipped"
		return b
	}
	for _, ref := range step.OutputRefs {
		a, err := fs.GetArtifact(ctx, ref)
		if err != nil || a.Kind != store.ArtifactVerdict {
			continue
		}
		var v verdict
		if err := json.Unmarshal(a.Content, &v); err != nil {
			continue
		}
		for _, x := range v.Violations {
			line := x.Code + " · " + x.Field + " · " + x.Detail
			if x.ValuePaise != nil {
				line += " · " + money.Paise(*x.ValuePaise).Format()
			}
			b.Reasons = append(b.Reasons, line)
		}
		return b
	}
	return b
}

func (s *Server) proposalView(p *store.JournalProposal) *templates.ProposalView {
	v := &templates.ProposalView{
		Status: p.Status, Maker: p.Maker,
		PostingDate: p.Payload.PostingDate, Remark: p.Payload.Remark,
	}
	var debit, credit int64
	for _, l := range p.Payload.Lines {
		debit += int64(l.DebitPaise)
		credit += int64(l.CreditPaise)
		v.Lines = append(v.Lines, templates.ProposalLine{
			Account: l.Account, Debit: l.DebitPaise.Format(), Credit: l.CreditPaise.Format(),
		})
	}
	v.TotalDebit = money.Paise(debit).Format()
	v.TotalCredit = money.Paise(credit).Format()
	v.Balanced = debit == credit
	if !v.Balanced {
		d := debit - credit
		if d < 0 {
			d = -d
		}
		v.Difference = money.Paise(d).Format()
	}
	if p.ERPName != nil && *p.ERPName != "" {
		v.ERPName = *p.ERPName
		if u, ok := erpURL(s.erpBase, "journal-entry", v.ERPName); ok {
			v.ERPLink = u
		}
	}
	return v
}

// validateReason trims the reason and checks it is 1 to 500 runes with no
// control characters other than newline.
func validateReason(raw string) (string, error) {
	reason := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if reason == "" {
		return "", errors.New("a reason is required")
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return "", fmt.Errorf("the reason must be at most %d characters", maxReasonRunes)
	}
	for _, c := range reason {
		if c != '\n' && unicode.IsControl(c) {
			return "", errors.New("the reason must not contain control characters")
		}
	}
	return reason, nil
}

func (s *Server) handleFindingAccept(w http.ResponseWriter, r *http.Request) {
	s.decideFinding(w, r, "accepted", "finding.accept")
}

func (s *Server) handleFindingDismiss(w http.ResponseWriter, r *http.Request) {
	s.decideFinding(w, r, "dismissed", "finding.dismiss")
}

// decideKind says how decideOne ended.
type decideKind int

const (
	decideDone      decideKind = iota // status changed (the audit row is best effort)
	decideNotFound                    // no such finding, or its run is missing
	decideDecided                     // not open or needs_review any more
	decideLive                        // a live journal proposal holds the finding
	decideBadReason                   // a dismissal without a valid reason
	decideFailed                      // a store error (Err is set)
)

// decision is decideOne's result. Finding and Run are set once the finding
// has loaded; RawReason echoes a dismissal reason that was refused.
type decision struct {
	Kind      decideKind
	Finding   store.Finding
	Run       store.CloseRun
	Msg       string
	RawReason string
	Err       error
}

// decideOne is the one place a finding is accepted or dismissed: the single
// accept and dismiss handlers and the bulk accept all call it, so the open or
// needs-review rule, the live-proposal rule and the audit entry apply to
// every finding the same way. It loads the finding afresh, so a stale list
// can't decide a finding that has moved on.
func (s *Server) decideOne(ctx context.Context, p *principal, id uuid.UUID, to, tool, rawReason string) decision {
	fs := s.runs.b.Findings
	f, err := fs.GetFinding(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return decision{Kind: decideNotFound}
	}
	if err != nil {
		return decision{Kind: decideFailed, Err: err}
	}
	run, err := fs.GetCloseRun(ctx, f.RunID)
	if errors.Is(err, store.ErrNotFound) {
		s.log.ErrorContext(ctx, "web: finding has no run", "finding", f.ID.String(), "run", f.RunID.String())
		return decision{Kind: decideNotFound}
	}
	if err != nil {
		return decision{Kind: decideFailed, Err: err}
	}
	d := decision{Finding: f, Run: run}
	if f.Status != "open" && f.Status != "needs_review" {
		d.Kind, d.Msg = decideDecided, "this finding is already "+f.Status
		return d
	}
	// A finding with a live journal proposal (any status but rejected) can't
	// be accepted or dismissed: the proposal decides it. A failed lookup
	// refuses too.
	_, hasLive, err := fs.GetProposalByFinding(ctx, f.ID)
	if err != nil {
		d.Kind, d.Err = decideFailed, err
		return d
	}
	if hasLive {
		d.Kind, d.Msg = decideLive, liveProposalMsg
		return d
	}
	args := map[string]string{
		"finding_id": f.ID.String(), "run_id": f.RunID.String(), "from": f.Status, "to": to,
	}
	if to == "dismissed" {
		reason, err := validateReason(rawReason)
		if err != nil {
			d.Kind, d.Msg, d.RawReason = decideBadReason, err.Error(), rawReason
			return d
		}
		args["reason"] = reason
	}
	if err := fs.SetFindingStatus(ctx, f.ID, to); err != nil {
		d.Kind, d.Err = decideFailed, err
		return d
	}
	// The status change and the audit row are not atomic: the store has no
	// transaction for both. A failed audit insert is logged, not retried.
	raw, _ := json.Marshal(args) // a map of strings always marshals
	if _, err := fs.InsertAuditLog(ctx, store.AuditLogEntry{Actor: p.user.Name, Tool: &tool, Args: raw}); err != nil {
		s.log.ErrorContext(ctx, "web: finding audit row not written", "finding", f.ID.String(), "tool", tool, "err", err)
	}
	d.Kind = decideDone
	return d
}

func (s *Server) decideFinding(w http.ResponseWriter, r *http.Request, to, tool string) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return
	}
	var raw string
	if to == "dismissed" {
		raw = r.PostFormValue("reason")
	}
	d := s.decideOne(r.Context(), p, id, to, tool, raw)
	switch d.Kind {
	case decideNotFound:
		http.Error(w, notFoundMsg, http.StatusNotFound)
	case decideFailed:
		s.findingErr(w, r, d.Err)
	case decideDecided, decideLive:
		s.renderFinding(w, r, http.StatusConflict, p, d.Finding, d.Run, d.Msg, "")
	case decideBadReason:
		s.renderFinding(w, r, http.StatusBadRequest, p, d.Finding, d.Run, d.Msg, d.RawReason)
	default:
		http.Redirect(w, r, "/findings/"+d.Finding.ID.String(), http.StatusSeeOther)
	}
}
