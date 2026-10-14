package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ---- fakes ----

// fakeStore is an in-memory journal_proposals table and audit log. Its
// save refuses what only the admin tool may write.
type fakeStore struct {
	t         *testing.T
	mu        sync.Mutex
	lock      sync.Mutex // the row lock (one for all rows is enough here)
	rows      map[uuid.UUID]store.JournalProposal
	audits    []store.AuditLogEntry
	auditCtx  []ctxSeen // the ctx of each InsertAuditLog, as seen then
	saves     int
	createErr error
	// findings is what WithProposalFindingLock reads; findingReads counts
	// those reads.
	findings     *fakeFindings
	findingReads int
}

func newFakeStore(t *testing.T) *fakeStore {
	return &fakeStore{t: t, rows: map[uuid.UUID]store.JournalProposal{}}
}

func (s *fakeStore) CreateProposalFromFinding(_ context.Context, p store.JournalProposal) (store.JournalProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return store.JournalProposal{}, s.createErr
	}
	for _, r := range s.rows {
		if r.FindingID != nil && p.FindingID != nil && *r.FindingID == *p.FindingID && r.Status != StatusRejected {
			return r, fmt.Errorf("%w: finding %s", store.ErrProposalExists, *p.FindingID)
		}
	}
	s.rows[p.ID] = p
	return p, nil
}

func (s *fakeStore) GetJournalProposal(_ context.Context, id uuid.UUID) (store.JournalProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[id]
	if !ok {
		return store.JournalProposal{}, fmt.Errorf("%w: journal proposal %s", store.ErrNotFound, id)
	}
	return p, nil
}

func (s *fakeStore) ListProposalsByStatus(_ context.Context, status string, _ int) ([]store.JournalProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.JournalProposal
	for _, p := range s.rows {
		if p.Status == status {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *fakeStore) WithProposalLock(ctx context.Context, id uuid.UUID, fn func(context.Context, store.JournalProposal, func(context.Context, store.JournalProposal) error) error) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.mu.Lock()
	orig, ok := s.rows[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: journal proposal %s", store.ErrNotFound, id)
	}
	pending := orig
	saved := false
	save := func(_ context.Context, np store.JournalProposal) error {
		if np.Status == StatusPosted {
			s.t.Errorf("service saved status posted for %s; only the admin tool may", id)
		}
		if !samePtr(np.ERPName, orig.ERPName) || !sameTime(np.PostedAt, orig.PostedAt) {
			s.t.Errorf("service changed erp_name or posted_at of %s", id)
		}
		if np.Maker != orig.Maker || np.CompanyID != orig.CompanyID || len(np.Payload.Lines) != len(orig.Payload.Lines) {
			s.t.Errorf("service changed maker, company or payload of %s", id)
		}
		pending, saved = np, true
		return nil
	}
	if err := fn(ctx, orig, save); err != nil {
		return err
	}
	if saved {
		s.mu.Lock()
		s.rows[id] = pending
		s.saves++
		s.mu.Unlock()
	}
	return nil
}

// WithProposalFindingLock reads the finding's status from s.findings while
// the row lock is held, counting each read.
func (s *fakeStore) WithProposalFindingLock(ctx context.Context, id uuid.UUID, fn func(context.Context, store.JournalProposal, store.FindingStatusFunc, func(context.Context, store.JournalProposal) error) error) error {
	return s.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		read := func(context.Context) (string, bool, error) {
			s.mu.Lock()
			s.findingReads++
			s.mu.Unlock()
			if p.FindingID == nil || s.findings == nil {
				return "", false, nil
			}
			f, ok := s.findings.findings[*p.FindingID]
			if !ok {
				return "", false, nil
			}
			return f.Status, true, nil
		}
		return fn(ctx, p, read, save)
	})
}

func (s *fakeStore) InsertAuditLog(ctx context.Context, e store.AuditLogEntry) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, e)
	s.auditCtx = append(s.auditCtx, seeCtx(ctx))
	return int64(len(s.audits)), nil
}

func (s *fakeStore) row(id uuid.UUID) store.JournalProposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id]
}

func (s *fakeStore) put(p store.JournalProposal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[p.ID] = p
}

func (s *fakeStore) lastAudit(t *testing.T) (store.AuditLogEntry, map[string]any) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.audits) == 0 {
		t.Fatal("no audit row")
	}
	e := s.audits[len(s.audits)-1]
	var args map[string]any
	if err := json.Unmarshal(e.Args, &args); err != nil {
		t.Fatalf("audit args: %v", err)
	}
	return e, args
}

// ctxSeen is what a fake saw of a context when it was called.
type ctxSeen struct {
	err      error
	deadline time.Time
	has      bool
	value    any
}

type ctxKey struct{}

func seeCtx(ctx context.Context) ctxSeen {
	dl, has := ctx.Deadline()
	return ctxSeen{err: ctx.Err(), deadline: dl, has: has, value: ctx.Value(ctxKey{})}
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

type fakeFindings struct {
	findings map[uuid.UUID]store.Finding
	runs     map[uuid.UUID]store.CloseRun
}

func (f *fakeFindings) GetFinding(_ context.Context, id uuid.UUID) (store.Finding, error) {
	x, ok := f.findings[id]
	if !ok {
		return store.Finding{}, fmt.Errorf("%w: finding %s", store.ErrNotFound, id)
	}
	return x, nil
}

func (f *fakeFindings) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	x, ok := f.runs[id]
	if !ok {
		return store.CloseRun{}, fmt.Errorf("%w: run %s", store.ErrNotFound, id)
	}
	return x, nil
}

// fakePoster plays the admin tool: on success it writes posted and
// erp_name on the row, as the tool does.
type fakePoster struct {
	st    *fakeStore
	mu    sync.Mutex
	calls int
	err   error
	// onPost, when set, runs first with the ctx Post was given.
	onPost func(ctx context.Context)
}

func (p *fakePoster) Post(ctx context.Context, id uuid.UUID) (PostResult, error) {
	p.mu.Lock()
	p.calls++
	err, hook := p.err, p.onPost
	p.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if err != nil {
		return PostResult{}, err
	}
	row := p.st.row(id)
	if row.Status == StatusPosted {
		return PostResult{ProposalID: id.String(), ERPName: *row.ERPName, Status: StatusPosted, AlreadyPosted: true}, nil
	}
	name, now := "ACC-JV-2026-"+id.String()[:5], time.Now().UTC()
	row.Status, row.ERPName, row.PostedAt = StatusPosted, &name, &now
	p.st.put(row)
	return PostResult{ProposalID: id.String(), ERPName: name, Status: StatusPosted}, nil
}

func (p *fakePoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fixture struct {
	svc      *Service
	st       *fakeStore
	poster   *fakePoster
	findings *fakeFindings
	run      store.CloseRun
	logs     *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := newFakeStore(t)
	run := store.CloseRun{ID: uuid.New(), CompanyID: "sharma", Month: "2026-08", Status: "done"}
	ff := &fakeFindings{findings: map[uuid.UUID]store.Finding{}, runs: map[uuid.UUID]store.CloseRun{run.ID: run}}
	st.findings = ff
	poster := &fakePoster{st: st}
	logs := &bytes.Buffer{}
	svc := New(Deps{
		Store: st, Poster: poster, Findings: ff,
		Now:    func() time.Time { return time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	return &fixture{svc: svc, st: st, poster: poster, findings: ff, run: run, logs: logs}
}

func balanced() store.JournalPayload {
	return store.JournalPayload{PostingDate: "2026-08-31", Remark: "  SMS charges for August  ", Lines: []store.JournalLine{
		{Account: "Bank Charges - STPL", DebitPaise: 29500},
		{Account: "HDFC Bank - STPL", CreditPaise: 29500},
	}}
}

// finding adds an open, verified finding with a balanced preview; mutate
// changes it first.
func (fx *fixture) finding(mutate func(*store.Finding)) uuid.UUID {
	f := store.Finding{
		ID: uuid.New(), RunID: fx.run.ID, Type: "unrecorded_bank_charge", Severity: "medium",
		Title: "Unrecorded bank charge", Status: "open", Verified: true,
	}
	f.Proposal = &store.JournalProposal{FindingID: &f.ID, CompanyID: "sharma", Payload: balanced(), Status: StatusProposed}
	if mutate != nil {
		mutate(&f)
	}
	fx.findings.findings[f.ID] = f
	return f.ID
}

// proposal adds a pending proposal made by asha.
func (fx *fixture) proposal(t *testing.T) uuid.UUID {
	t.Helper()
	p, err := fx.svc.Propose(t.Context(), "asha", fx.finding(nil))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return p.ID
}

// ---- ValidatePayload ----

func TestValidatePayload(t *testing.T) {
	line := func(acct string, dr, cr money.Paise) store.JournalLine {
		return store.JournalLine{Account: acct, DebitPaise: dr, CreditPaise: cr}
	}
	ok2 := []store.JournalLine{line("A", 100, 0), line("B", 0, 100)}
	cases := []struct {
		name string
		p    store.JournalPayload
		code string // "" means valid
	}{
		{"valid", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "ok"}, ""},
		{"valid three lines", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 150, 0), line("B", 0, 100), line("C", 0, 50)}}, ""},
		{"valid remark trimmed to 500", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "  " + strings.Repeat("é", 500) + " "}, ""},
		{"empty date", store.JournalPayload{Lines: ok2}, "invalid_posting_date"},
		{"bad date", store.JournalPayload{PostingDate: "2026-02-30", Lines: ok2}, "invalid_posting_date"},
		{"not padded date", store.JournalPayload{PostingDate: "2026-8-31", Lines: ok2}, "invalid_posting_date"},
		{"datetime", store.JournalPayload{PostingDate: "2026-08-31T00:00:00Z", Lines: ok2}, "invalid_posting_date"},
		{"one line", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2[:1]}, "invalid_lines"},
		{"no lines", store.JournalPayload{PostingDate: "2026-08-31"}, "invalid_lines"},
		{"blank account", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("  ", 100, 0), line("B", 0, 100)}}, "invalid_account"},
		{"long account", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line(strings.Repeat("a", 141), 100, 0), line("B", 0, 100)}}, "invalid_account"},
		{"account 140", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line(strings.Repeat("a", 140), 100, 0), line("B", 0, 100)}}, ""},
		{"control account", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A\nB", 100, 0), line("B", 0, 100)}}, "invalid_account"},
		{"both sides", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 100, 100), line("B", 0, 100), line("C", 100, 0)}}, "invalid_amount"},
		{"zero line", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 100, 0), line("B", 0, 100), line("C", 0, 0)}}, "invalid_amount"},
		{"negative debit", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", -100, 0), line("B", -100, 0)}}, "invalid_amount"},
		{"negative credit", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 100, 0), line("B", 0, -100)}}, "invalid_amount"},
		{"unbalanced", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 100, 0), line("B", 0, 99)}}, "unbalanced"},
		{"all debit", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", 100, 0), line("B", 100, 0)}}, "unbalanced"},
		{"overflow", store.JournalPayload{PostingDate: "2026-08-31", Lines: []store.JournalLine{line("A", math.MaxInt64, 0), line("B", 1, 0), line("C", 0, 1)}}, "overflow"},
		{"remark 501 runes", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: strings.Repeat("r", 501)}, "invalid_remark"},
		{"remark control", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "bank\x07charge"}, "invalid_remark"},
		{"remark inner newline", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "a\nb"}, "invalid_remark"},
		{"remark invalid utf8", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "a\xffb"}, "invalid_remark"},
		{"too many lines", store.JournalPayload{PostingDate: "2026-08-31", Lines: make([]store.JournalLine, MaxPayloadLines+1)}, "invalid_lines"},
		{"remark GSTIN token", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "charge for GSTIN_0a1b2c3d"}, "pseudonym_in_remark"},
		{"remark PAN token", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_1a2b3c"}, "pseudonym_in_remark"},
		{"remark PERSON token", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "paid to PERSON_abcdef012345."}, "pseudonym_in_remark"},
		{"remark 64-hex token", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_" + strings.Repeat("f", 64)}, "pseudonym_in_remark"},
		{"remark short hex tail", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_1a2b3"}, ""},
		{"remark other prefix", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "TAN_1a2b3c"}, ""},
		{"remark lower-case prefix", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "pan_1a2b3c"}, ""},
		{"remark upper-case hex", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_1A2B3C"}, ""},
		{"remark no leading boundary", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "XPAN_1a2b3c"}, ""},
		{"remark no trailing boundary", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_1a2b3cz"}, ""},
		{"remark 65-hex tail", store.JournalPayload{PostingDate: "2026-08-31", Lines: ok2, Remark: "PAN_" + strings.Repeat("f", 65)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePayload(tc.p)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("ValidatePayload: %v, want ok", err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || !errors.Is(err, ErrInvalidProposal) || e.Code != tc.code {
				t.Fatalf("ValidatePayload: %v, want %s (ErrInvalidProposal)", err, tc.code)
			}
			if tc.p.Remark != "" && strings.Contains(err.Error(), strings.TrimSpace(tc.p.Remark)) {
				t.Errorf("message echoes the remark: %q", err.Error())
			}
		})
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{"asha": "asha", " Asha ": "asha", "RAVI\t": "ravi", "": ""} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- Propose ----

func TestPropose(t *testing.T) {
	fx := newFixture(t)
	fid := fx.finding(nil)
	p, err := fx.svc.Propose(t.Context(), "  Asha ", fid)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.ID == uuid.Nil || p.Maker != "asha" || p.Status != StatusProposed || p.CompanyID != "sharma" ||
		p.FindingID == nil || *p.FindingID != fid || p.Payload.Remark != "SMS charges for August" || len(p.Payload.Lines) != 2 ||
		p.Checker != nil || p.ERPName != nil || !p.CreatedAt.Equal(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("proposal %+v", p)
	}
	e, args := fx.st.lastAudit(t)
	if e.Actor != "asha" || *e.Server != AuditServer || *e.Tool != ToolCreate ||
		args["proposal_id"] != p.ID.String() || args["finding_id"] != fid.String() || args["result"] != "created" {
		t.Errorf("audit %+v %v", e, args)
	}
	if strings.Contains(string(e.Args), "Bank Charges") {
		t.Errorf("audit args carry payload lines: %s", e.Args)
	}

	// Twice on one finding: 409 with the existing proposal named in the audit.
	_, err = fx.svc.Propose(t.Context(), "asha", fid)
	if !errors.Is(err, ErrProposalExists) {
		t.Fatalf("second Propose: %v, want ErrProposalExists", err)
	}
	_, args = fx.st.lastAudit(t)
	if args["result"] != "refused:exists" || args["proposal_id"] != p.ID.String() {
		t.Errorf("refusal audit %v", args)
	}
}

func TestProposeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*store.Finding)
		kind   error
		code   string
	}{
		{"no preview", func(f *store.Finding) { f.Proposal = nil }, ErrNotProposable, "no_preview"},
		{"needs review", func(f *store.Finding) { f.Status = "needs_review" }, ErrNotProposable, "needs_review"},
		{"unverified", func(f *store.Finding) { f.Verified = false }, ErrNotProposable, "unverified"},
		{"unverified needs review", func(f *store.Finding) { f.Verified, f.Status = false, "needs_review" }, ErrNotProposable, "needs_review"},
		{"accepted", func(f *store.Finding) { f.Status = "accepted" }, ErrNotProposable, "accepted"},
		{"dismissed", func(f *store.Finding) { f.Status = "dismissed" }, ErrNotProposable, "dismissed"},
		{"other company", func(f *store.Finding) { f.Proposal.CompanyID = "mehta" }, ErrNotProposable, "company_mismatch"},
		{"no company", func(f *store.Finding) { f.Proposal.CompanyID = "" }, ErrNotProposable, "company_mismatch"},
		{"other finding", func(f *store.Finding) { other := uuid.New(); f.Proposal.FindingID = &other }, ErrNotProposable, "finding_mismatch"},
		{"remark control", func(f *store.Finding) { f.Proposal.Payload.Remark = "charge\x1b[31m" }, ErrInvalidProposal, "invalid_remark"},
		{"remark 501 runes", func(f *store.Finding) { f.Proposal.Payload.Remark = strings.Repeat("ç", 501) }, ErrInvalidProposal, "invalid_remark"},
		{"unbalanced", func(f *store.Finding) { f.Proposal.Payload.Lines[1].CreditPaise = 1 }, ErrInvalidProposal, "unbalanced"},
		{"remark pseudonym", func(f *store.Finding) { f.Proposal.Payload.Remark = "SMS charges, PERSON_1a2b3c4d" }, ErrInvalidProposal, "pseudonym_in_remark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fid := fx.finding(tc.mutate)
			_, err := fx.svc.Propose(t.Context(), "asha", fid)
			var e *Error
			if !errors.Is(err, tc.kind) || !errors.As(err, &e) || e.Code != tc.code {
				t.Fatalf("Propose: %v, want %v/%s", err, tc.kind, tc.code)
			}
			if len(fx.st.rows) != 0 {
				t.Errorf("a refused proposal was stored")
			}
			_, args := fx.st.lastAudit(t)
			if args["result"] != "refused:"+tc.code {
				t.Errorf("audit result %v", args["result"])
			}
		})
	}
	t.Run("unknown finding", func(t *testing.T) {
		fx := newFixture(t)
		if _, err := fx.svc.Propose(t.Context(), "asha", uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("Propose: %v, want ErrNotFound", err)
		}
	})
	t.Run("unknown run", func(t *testing.T) {
		fx := newFixture(t)
		fid := fx.finding(func(f *store.Finding) { f.RunID = uuid.New() })
		if _, err := fx.svc.Propose(t.Context(), "asha", fid); !errors.Is(err, ErrNotFound) {
			t.Errorf("Propose: %v, want ErrNotFound", err)
		}
	})
	t.Run("store failure is not a refusal", func(t *testing.T) {
		fx := newFixture(t)
		fx.st.createErr = errors.New("db down")
		_, err := fx.svc.Propose(t.Context(), "asha", fx.finding(nil))
		var e *Error
		if err == nil || errors.As(err, &e) {
			t.Errorf("Propose: %v, want a plain error", err)
		}
		_, args := fx.st.lastAudit(t)
		if args["result"] != "error" {
			t.Errorf("audit result %v", args["result"])
		}
	})
	t.Run("no actor", func(t *testing.T) {
		fx := newFixture(t)
		if _, err := fx.svc.Propose(t.Context(), "  ", fx.finding(nil)); err == nil {
			t.Error("want an error")
		}
	})
}

// ---- Approve ----

func TestApproveSelfApprovalRefused(t *testing.T) {
	for _, actor := range []string{"asha", "Asha", " ASHA ", "\tasha\n"} {
		t.Run(actor, func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			before := fx.st.row(id)
			_, err := fx.svc.Approve(t.Context(), actor, id)
			if !errors.Is(err, ErrSelfApproval) {
				t.Fatalf("Approve: %v, want ErrSelfApproval", err)
			}
			if err.Error() != "you proposed this entry, so a different checker must approve it" {
				t.Errorf("message %q", err.Error())
			}
			if fx.poster.count() != 0 {
				t.Error("poster called on a self-approval")
			}
			if after := fx.st.row(id); after.Status != before.Status || after.Checker != nil || after.DecidedAt != nil || fx.st.saves != 0 {
				t.Errorf("row changed: %+v", after)
			}
			e, args := fx.st.lastAudit(t)
			if e.Actor != "asha" || *e.Tool != ToolApprove || args["result"] != "refused:self_approval" {
				t.Errorf("audit %+v %v", e, args)
			}
		})
	}
}

func TestApproveTwicePostsOnce(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	r1, err := fx.svc.Approve(t.Context(), "Ravi ", id)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if r1.Status != StatusPosted || r1.ERPName == "" || r1.AlreadyPosted {
		t.Errorf("first result %+v", r1)
	}
	row := fx.st.row(id)
	if row.Checker == nil || *row.Checker != "ravi" || row.DecidedAt == nil {
		t.Errorf("row %+v", row)
	}
	r2, err := fx.svc.Approve(t.Context(), "ravi", id)
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	if r2.ERPName != r1.ERPName || !r2.AlreadyPosted {
		t.Errorf("second result %+v, want %s already posted", r2, r1.ERPName)
	}
	if n := fx.poster.count(); n != 1 {
		t.Errorf("poster called %d times, want 1", n)
	}
	e, args := fx.st.lastAudit(t)
	if e.Actor != "ravi" || args["result"] != "posted" || args["proposal_id"] != id.String() {
		t.Errorf("audit %+v %v", e, args)
	}
}

func TestApproveConcurrentSameChecker(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	var wg sync.WaitGroup
	names := make([]string, 5)
	errs := make([]error, 5)
	for i := range names {
		wg.Go(func() {
			r, err := fx.svc.Approve(context.Background(), "ravi", id)
			names[i], errs[i] = r.ERPName, err
		})
	}
	wg.Wait()
	for i := range names {
		if errs[i] != nil || names[i] != names[0] || names[0] == "" {
			t.Errorf("approve %d: %q %v", i, names[i], errs[i])
		}
	}
}

func TestRejectedCantBeApproved(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	if err := fx.svc.Reject(t.Context(), "ravi", id, "wrong account"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrNotPending) {
		t.Errorf("Approve after reject: %v, want ErrNotPending", err)
	}
	if fx.poster.count() != 0 {
		t.Error("poster called for a rejected proposal")
	}
}

func TestPostedCantBeRejected(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	if _, err := fx.svc.Approve(t.Context(), "ravi", id); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.Reject(t.Context(), "ravi", id, "changed my mind"); !errors.Is(err, ErrNotPending) {
		t.Errorf("Reject posted: %v, want ErrNotPending", err)
	}
	if fx.st.row(id).Status != StatusPosted {
		t.Error("posted row changed")
	}
}

func TestApprovePostFailureAndRetry(t *testing.T) {
	cases := []struct {
		name     string
		postErr  error
		wantCode string
	}{
		{"refusal", &Refusal{Code: "erp_unavailable"}, "erp_unavailable"},
		{"transport", errors.New("connection refused; token=" + "never-shown"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			fx.poster.err = tc.postErr
			_, err := fx.svc.Approve(t.Context(), "ravi", id)
			var e *Error
			if !errors.Is(err, ErrPostFailed) || !errors.As(err, &e) || e.Code != tc.wantCode {
				t.Fatalf("Approve: %v, want ErrPostFailed/%s", err, tc.wantCode)
			}
			if strings.Contains(err.Error(), "never-shown") || strings.Contains(err.Error(), "connection refused") {
				t.Errorf("error carries the cause: %q", err.Error())
			}
			if row := fx.st.row(id); row.Status != StatusApproved || row.Checker == nil || *row.Checker != "ravi" {
				t.Errorf("row after failure %+v, want approved by ravi", row)
			}
			_, args := fx.st.lastAudit(t)
			want := "refused:" + tc.wantCode
			if tc.wantCode == "error" {
				want = "error"
			}
			if args["result"] != want {
				t.Errorf("audit result %v, want %s", args["result"], want)
			}

			// A different checker can't take over; asha still can't.
			if _, err := fx.svc.Approve(t.Context(), "meera", id); !errors.Is(err, ErrNotPending) {
				t.Errorf("other checker: %v, want ErrNotPending", err)
			}
			if _, err := fx.svc.Approve(t.Context(), "asha", id); !errors.Is(err, ErrSelfApproval) {
				t.Errorf("maker: %v, want ErrSelfApproval", err)
			}
			// An approved row can't be rejected.
			if err := fx.svc.Reject(t.Context(), "ravi", id, "stuck"); !errors.Is(err, ErrNotPending) {
				t.Errorf("reject approved: %v, want ErrNotPending", err)
			}
			if fx.poster.count() != 1 {
				t.Errorf("poster called %d times, want 1", fx.poster.count())
			}

			// The same checker retries and posts.
			fx.poster.err = nil
			r, err := fx.svc.Approve(t.Context(), " RAVI", id)
			if err != nil || r.Status != StatusPosted || r.ERPName == "" {
				t.Fatalf("retry: %+v %v", r, err)
			}
			if fx.poster.count() != 2 {
				t.Errorf("poster called %d times, want 2", fx.poster.count())
			}
		})
	}
}

// A proposal whose finding was dismissed or accepted after it was made is
// refused when first approved: the poster is never called and the row is
// unchanged. The finding's status is read under the proposal lock.
func TestApproveFindingNotOpen(t *testing.T) {
	const msg = "the finding is no longer open, so this entry can't be approved"
	for _, status := range []string{"dismissed", "accepted", "needs_review", "missing"} {
		t.Run(status+"/proposed", func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			fid := *fx.st.row(id).FindingID
			setFindingStatus(fx, fid, status)
			before, calls, saves, reads := fx.st.row(id), fx.poster.count(), fx.st.saves, fx.st.findingReads
			_, err := fx.svc.Approve(t.Context(), "ravi", id)
			var e *Error
			if !errors.Is(err, ErrFindingNotOpen) || !errors.As(err, &e) || e.Code != "finding_not_open" || err.Error() != msg {
				t.Fatalf("Approve: %v, want ErrFindingNotOpen", err)
			}
			if fx.st.findingReads != reads+1 {
				t.Errorf("finding read %d times under the lock, want 1", fx.st.findingReads-reads)
			}
			if fx.poster.count() != calls {
				t.Error("poster called for a finding that is not open")
			}
			after := fx.st.row(id)
			if after.Status != before.Status || !samePtr(after.Checker, before.Checker) || !sameTime(after.DecidedAt, before.DecidedAt) || fx.st.saves != saves {
				t.Errorf("row changed: %+v", after)
			}
			e2, args := fx.st.lastAudit(t)
			if e2.Actor != "ravi" || *e2.Tool != ToolApprove || args["result"] != "refused:finding_not_open" {
				t.Errorf("audit %+v %v", e2, args)
			}
			// Self-approval is still refused first, without reading the
			// finding.
			reads = fx.st.findingReads
			if _, err := fx.svc.Approve(t.Context(), "asha", id); !errors.Is(err, ErrSelfApproval) {
				t.Errorf("maker: %v, want ErrSelfApproval", err)
			}
			if fx.st.findingReads != reads {
				t.Error("finding read before the self-approval refusal")
			}
			// Reject is unaffected.
			if err := fx.svc.Reject(t.Context(), "ravi", id, "finding closed"); err != nil {
				t.Errorf("Reject: %v", err)
			}
		})
	}
}

// setFindingStatus changes the finding's status; "missing" deletes it.
func setFindingStatus(fx *fixture, fid uuid.UUID, status string) {
	if status == "missing" {
		delete(fx.findings.findings, fid)
		return
	}
	f := fx.findings.findings[fid]
	f.Status = status
	fx.findings.findings[fid] = f
}

// The recorded checker's retry of an approved proposal is not refused when
// its finding has since left open, so the idempotent admin tool can
// reconcile a post that already reached ERPNext. Another checker is still
// refused, and the maker still can't approve.
func TestApproveRetryAfterFindingClosed(t *testing.T) {
	for _, status := range []string{"dismissed", "accepted", "needs_review", "missing"} {
		t.Run(status, func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			// A failed first post leaves the row approved by ravi.
			fx.poster.err = &Refusal{Code: "erp_unavailable"}
			if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrPostFailed) {
				t.Fatalf("first Approve: %v", err)
			}
			fx.poster.err = nil
			setFindingStatus(fx, *fx.st.row(id).FindingID, status)

			// Self-approval first, then another checker: both refused
			// without a post.
			if _, err := fx.svc.Approve(t.Context(), "asha", id); !errors.Is(err, ErrSelfApproval) {
				t.Errorf("maker: %v, want ErrSelfApproval", err)
			}
			_, err := fx.svc.Approve(t.Context(), "meera", id)
			var e *Error
			if !errors.Is(err, ErrNotPending) || !errors.As(err, &e) || e.Code != "other_checker" {
				t.Errorf("other checker: %v, want ErrNotPending/other_checker", err)
			}
			if fx.poster.count() != 1 {
				t.Fatalf("poster called %d times, want 1", fx.poster.count())
			}

			reads := fx.st.findingReads
			r, err := fx.svc.Approve(t.Context(), "ravi", id)
			if err != nil || r.Status != StatusPosted || r.ERPName == "" {
				t.Fatalf("retry: %+v %v, want posted", r, err)
			}
			if fx.poster.count() != 2 {
				t.Errorf("poster called %d times, want 2", fx.poster.count())
			}
			if fx.st.findingReads != reads {
				t.Error("retry read the finding's status")
			}
			if row := fx.st.row(id); row.Status != StatusPosted || row.ERPName == nil || *row.ERPName != r.ERPName {
				t.Errorf("row %+v, want posted as %s", row, r.ERPName)
			}
			if _, args := fx.st.lastAudit(t); args["result"] != "posted" {
				t.Errorf("audit result %v, want posted", args["result"])
			}
		})
	}
}

// An approved row is reconciled by its checker's retry even after the
// finding was dismissed; another checker is refused; a first approval of a
// dismissed finding is still refused without a post.
func TestApproveDismissedReconcile(t *testing.T) {
	t.Run("same checker retry posts", func(t *testing.T) {
		fx := newFixture(t)
		id := fx.proposal(t)
		fx.poster.err = &Refusal{Code: "erp_unavailable"}
		if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrPostFailed) {
			t.Fatalf("first Approve: %v", err)
		}
		fx.poster.err = nil
		setFindingStatus(fx, *fx.st.row(id).FindingID, "dismissed")
		r, err := fx.svc.Approve(t.Context(), "ravi", id)
		if err != nil || r.ERPName == "" || r.Status != StatusPosted {
			t.Fatalf("retry: %+v %v", r, err)
		}
		if fx.poster.count() != 2 {
			t.Errorf("poster called %d times, want 2", fx.poster.count())
		}
	})
	t.Run("other checker refused", func(t *testing.T) {
		fx := newFixture(t)
		id := fx.proposal(t)
		fx.poster.err = &Refusal{Code: "erp_unavailable"}
		if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrPostFailed) {
			t.Fatalf("first Approve: %v", err)
		}
		fx.poster.err = nil
		setFindingStatus(fx, *fx.st.row(id).FindingID, "dismissed")
		if _, err := fx.svc.Approve(t.Context(), "meera", id); !errors.Is(err, ErrNotPending) {
			t.Errorf("other checker: %v, want ErrNotPending", err)
		}
		if fx.poster.count() != 1 {
			t.Errorf("poster called %d times, want 1", fx.poster.count())
		}
		if row := fx.st.row(id); row.Status != StatusApproved {
			t.Errorf("row %+v, want approved", row)
		}
	})
	t.Run("first approval refused", func(t *testing.T) {
		fx := newFixture(t)
		id := fx.proposal(t)
		setFindingStatus(fx, *fx.st.row(id).FindingID, "dismissed")
		if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrFindingNotOpen) {
			t.Errorf("Approve: %v, want ErrFindingNotOpen", err)
		}
		if fx.poster.count() != 0 {
			t.Errorf("poster called %d times, want 0", fx.poster.count())
		}
		if row := fx.st.row(id); row.Status != StatusProposed || row.Checker != nil {
			t.Errorf("row %+v, want proposed", row)
		}
	})
}

// A posted proposal stays idempotent after its finding is dismissed; a
// rejected one stays not pending.
func TestApproveFindingNotOpenTerminal(t *testing.T) {
	dismiss := func(fx *fixture, id uuid.UUID) {
		fid := *fx.st.row(id).FindingID
		f := fx.findings.findings[fid]
		f.Status = "dismissed"
		fx.findings.findings[fid] = f
	}
	t.Run("posted", func(t *testing.T) {
		fx := newFixture(t)
		id := fx.proposal(t)
		r1, err := fx.svc.Approve(t.Context(), "ravi", id)
		if err != nil {
			t.Fatal(err)
		}
		dismiss(fx, id)
		r2, err := fx.svc.Approve(t.Context(), "meera", id)
		if err != nil || r2.ERPName != r1.ERPName || !r2.AlreadyPosted {
			t.Errorf("Approve posted: %+v %v, want %s already posted", r2, err, r1.ERPName)
		}
		if fx.poster.count() != 1 {
			t.Errorf("poster called %d times, want 1", fx.poster.count())
		}
	})
	t.Run("rejected", func(t *testing.T) {
		fx := newFixture(t)
		id := fx.proposal(t)
		if err := fx.svc.Reject(t.Context(), "ravi", id, "no"); err != nil {
			t.Fatal(err)
		}
		dismiss(fx, id)
		if _, err := fx.svc.Approve(t.Context(), "ravi", id); !errors.Is(err, ErrNotPending) {
			t.Errorf("Approve rejected: %v, want ErrNotPending", err)
		}
	})
	t.Run("no finding", func(t *testing.T) {
		fx := newFixture(t)
		id := uuid.New()
		fx.st.put(store.JournalProposal{ID: id, CompanyID: "sharma", Payload: balanced(), Status: StatusProposed, Maker: "asha"})
		if r, err := fx.svc.Approve(t.Context(), "ravi", id); err != nil || r.Status != StatusPosted {
			t.Errorf("Approve without a finding: %+v %v", r, err)
		}
	})
}

// Approve's post and every audit row outlive the request: a client that
// goes away before or during the call still gets a post and an audit row.
func TestApproveDetachedFromCancel(t *testing.T) {
	for _, when := range []string{"before", "during post"} {
		t.Run(when, func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "req-1"))
			defer cancel()
			var seen ctxSeen
			fx.poster.onPost = func(pctx context.Context) {
				if when == "during post" {
					cancel()
				}
				seen = seeCtx(pctx)
			}
			if when == "before" {
				// The lock step runs on the request ctx; the fake store
				// ignores it, so the post still runs.
				cancel()
			}
			start := time.Now()
			r, err := fx.svc.Approve(ctx, "ravi", id)
			if err != nil || r.Status != StatusPosted {
				t.Fatalf("Approve: %+v %v", r, err)
			}
			if seen.err != nil || !seen.has || seen.value != "req-1" {
				t.Errorf("post ctx %+v", seen)
			}
			if d := seen.deadline.Sub(start); d < PostTimeout || d > postCallTimeout+time.Second {
				t.Errorf("post deadline in %s, want just above %s", d, PostTimeout)
			}
			fx.st.mu.Lock()
			actx := fx.st.auditCtx[len(fx.st.auditCtx)-1]
			fx.st.mu.Unlock()
			if actx.err != nil || !actx.has || actx.value != "req-1" {
				t.Errorf("audit ctx %+v", actx)
			}
			if _, args := fx.st.lastAudit(t); args["result"] != "posted" {
				t.Errorf("audit result %v", args["result"])
			}
		})
	}
}

// Propose and Reject audit rows are written on a cancelled request too.
func TestAuditDetachedFromCancel(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "req-2"))
	cancel()
	if err := fx.svc.Reject(ctx, "ravi", id, "no"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := fx.svc.Propose(ctx, "asha", fx.finding(nil)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	fx.st.mu.Lock()
	ctxs := append([]ctxSeen(nil), fx.st.auditCtx[1:]...)
	fx.st.mu.Unlock()
	if len(ctxs) != 2 {
		t.Fatalf("%d audit rows, want 2", len(ctxs))
	}
	for i, actx := range ctxs {
		if actx.err != nil || !actx.has || time.Until(actx.deadline) > auditTimeout || actx.value != "req-2" {
			t.Errorf("audit %d ctx %+v", i, actx)
		}
	}
}

func TestApproveUnknown(t *testing.T) {
	fx := newFixture(t)
	if _, err := fx.svc.Approve(t.Context(), "ravi", uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Approve: %v, want ErrNotFound", err)
	}
	if err := fx.svc.Reject(t.Context(), "ravi", uuid.New(), "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Reject: %v, want ErrNotFound", err)
	}
}

// ---- Reject ----

func TestReject(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	if err := fx.svc.Reject(t.Context(), "Ravi", id, "  wrong expense account  "); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	row := fx.st.row(id)
	if row.Status != StatusRejected || row.Checker == nil || *row.Checker != "ravi" || row.Reason == nil ||
		*row.Reason != "wrong expense account" || row.DecidedAt == nil || row.ERPName != nil {
		t.Errorf("row %+v", row)
	}
	e, args := fx.st.lastAudit(t)
	if e.Actor != "ravi" || *e.Tool != ToolReject || args["reason"] != "wrong expense account" || args["result"] != "rejected" {
		t.Errorf("audit %+v %v", e, args)
	}
	if fx.poster.count() != 0 {
		t.Error("reject called the poster")
	}
	if err := fx.svc.Reject(t.Context(), "ravi", id, "again"); !errors.Is(err, ErrNotPending) {
		t.Errorf("reject twice: %v, want ErrNotPending", err)
	}
}

func TestRejectRefusals(t *testing.T) {
	cases := []struct {
		name, actor, reason string
		kind                error
	}{
		{"no reason", "ravi", "", ErrInvalidReason},
		{"blank reason", "ravi", "  \t ", ErrInvalidReason},
		{"control reason", "ravi", "bad\x00reason", ErrInvalidReason},
		{"long reason", "ravi", strings.Repeat("x", 501), ErrInvalidReason},
		{"self", " asha", "mine", ErrSelfApproval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			id := fx.proposal(t)
			if err := fx.svc.Reject(t.Context(), tc.actor, id, tc.reason); !errors.Is(err, tc.kind) {
				t.Fatalf("Reject: %v, want %v", err, tc.kind)
			}
			if row := fx.st.row(id); row.Status != StatusProposed || row.Checker != nil || fx.st.saves != 0 {
				t.Errorf("row changed %+v", row)
			}
			if fx.poster.count() != 0 {
				t.Error("poster called")
			}
			_, args := fx.st.lastAudit(t)
			if r, _ := args["result"].(string); !strings.HasPrefix(r, "refused:") {
				t.Errorf("audit result %v", args["result"])
			}
			if tc.kind == ErrInvalidReason {
				if _, ok := args["reason"]; ok {
					t.Error("an invalid reason was audited")
				}
			}
		})
	}
	t.Run("500 runes ok", func(t *testing.T) {
		fx := newFixture(t)
		if err := fx.svc.Reject(t.Context(), "ravi", fx.proposal(t), strings.Repeat("é", 500)); err != nil {
			t.Errorf("Reject: %v", err)
		}
	})
}

// ---- Pending ----

func TestPending(t *testing.T) {
	fx := newFixture(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mk := func(status string, offset time.Duration) uuid.UUID {
		p := store.JournalProposal{ID: uuid.New(), CompanyID: "sharma", Payload: balanced(), Status: status, Maker: "asha", CreatedAt: base.Add(offset)}
		fx.st.put(p)
		return p.ID
	}
	approved := mk(StatusApproved, 1*time.Minute)
	proposed2 := mk(StatusProposed, 2*time.Minute)
	proposed0 := mk(StatusProposed, 0)
	mk(StatusPosted, -time.Minute)
	mk(StatusRejected, -2*time.Minute)
	got, err := fx.svc.Pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{proposed0, approved, proposed2}
	if len(got) != len(want) {
		t.Fatalf("pending %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("pending[%d] = %s, want %s", i, got[i].ID, want[i])
		}
	}
}

// The logs of a full propose, approve, failed approve and reject never
// carry payload lines.
func TestNoPayloadInLogsOrAudit(t *testing.T) {
	fx := newFixture(t)
	id := fx.proposal(t)
	fx.poster.err = errors.New("boom")
	_, _ = fx.svc.Approve(t.Context(), "ravi", id)
	id2 := fx.proposal(t)
	_ = fx.svc.Reject(t.Context(), "ravi", id2, "no")
	for _, e := range fx.st.audits {
		if strings.Contains(string(e.Args), "HDFC") || strings.Contains(string(e.Args), "29500") {
			t.Errorf("audit args carry payload: %s", e.Args)
		}
		if e.Server == nil || *e.Server != AuditServer {
			t.Errorf("audit server %v", e.Server)
		}
	}
	if strings.Contains(fx.logs.String(), "HDFC") {
		t.Errorf("logs carry payload: %s", fx.logs.String())
	}
}
