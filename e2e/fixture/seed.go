package fixture

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Synthetic ids: stable so screenshots and tests can name them.
func sid(prefix byte, n byte) uuid.UUID { return uuid.UUID{0: prefix, 1: 0xe2, 15: n} }

// Exported ids the tests use.
var (
	RunDone    = sid(0xa1, 1) // mehta 2026-09, three planted bank charges
	RunFailed  = sid(0xa1, 2)
	RunQueued  = sid(0xa1, 3)
	RunOlder   = sid(0xa1, 4) // mehta 2026-08, proposals in every state
	FindClean  = sid(0xb1, 1) // verified, with citations, no entry
	FindReview = sid(0xb1, 2) // needs review
	FindEntry  = sid(0xb1, 3) // verified, with an entry the maker can propose
)

func paise(n int64) *money.Paise { p := money.Paise(n); return &p }
func str(s string) *string       { return &s }
func at(base time.Time, minutes int) *time.Time {
	t := base.Add(time.Duration(minutes) * time.Minute)
	return &t
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // a literal in this file
	}
	return b
}

func payload(date string, amount int64, remark string) store.JournalPayload {
	return store.JournalPayload{PostingDate: date, Remark: remark, Lines: []store.JournalLine{
		{Account: "Bank Charges - MT", DebitPaise: money.Paise(amount)},
		{Account: "HDFC Current Account - MT", CreditPaise: money.Paise(amount)},
	}}
}

// seed fills s with synthetic data. base is the fixture clock's origin.
func (s *Store) seed(base time.Time) {
	s.companies = []store.Company{
		{ID: "mehta", ERPCompany: "Mehta Textiles Pvt Ltd", GSTIN: "00AAAAA0000A0Z0"},
		{ID: "sharma", ERPCompany: "Sharma Foods Pvt Ltd", GSTIN: "00BBBBB0000B0Z0"},
	}
	day := base.Add(-24 * time.Hour)

	s.runs[RunDone] = store.CloseRun{ID: RunDone, CompanyID: "mehta", Month: "2026-09", Status: store.RunDone,
		StartedAt: at(day, 0), FinishedAt: at(day, 4), InputTokens: 6000, OutputTokens: 1200, CacheReadTokens: 1200, CostUSD: 0.042}
	s.runs[RunFailed] = store.CloseRun{ID: RunFailed, CompanyID: "sharma", Month: "2026-09", Status: store.RunFailed,
		StartedAt: at(day, -90), FinishedAt: at(day, -88), Error: str("ERPNext did not answer while reading bank transactions")}
	s.runs[RunQueued] = store.CloseRun{ID: RunQueued, CompanyID: "sharma", Month: "2026-08", Status: store.RunQueued}
	s.runs[RunOlder] = store.CloseRun{ID: RunOlder, CompanyID: "mehta", Month: "2026-08", Status: store.RunDone,
		StartedAt: at(day, -3000), FinishedAt: at(day, -2996)}

	// Run 1: the three planted bank charges.
	bankSHA := "e2e-snapshot-bank"
	s.arts[bankSHA] = store.Artifact{SHA256: bankSHA, Kind: store.ArtifactToolResult, Content: raw(map[string]any{
		"server": "books", "tool": "get_bank_transactions", "args": map[string]any{"month": "2026-09"},
		"result": []map[string]any{
			{"name": "BT-0001", "date": "2026-09-12", "description": "Account maintenance charge", "withdrawal": 118000},
			{"name": "BT-0002", "date": "2026-09-19", "description": "Cheque return charge", "withdrawal": 59000},
			{"name": "BT-0003", "date": "2026-09-27", "description": "SMS alert charge", "withdrawal": 23600},
		},
	}), CreatedAt: base}
	ev := func(id string) []store.EvidenceRef {
		return []store.EvidenceRef{{Server: "books", Tool: "get_bank_transactions", Args: raw(map[string]any{"month": "2026-09"}),
			IDs: []string{id}, Artifact: bankSHA}}
	}
	s.putFinding(store.Finding{ID: FindClean, RunID: RunDone, Type: "bank_charge", Severity: "high",
		Title: "Unbooked bank charge on 2026-09-12", AmountPaise: paise(118000), Keys: map[string]string{"bank_txn": "BT-0001"},
		Evidence: ev("BT-0001"), Explanation: str("The bank debited an account maintenance charge on 12 September that has no matching entry in the books."),
		Action: str("Book the charge to Bank Charges."), Citations: []store.Citation{{DocID: "POL-001", Section: "4.2"}},
		Verified: true, Status: "open"})
	s.putFinding(store.Finding{ID: FindReview, RunID: RunDone, Type: "bank_charge", Severity: "medium",
		Title: "Unbooked bank charge on 2026-09-19", AmountPaise: paise(59000), Keys: map[string]string{"bank_txn": "BT-0002"},
		Evidence: ev("BT-0002"), Explanation: str("A cheque return charge appears on the statement and not in the books."),
		Action: str("Confirm the charge with the bank, then book it."), Verified: false, Status: "needs_review"})
	s.putFinding(store.Finding{ID: FindEntry, RunID: RunDone, Type: "bank_charge", Severity: "low",
		Title: "Unbooked bank charge on 2026-09-27", AmountPaise: paise(23600), Keys: map[string]string{"bank_txn": "BT-0003"},
		Evidence: ev("BT-0003"), Explanation: str("An SMS alert charge was debited on 27 September and is not booked."),
		Action: str("Book the charge to Bank Charges."), Citations: []store.Citation{{DocID: "POL-001", Section: "4.2"}},
		Proposal: &store.JournalProposal{CompanyID: "mehta", Payload: payload("2026-09-30", 23600, "SMS alert charge, September 2026"), Status: "proposed"},
		Verified: true, Status: "open"})
	verdictSHA := "e2e-verdict-review"
	s.arts[verdictSHA] = store.Artifact{SHA256: verdictSHA, Kind: store.ArtifactVerdict, Content: raw(map[string]any{
		"pass": false, "violations": []map[string]any{{"code": "number_not_in_evidence", "field": "explanation", "value_paise": 59000,
			"detail": "the amount is in the statement but the cheque number is not"}},
	}), CreatedAt: base}
	s.steps[RunDone] = []store.Step{{ID: sid(0xc1, 1), RunID: RunDone, Kind: store.StepKindVerify, Subject: FindReview.String(),
		Status: store.StepDone, Attempt: 1, OutputRefs: []string{verdictSHA}}}

	// Run 2 (older): proposals in every state.
	pendingA := s.putFinding(store.Finding{ID: sid(0xb1, 4), RunID: RunOlder, Type: "bank_charge", Severity: "high",
		Title: "Unbooked bank charge on 2026-08-08", AmountPaise: paise(47200), Keys: map[string]string{}, Verified: true, Status: "open",
		Evidence: nil, Explanation: str("Debit for locker rent has no entry."), Action: str("Book it.")})
	pendingB := s.putFinding(store.Finding{ID: sid(0xb1, 5), RunID: RunOlder, Type: "bank_charge", Severity: "medium",
		Title: "Unbooked bank charge on 2026-08-14", AmountPaise: paise(35400), Keys: map[string]string{}, Verified: true, Status: "open",
		Explanation: str("Debit for NEFT fees has no entry."), Action: str("Book it.")})
	pendingC := s.putFinding(store.Finding{ID: sid(0xb1, 8), RunID: RunOlder, Type: "bank_charge", Severity: "low",
		Title: "Unbooked bank charge on 2026-08-20", AmountPaise: paise(11800), Keys: map[string]string{}, Verified: true, Status: "open",
		Explanation: str("Debit for statement fees has no entry."), Action: str("Book it.")})
	posted := s.putFinding(store.Finding{ID: sid(0xb1, 6), RunID: RunOlder, Type: "bank_charge", Severity: "low",
		Title: "Unbooked bank charge on 2026-08-21", AmountPaise: paise(29500), Keys: map[string]string{}, Verified: true, Status: "accepted",
		Explanation: str("Debit for cheque book fees has no entry."), Action: str("Book it.")})
	rejected := s.putFinding(store.Finding{ID: sid(0xb1, 7), RunID: RunOlder, Type: "bank_charge", Severity: "low",
		Title: "Unbooked bank charge on 2026-08-25", AmountPaise: paise(17700), Keys: map[string]string{}, Verified: true, Status: "open",
		Explanation: str("Debit for demand draft fees has no entry."), Action: str("Book it.")})

	prop := func(n byte, f store.Finding, status, maker string, minutes int) store.JournalProposal {
		id := f.ID
		return store.JournalProposal{ID: sid(0xd1, n), FindingID: &id, CompanyID: "mehta", Status: status, Maker: maker,
			Payload:   payload("2026-08-31", int64(*f.AmountPaise), f.Title),
			CreatedAt: *at(day, minutes)}
	}
	s.proposals[sid(0xd1, 1)] = prop(1, pendingA, "proposed", "asha", -2900)
	s.proposals[sid(0xd1, 2)] = prop(2, pendingB, "proposed", "asha", -2890)
	s.proposals[sid(0xd1, 5)] = prop(5, pendingC, "proposed", "asha", -2880)
	p3 := prop(3, posted, "posted", "asha", -2850)
	p3.Checker, p3.ERPName, p3.DecidedAt, p3.PostedAt = str("ravi"), str("ACC-JV-2026-00001"), at(day, -2840), at(day, -2839)
	s.proposals[p3.ID] = p3
	p4 := prop(4, rejected, "rejected", "asha", -2830)
	p4.Checker, p4.Reason, p4.DecidedAt = str("ravi"), str("Duplicate of the August bank charge already booked"), at(day, -2820)
	s.proposals[p4.ID] = p4

	// A few audit rows, oldest first.
	tool := func(server, name string, n int) store.AuditLogEntry {
		return store.AuditLogEntry{Actor: "agent", Server: str(server), Tool: str(name), Args: raw(map[string]any{"month": "2026-09"}),
			ResultCount: &n, LatencyMS: &n, At: *at(day, 1)}
	}
	s.addAudit(tool("books", "get_bank_transactions", 3))
	s.addAudit(tool("books", "get_gl_entries", 12))
	s.addAudit(tool("evidence", "search_documents", 2))
	s.addAudit(store.AuditLogEntry{Actor: "asha", Server: str("web"), Tool: str("finding.accept"),
		Args: raw(map[string]any{"finding": posted.ID.String()}), At: *at(day, -2860)})
	s.addAudit(store.AuditLogEntry{Actor: "ravi", Server: str("approvals"), Tool: str("approve_proposal"),
		Args: raw(map[string]any{"proposal": p3.ID.String()}), At: *at(day, -2840)})
}

func (s *Store) putFinding(f store.Finding) store.Finding {
	s.findings[f.ID] = f
	s.order = append(s.order, f.ID)
	return f
}

// Execute is the web.RunExecutor: it steps a queued run to done in a few
// hundred milliseconds so the progress panel is visible, adding the three
// planted bank charges.
func (s *Store) Execute(ctx context.Context, id uuid.UUID) error {
	wait := func() error {
		select {
		case <-time.After(s.stepDelay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	r := s.runs[id]
	r.Status, r.StartedAt = store.RunRunning, ptr(s.tick())
	s.runs[id] = r
	s.mu.Unlock()
	if err := wait(); err != nil {
		return err
	}
	s.mu.Lock()
	for i, c := range []struct {
		sev   string
		cents int64
		date  string
	}{{"high", 118000, "12"}, {"medium", 59000, "19"}, {"low", 23600, "27"}} {
		s.putFinding(store.Finding{ID: uuid.New(), RunID: id, Type: "bank_charge", Severity: c.sev,
			Title:       "Unbooked bank charge on " + r.Month + "-" + c.date,
			AmountPaise: paise(c.cents), Keys: map[string]string{}, Verified: i != 1, Status: map[bool]string{true: "open", false: "needs_review"}[i != 1],
			Explanation: str("The bank debited a charge that has no matching entry in the books."), Action: str("Book the charge to Bank Charges.")})
	}
	s.mu.Unlock()
	if err := wait(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r = s.runs[id]
	r.Status, r.FinishedAt = store.RunDone, ptr(s.tick())
	s.runs[id] = r
	return nil
}

func ptr[T any](v T) *T { return &v }
