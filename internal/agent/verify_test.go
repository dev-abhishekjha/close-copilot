package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ---- fakes ----

// fakeCitations holds the doc_chunks passages per company ("" = shared).
type fakeCitations struct {
	mu       sync.Mutex
	passages map[string]map[store.Citation]bool
	calls    int
	err      error
}

func (c *fakeCitations) CitationExists(_ context.Context, company, docID, section string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return false, c.err
	}
	key := store.Citation{DocID: docID, Section: section}
	return c.passages[company][key] || c.passages[""][key], nil
}

// forbiddenModel fails the test if the verifier calls it.
type forbiddenModel struct{ t *testing.T }

func (m forbiddenModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	m.t.Error("the verifier called a model")
	return llm.Response{}, errors.New("forbidden")
}

// ---- fixture (synthetic) ----

const (
	verifyCompany = "sharma"
	verifyMonth   = "2026-09"
	otherCompany  = "otherco"
)

var (
	verifyRunID     = uuid.MustParse("00000000-0000-4000-8000-0000000000b1")
	verifyFindingID = uuid.MustParse("00000000-0000-4000-8000-0000000000b2")
)

type verifyFixture struct {
	st    *memStore
	cites *fakeCitations
	v     *CodeVerifier
	req   VerifyRequest
	// base is a grounded explanation.
	base ExplanationArtifact
	// accounts snapshots: this company's, another company's, another
	// month's.
	accounts, otherAccounts, otherMonthAccounts string
	// retrieval artifacts.
	documents string
}

func bankSnapshot(company, from, to string, lines ...map[string]any) toolSnapshot {
	result := make([]any, len(lines))
	for i, l := range lines {
		result[i] = l
	}
	return toolSnapshot{
		Server: ServerEvidence, Tool: toolListBankLines,
		Args:   map[string]any{"company": company, "from_date": from, "to_date": to},
		Result: result,
	}
}

func accountSnapshot(company, from, to string, accounts ...string) toolSnapshot {
	rows := make([]accountRow, len(accounts))
	for i, a := range accounts {
		rows[i] = accountRow{Account: a}
	}
	return toolSnapshot{Server: ServerBooks, Tool: toolGetTrialBalance,
		Args: map[string]any{"company": company, "from_date": from, "to_date": to}, Result: accountsSnapshot{Rows: rows}}
}

func (fx *verifyFixture) put(t *testing.T, kind string, v any) string {
	t.Helper()
	sha, err := fx.st.PutArtifact(t.Context(), kind, uuid.Nil, uuid.Nil, v)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	fx := &verifyFixture{st: newMemStore(), cites: &fakeCitations{passages: map[string]map[store.Citation]bool{
		verifyCompany: {{DocID: "POL-BANK", Section: "2.1"}: true},
		"":            {{DocID: "POL-SHARED", Section: "1"}: true},
		otherCompany:  {{DocID: "POL-OTHER", Section: "4"}: true},
	}}}
	fx.v = &CodeVerifier{Store: fx.st, Citations: fx.cites}

	// The finding names C1 and C2; C9 is in the snapshot but not named,
	// so its amount was never shown to the model.
	evidence := fx.put(t, store.ArtifactToolResult, bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
		map[string]any{"txn_id": "HDFC-20260915-C1", "txn_date": "2026-09-15", "narration": "DEBIT CARD ANNUAL FEE", "amount_paise": -11800000, "balance_paise": 250000000},
		map[string]any{"txn_id": "HDFC-20260920-C2", "txn_date": "2026-09-20", "narration": "SMS CHGS", "amount_paise": -118050},
		map[string]any{"txn_id": "HDFC-20260925-C9", "txn_date": "2026-09-25", "narration": "OTHER", "amount_paise": -777700},
	))
	fx.accounts = fx.put(t, store.ArtifactToolResult, accountSnapshot(verifyCompany, "2026-09-01", "2026-09-30", "Bank Charges - STPL", "HDFC Current 0001 - STPL"))
	fx.otherAccounts = fx.put(t, store.ArtifactToolResult, accountSnapshot(otherCompany, "2026-09-01", "2026-09-30", "Other Expense - OC"))
	fx.otherMonthAccounts = fx.put(t, store.ArtifactToolResult, accountSnapshot(verifyCompany, "2026-08-01", "2026-08-31", "Bank Charges - STPL", "HDFC Current 0001 - STPL"))
	fx.documents = fx.put(t, store.ArtifactRetrieval, map[string]any{"query": "bank charges", "passages": []map[string]any{
		{"doc_id": "POL-BANK", "section": "2.1", "text": "Bank charges are booked monthly."},
		{"doc_id": "POL-SHARED", "section": "1", "text": "Shared policy."},
		{"doc_id": "POL-GONE", "section": "3", "text": "A passage since deleted."},
		{"doc_id": "POL-OTHER", "section": "4", "text": "Another company's passage."},
	}})

	amt := money.Paise(59000)
	finding := store.Finding{
		ID: verifyFindingID, RunID: verifyRunID, Type: checks.TypeUnrecordedBankCharge, Severity: "medium",
		Title: "Unrecorded bank charge", AmountPaise: &amt,
		Evidence: []store.EvidenceRef{{Server: ServerEvidence, Tool: toolListBankLines, Args: json.RawMessage(`{"company":"sharma"}`),
			IDs: []string{"HDFC-20260915-C1", "HDFC-20260920-C2"}, Artifact: evidence}},
	}
	fx.req = VerifyRequest{
		RunID: verifyRunID, StepID: uuid.MustParse("00000000-0000-4000-8000-0000000000b3"), Attempt: 1,
		Company: verifyCompany, Month: verifyMonth, Finding: finding, EvidenceRefs: []string{evidence},
		ExplainAttempt: 1, Model: forbiddenModel{t},
	}
	fx.base = ExplanationArtifact{
		RunID: verifyRunID.String(), FindingID: verifyFindingID.String(), Model: explainFast,
		Explanation:       "The bank debited ₹1,18,000.00 on 2026-09-15 for a debit card fee and Rs.1,180.50 on 20 Sep; supplier GSTIN 27ABCDE1234F1Z5. Neither is booked.",
		SuggestedAction:   checks.ActionBookEntry,
		ActionNote:        "Book ₹1,180.50 to Bank Charges.",
		CitedAmountsPaise: []money.Paise{11800000, 118050},
		Citations:         []store.Citation{},
		Proposal: &store.JournalPayload{PostingDate: "2026-09-30", Remark: "Bank charges per statement", Lines: []store.JournalLine{
			{Account: "Bank Charges - STPL", DebitPaise: 118050},
			{Account: "HDFC Current 0001 - STPL", CreditPaise: 118050},
		}},
		AccountsRef:  fx.accounts,
		DocumentRefs: []string{},
	}
	return fx
}

// explanation stores a copy of base changed by mutate and points the
// request at it.
func (fx *verifyFixture) explanation(t *testing.T, mutate func(*ExplanationArtifact)) VerifyRequest {
	t.Helper()
	art := fx.base
	art.CitedAmountsPaise = slices.Clone(art.CitedAmountsPaise)
	art.Citations = slices.Clone(art.Citations)
	if art.Proposal != nil {
		p := *art.Proposal
		p.Lines = slices.Clone(p.Lines)
		art.Proposal = &p
	}
	if mutate != nil {
		mutate(&art)
	}
	req := fx.req
	req.ExplanationRefs = []string{fx.put(t, store.ArtifactExplanation, art)}
	return req
}

// verify runs Verify and returns the stored verdict.
func (fx *verifyFixture) verify(t *testing.T, req VerifyRequest) (StepResult, VerdictArtifact) {
	t.Helper()
	res, err := fx.v.Verify(t.Context(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != store.StepDone || res.StepID != req.StepID || len(res.OutputRefs) != 1 {
		t.Fatalf("result %+v", res)
	}
	a, err := fx.st.GetArtifact(t.Context(), res.OutputRefs[0])
	if err != nil || a.Kind != store.ArtifactVerdict {
		t.Fatalf("verdict artifact: %v %s", err, a.Kind)
	}
	var v VerdictArtifact
	if err := json.Unmarshal(a.Content, &v); err != nil {
		t.Fatal(err)
	}
	return res, v
}

func codesOf(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		if !slices.Contains(out, v.Code) {
			out = append(out, v.Code)
		}
	}
	slices.Sort(out)
	return out
}

func line(account string, debit, credit money.Paise) store.JournalLine {
	return store.JournalLine{Account: account, DebitPaise: debit, CreditPaise: credit}
}

// ---- tests ----

func TestVerifyAcceptsGroundedOutputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExplanationArtifact)
	}{
		{"the base explanation", nil},
		{"Indian grouping", func(a *ExplanationArtifact) { a.Explanation = "A fee of ₹1,18,000 was charged." }},
		{"Rs. and decimals", func(a *ExplanationArtifact) { a.Explanation = "Rs.1,180.50 was debited." }},
		{"INR without a comma", func(a *ExplanationArtifact) { a.Explanation = "INR 1180 was debited." }},
		{"western grouping", func(a *ExplanationArtifact) { a.Explanation = "A fee of 118,000.00 was charged." }},
		{"+100 paise", func(a *ExplanationArtifact) {
			a.Explanation = "About ₹1,181.50."
			a.CitedAmountsPaise = []money.Paise{118150}
		}},
		{"-100 paise", func(a *ExplanationArtifact) {
			a.Explanation = "About ₹1,179.50."
			a.CitedAmountsPaise = []money.Paise{117950}
		}},
		{"the sum of two amounts", func(a *ExplanationArtifact) {
			a.Explanation = "Together ₹1,19,180.50."
			a.CitedAmountsPaise = []money.Paise{11918050}
		}},
		{"the difference of two amounts", func(a *ExplanationArtifact) {
			a.Explanation = "The difference is ₹1,16,819.50."
			a.CitedAmountsPaise = []money.Paise{11681950}
		}},
		{"the finding's amount", func(a *ExplanationArtifact) {
			a.Explanation = "The finding is for ₹590.00."
			a.CitedAmountsPaise = []money.Paise{59000}
		}},
		{"a negative evidence amount by absolute value", func(a *ExplanationArtifact) { a.Explanation = "The bank debited -₹1,180.50." }},
		{"the balance", func(a *ExplanationArtifact) { a.Explanation = "The closing balance was ₹25,00,000." }},
		{"dates and a GSTIN are not amounts", func(a *ExplanationArtifact) {
			a.Explanation = "Posted 2026-09-15 (15/09/2026) by 27ABCDE1234F1Z5 for 2026-09."
			a.CitedAmountsPaise = []money.Paise{}
		}},
		{"a proposal posting in the next month", func(a *ExplanationArtifact) { a.Proposal.PostingDate = "2026-10-31" }},
		{"a proposal posting on the month's first day", func(a *ExplanationArtifact) { a.Proposal.PostingDate = "2026-09-01" }},
		{"no proposal and no account snapshot", func(a *ExplanationArtifact) { a.Proposal = nil; a.AccountsRef = "" }},
		{"citations supplied and known", func(a *ExplanationArtifact) {
			a.DocumentRefs = []string{""} // replaced below
			a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}, {DocID: "POL-SHARED", Section: "1"}}
		}},
		{"a model-flagged review passes", func(a *ExplanationArtifact) { a.NeedsReview = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newVerifyFixture(t)
			req := fx.explanation(t, func(a *ExplanationArtifact) {
				if tt.mutate != nil {
					tt.mutate(a)
				}
				if len(a.DocumentRefs) == 1 && a.DocumentRefs[0] == "" {
					a.DocumentRefs = []string{fx.documents}
				}
			})
			res, v := fx.verify(t, req)
			if !v.Pass || len(v.Violations) != 0 || len(res.Violations) != 0 || res.Reason != "" {
				t.Fatalf("verdict %+v, result %+v; want a pass", v, res)
			}
			if v.RunID != verifyRunID.String() || v.FindingID != verifyFindingID.String() || v.ExplanationRef != req.ExplanationRefs[0] ||
				v.ExplainAttempt != 1 || v.VerifierVersion != VerifierVersion {
				t.Errorf("verdict header %+v", v)
			}
			if res.NeedsReview != (tt.name == "a model-flagged review passes") {
				t.Errorf("needs review %v", res.NeedsReview)
			}
		})
	}
}

func TestVerifyRejectsCraftedOutputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*verifyFixture, *ExplanationArtifact, *VerifyRequest)
		want   []string // violation codes
		field  string   // one violation's field
	}{
		{"an invented number in the text", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Explanation += " The total exposure is ₹9,87,654.32."
		}, []string{ViolationAmountNotInEvidence}, "explanation"},
		{"an invented range in the text", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Explanation += " Similar charges ran ₹9,87,654-9,87,700."
		}, []string{ViolationAmountNotInEvidence}, "explanation"},
		{"an invented number in the action note", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.ActionNote = "Book ₹4,321.00."
		}, []string{ViolationAmountNotInEvidence}, "action_note"},
		{"an invented number in the remark", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Remark = "Charges of Rs 4,321"
		}, []string{ViolationAmountNotInEvidence}, "proposal.remark"},
		{"101 paise off", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Explanation = "About ₹1,181.51."
		}, []string{ViolationAmountNotInEvidence}, "explanation"},
		{"an amount only in an unnamed record", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Explanation = "Another debit of ₹7,777.00."
		}, []string{ViolationAmountNotInEvidence}, "explanation"},
		{"an invented cited amount", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.CitedAmountsPaise = append(a.CitedAmountsPaise, 432100)
		}, []string{ViolationCitedAmountNotInEvidence}, "cited_amounts_paise[2]"},
		{"an invented proposal amount that balances", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Lines = []store.JournalLine{line("Bank Charges - STPL", 500000, 0), line("HDFC Current 0001 - STPL", 0, 500000)}
		}, []string{ViolationProposalAmountNotInEvidence}, "proposal.lines[0].debit"},
		{"an unbalanced entry", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Lines = []store.JournalLine{line("Bank Charges - STPL", 118050, 0), line("HDFC Current 0001 - STPL", 0, 11800000)}
		}, []string{ViolationProposalUnbalanced}, "proposal.lines"},
		{"a two-sided line", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Lines = []store.JournalLine{line("Bank Charges - STPL", 118050, 118050), line("HDFC Current 0001 - STPL", 0, 0)}
		}, []string{ViolationProposalUnbalanced}, "proposal.lines"},
		{"a one-line entry", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Lines = a.Proposal.Lines[:1]
		}, []string{ViolationProposalUnbalanced}, "proposal.lines"},
		{"an unknown account", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.Lines[0].Account = "Suspense - STPL"
		}, []string{ViolationProposalUnknownAccount}, "proposal.lines[0].account"},
		{"an account only in another company's snapshot", func(fx *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.AccountsRef = fx.otherAccounts
			a.Proposal.Lines[0].Account = "Other Expense - OC"
		}, []string{ViolationProposalUnknownAccount, ViolationScopeMismatch}, "accounts.args.company"},
		{"an account snapshot of another month", func(fx *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.AccountsRef = fx.otherMonthAccounts
		}, []string{ViolationProposalUnknownAccount, ViolationScopeMismatch}, "accounts.args.from_date"},
		{"a proposal without an account snapshot", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.AccountsRef = ""
		}, []string{ViolationAccountsUnverifiable}, "accounts_ref"},
		{"a posting date before the month", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.PostingDate = "2026-08-31"
		}, []string{ViolationProposalDateOutOfWindow}, "proposal.posting_date"},
		{"a posting date two months later", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.PostingDate = "2026-11-01"
		}, []string{ViolationProposalDateOutOfWindow}, "proposal.posting_date"},
		{"a posting date that is not a date", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Proposal.PostingDate = "30/09/2026"
		}, []string{ViolationProposalDateOutOfWindow}, "proposal.posting_date"},
		{"an action outside the enum", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.SuggestedAction = "pay_now"
		}, []string{ViolationActionNotAllowed}, "suggested_action"},
		{"a fake citation with no documents", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
		}, []string{ViolationCitationNotSupplied}, "citations[0]"},
		{"a citation not among the documents", func(fx *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.DocumentRefs = []string{fx.documents}
			a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "9.9"}}
		}, []string{ViolationCitationNotSupplied}, "citations[0]"},
		{"a citation supplied but missing from doc_chunks", func(fx *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.DocumentRefs = []string{fx.documents}
			a.Citations = []store.Citation{{DocID: "POL-GONE", Section: "3"}}
		}, []string{ViolationCitationUnknown}, "citations[0]"},
		{"a citation to another company's chunk", func(fx *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.DocumentRefs = []string{fx.documents}
			a.Citations = []store.Citation{{DocID: "POL-OTHER", Section: "4"}}
		}, []string{ViolationCitationUnknown}, "citations[0]"},
		{"an explanation of another run", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.RunID = uuid.NewString()
		}, []string{ViolationScopeMismatch}, "explanation.run_id"},
		{"an explanation of another finding", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.FindingID = uuid.NewString()
		}, []string{ViolationScopeMismatch}, "explanation.finding_id"},
		{"evidence of another company", func(fx *verifyFixture, _ *ExplanationArtifact, req *VerifyRequest) {
			fx.swapEvidence(t, req, bankSnapshot(otherCompany, "2026-09-01", "2026-09-30",
				map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "balance_paise": 250000000},
				map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050}))
		}, []string{ViolationScopeMismatch}, "evidence[0].args.company"},
		{"evidence of another month", func(fx *verifyFixture, _ *ExplanationArtifact, req *VerifyRequest) {
			fx.swapEvidence(t, req, bankSnapshot(verifyCompany, "2026-08-01", "2026-09-30",
				map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "balance_paise": 250000000},
				map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050}))
		}, []string{ViolationScopeMismatch}, "evidence[0].args.from_date"},
		{"unparseable precision", func(_ *verifyFixture, a *ExplanationArtifact, _ *VerifyRequest) {
			a.Explanation = "A fee of ₹1,180.505 was charged."
		}, []string{ViolationAmountUnparseable}, "explanation"},
		{"too many evidence amounts", func(fx *verifyFixture, _ *ExplanationArtifact, req *VerifyRequest) {
			many := make([]int, maxEvidenceAmounts)
			for i := range many {
				many[i] = 1000 + i
			}
			fx.swapEvidence(t, req, bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
				map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "balance_paise": 250000000, "amounts_paise": many},
				map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050}))
		}, []string{ViolationEvidenceTruncated}, "evidence"},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newVerifyFixture(t)
			tmp := fx.req
			req := fx.explanation(t, func(a *ExplanationArtifact) { tt.mutate(fx, a, &tmp) })
			req.Finding, req.EvidenceRefs = tmp.Finding, tmp.EvidenceRefs
			res, v := fx.verify(t, req)
			if v.Pass {
				t.Fatalf("verdict passed: %+v", v)
			}
			if got := codesOf(v.Violations); !slices.Equal(got, tt.want) {
				t.Errorf("codes %v, want %v (violations %+v)", got, tt.want, v.Violations)
			}
			if !slices.ContainsFunc(v.Violations, func(x Violation) bool { return x.Field == tt.field }) {
				t.Errorf("no violation on field %s: %+v", tt.field, v.Violations)
			}
			for _, c := range codesOf(v.Violations) {
				seen[c] = true
			}
			// The step result carries the feedback contract and a reason
			// of codes only.
			var fb VerifierFeedback
			if err := json.Unmarshal(res.Violations, &fb); err != nil || !slices.Equal(codesOf(fb.Violations), tt.want) {
				t.Errorf("feedback %s: %v", res.Violations, err)
			}
			if res.Reason != "verification failed: "+strings.Join(tt.want, ", ") || res.NeedsReview {
				t.Errorf("reason %q needs review %v", res.Reason, res.NeedsReview)
			}
			// No evidence or document text leaks into a violation.
			for _, x := range v.Violations {
				for _, leak := range []string{"DEBIT CARD", "SMS CHGS", "booked monthly", "Shared policy", "Suspense", "Other Expense"} {
					if strings.Contains(x.Detail, leak) || strings.Contains(x.Field, leak) {
						t.Errorf("violation leaks %q: %+v", leak, x)
					}
				}
				if violationDetail[x.Code] == "" || !strings.HasPrefix(x.Detail, violationDetail[x.Code]) {
					t.Errorf("violation detail is not the code's fixed text: %+v", x)
				}
			}
		})
	}
	t.Run("every code is exercised", func(t *testing.T) {
		for code := range violationDetail {
			if !seen[code] {
				t.Errorf("no crafted output produces %s", code)
			}
		}
	})
}

// swapEvidence stores a different snapshot and points the request and
// the finding's evidence at it.
func (fx *verifyFixture) swapEvidence(t *testing.T, req *VerifyRequest, snap toolSnapshot) {
	t.Helper()
	sha := fx.put(t, store.ArtifactToolResult, snap)
	f := req.Finding
	f.Evidence = slices.Clone(f.Evidence)
	f.Evidence[0].Artifact = sha
	req.Finding = f
	req.EvidenceRefs = []string{sha}
}

func TestVerifyIntegrityErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*verifyFixture, *VerifyRequest)
		want  error
	}{
		{"a missing explanation", func(_ *verifyFixture, req *VerifyRequest) {
			req.ExplanationRefs = []string{strings.Repeat("0", 64)}
		}, store.ErrNotFound},
		{"a missing evidence snapshot", func(_ *verifyFixture, req *VerifyRequest) {
			req.EvidenceRefs = []string{strings.Repeat("1", 64)}
		}, store.ErrNotFound},
		{"a tampered explanation", func(fx *verifyFixture, req *VerifyRequest) {
			fx.st.tamper(req.ExplanationRefs[0], []byte(`{"explanation":"tampered"}`))
		}, store.ErrArtifactMismatch},
		{"a tampered evidence snapshot", func(fx *verifyFixture, req *VerifyRequest) {
			fx.st.tamper(req.EvidenceRefs[0], []byte(`{"server":"evidence","tool":"list_bank_lines","args":{},"result":[]}`))
		}, store.ErrArtifactMismatch},
		{"a tampered account snapshot", func(fx *verifyFixture, _ *VerifyRequest) {
			fx.st.tamper(fx.accounts, []byte(`{"server":"books","tool":"get_trial_balance","args":{},"result":{"rows":[]}}`))
		}, store.ErrArtifactMismatch},
		{"a missing account snapshot", func(fx *verifyFixture, req *VerifyRequest) {
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) { a.AccountsRef = strings.Repeat("2", 64) })
		}, store.ErrNotFound},
		{"a missing document artifact", func(fx *verifyFixture, req *VerifyRequest) {
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) {
				a.DocumentRefs = []string{strings.Repeat("3", 64)}
				a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
			})
		}, store.ErrNotFound},
		{"two explanation refs", func(_ *verifyFixture, req *VerifyRequest) {
			req.ExplanationRefs = append(req.ExplanationRefs, req.ExplanationRefs[0])
		}, nil},
		{"an explanation ref that is not an explanation", func(_ *verifyFixture, req *VerifyRequest) {
			req.ExplanationRefs = req.EvidenceRefs
		}, nil},
		{"an evidence ref that is not a tool result", func(_ *verifyFixture, req *VerifyRequest) {
			req.EvidenceRefs = req.ExplanationRefs
		}, nil},
		{"an account ref that is not a tool result", func(fx *verifyFixture, req *VerifyRequest) {
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) { a.AccountsRef = fx.documents })
		}, nil},
		{"an account ref that is not a trial balance", func(fx *verifyFixture, req *VerifyRequest) {
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) { a.AccountsRef = req.EvidenceRefs[0] })
		}, nil},
		{"a document ref that is not a retrieval", func(fx *verifyFixture, req *VerifyRequest) {
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) {
				a.DocumentRefs = []string{fx.accounts}
				a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
			})
		}, nil},
		{"a bad month", func(_ *verifyFixture, req *VerifyRequest) { req.Month = "2026-13" }, nil},
		{"a citation lookup error", func(fx *verifyFixture, req *VerifyRequest) {
			fx.cites.err = errors.New("synthetic lookup failure")
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) {
				a.DocumentRefs = []string{fx.documents}
				a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
			})
		}, nil},
		{"no citation lookup", func(fx *verifyFixture, req *VerifyRequest) {
			fx.v.Citations = nil
			*req = fx.explanationWith(req, func(a *ExplanationArtifact) {
				a.DocumentRefs = []string{fx.documents}
				a.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
			})
		}, nil},
		{"no store", func(fx *verifyFixture, _ *VerifyRequest) { fx.v.Store = nil }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newVerifyFixture(t)
			req := fx.explanation(t, nil)
			tt.setup(fx, &req)
			before := fx.st.puts
			res, err := fx.v.Verify(t.Context(), req)
			if err == nil {
				t.Fatalf("Verify passed with %+v; want an error", res)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err %v, want %v", err, tt.want)
			}
			if fx.st.puts != before {
				t.Error("a verdict was stored for an integrity failure")
			}
		})
	}
}

// explanationWith stores base changed by mutate (keeping the request's
// other fields).
func (fx *verifyFixture) explanationWith(req *VerifyRequest, mutate func(*ExplanationArtifact)) VerifyRequest {
	art := fx.base
	art.Proposal = &store.JournalPayload{PostingDate: fx.base.Proposal.PostingDate, Remark: fx.base.Proposal.Remark, Lines: slices.Clone(fx.base.Proposal.Lines)}
	mutate(&art)
	sha, err := fx.st.PutArtifact(context.Background(), store.ArtifactExplanation, uuid.Nil, uuid.Nil, art)
	if err != nil {
		panic(err)
	}
	out := *req
	out.ExplanationRefs = []string{sha}
	return out
}

func TestVerifyDeterministic(t *testing.T) {
	fx := newVerifyFixture(t)
	req := fx.explanation(t, func(a *ExplanationArtifact) {
		a.Explanation += " Also ₹4,321.00 and ₹1,234.56 and ₹4,321.00."
		a.CitedAmountsPaise = []money.Paise{777700, 432100}
		a.Proposal.PostingDate = "2026-12-01"
	})
	first, v1 := fx.verify(t, req)
	second, v2 := fx.verify(t, req)
	if first.OutputRefs[0] != second.OutputRefs[0] || string(first.Violations) != string(second.Violations) {
		t.Error("the same inputs gave different verdicts")
	}
	// Violations are sorted and repeated amounts appear once.
	if !slices.IsSortedFunc(v1.Violations, func(a, b Violation) int {
		return strings.Compare(a.Code+"\x00"+a.Field, b.Code+"\x00"+b.Field)
	}) {
		t.Errorf("violations not sorted: %+v", v1.Violations)
	}
	n := 0
	for _, x := range v1.Violations {
		if x.ValuePaise != nil && *x.ValuePaise == 432100 && x.Code == ViolationAmountNotInEvidence {
			n++
		}
	}
	if n != 1 {
		t.Errorf("₹4,321.00 reported %d times, want once", n)
	}
	if v2.Checked.Amounts != 11 || v2.Checked.ProposalLines != 2 || v2.Checked.Citations != 0 {
		t.Errorf("checked %+v", v2.Checked)
	}
	// Evaluate gives the same verdict without storing it.
	puts := fx.st.puts
	v3, err := fx.v.Evaluate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	_, sha, err := store.CanonicalHash(v3)
	if err != nil || sha != first.OutputRefs[0] || fx.st.puts != puts {
		t.Errorf("Evaluate sha %s (stored %s), puts %d -> %d", sha, first.OutputRefs[0], puts, fx.st.puts)
	}
}

func TestVerifyFeedbackIsCapped(t *testing.T) {
	fx := newVerifyFixture(t)
	req := fx.explanation(t, func(a *ExplanationArtifact) {
		a.CitedAmountsPaise = nil
		for i := range 60 {
			a.CitedAmountsPaise = append(a.CitedAmountsPaise, money.Paise(900000+i))
		}
	})
	res, v := fx.verify(t, req)
	var fb VerifierFeedback
	if err := json.Unmarshal(res.Violations, &fb); err != nil {
		t.Fatal(err)
	}
	if len(v.Violations) != 60 || len(fb.Violations) != maxFeedbackViolations {
		t.Errorf("verdict %d violations, feedback %d", len(v.Violations), len(fb.Violations))
	}
}

// TestVerifyFeedbackFitsTheStore: many long violations are dropped from
// the end of the feedback until it fits what ReopenStep stores; the
// verdict keeps them all.
func TestVerifyFeedbackFitsTheStore(t *testing.T) {
	fx := newVerifyFixture(t)
	long := strings.Repeat("₹", maxCitationRunes)
	req := fx.explanation(t, func(a *ExplanationArtifact) {
		a.Citations = nil
		for i := range 60 {
			a.Citations = append(a.Citations, store.Citation{DocID: long + strconv.Itoa(i), Section: long})
		}
	})
	res, v := fx.verify(t, req)
	var fb VerifierFeedback
	if err := json.Unmarshal(res.Violations, &fb); err != nil {
		t.Fatal(err)
	}
	if len(v.Violations) != 60 {
		t.Errorf("verdict has %d violations, want 60", len(v.Violations))
	}
	if len(res.Violations) > store.MaxFeedbackBytes || len(fb.Violations) == 0 || len(fb.Violations) >= maxFeedbackViolations {
		t.Errorf("feedback of %d bytes with %d violations; want under %d bytes, fewer than %d",
			len(res.Violations), len(fb.Violations), store.MaxFeedbackBytes, maxFeedbackViolations)
	}
	// What is kept is the verdict's first violations, in order.
	for i, x := range fb.Violations {
		if w := v.Violations[i]; x.Code != w.Code || x.Field != w.Field || x.Detail != w.Detail {
			t.Fatalf("feedback violation %d is %+v, verdict has %+v", i, x, v.Violations[i])
		}
	}
}

// TestVerifyProposalTotals: lines may use the pair rule (minus balances),
// but the entry's total must be a single evidence amount or the finding's
// amount, and two balances never ground anything together.
func TestVerifyProposalTotals(t *testing.T) {
	const bank, cash = "Bank Charges - STPL", "HDFC Current 0001 - STPL"
	twoBalances := bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
		map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "balance_paise": 250000000},
		map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050, "balance_paise": 249000000})
	gstLine := bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
		map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -59000, "taxable": 50000, "igst": 9000})
	outstanding := bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
		map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "outstanding_amount": 4000000},
		map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050})
	zeros := bankSnapshot(verifyCompany, "2026-09-01", "2026-09-30",
		map[string]any{"txn_id": "HDFC-20260915-C1", "amount_paise": -11800000, "debit": 0, "credit": 0},
		map[string]any{"txn_id": "HDFC-20260920-C2", "amount_paise": -118050, "debit": 0})
	tests := []struct {
		name     string
		evidence *toolSnapshot
		mutate   func(*ExplanationArtifact)
		pass     bool
		field    string
	}{
		{"Dr X / Cr X where X is the difference of two balances", &twoBalances, func(a *ExplanationArtifact) {
			a.Proposal.Lines = []store.JournalLine{line(bank, 1000000, 0), line(cash, 0, 1000000)}
		}, false, "proposal.total_debit"},
		{"Dr X / Cr X where X is a bank balance", &twoBalances, func(a *ExplanationArtifact) {
			a.Proposal.Lines = []store.JournalLine{line(bank, 250000000, 0), line(cash, 0, 250000000)}
		}, false, "proposal.total_debit"},
		{"Dr X / Cr X where X is an outstanding amount", &outstanding, func(a *ExplanationArtifact) {
			a.Proposal.Lines = []store.JournalLine{line(bank, 4000000, 0), line(cash, 0, 4000000)}
		}, false, "proposal.lines[0].debit"},
		{"Dr 50 paise / Cr 50 paise with only zero leaves around", &zeros, func(a *ExplanationArtifact) {
			a.Proposal.Lines = []store.JournalLine{line(bank, 50, 0), line(cash, 0, 50)}
		}, false, "proposal.lines[1].credit"},
		{"a total that is the sum of two amounts", nil, func(a *ExplanationArtifact) {
			a.Proposal.Lines = []store.JournalLine{line(bank, 11800000, 0), line(bank, 118050, 0), line(cash, 0, 11918050)}
		}, false, "proposal.total_debit"},
		{"a GST split whose total is the bank line", &gstLine, func(a *ExplanationArtifact) {
			a.Explanation = "The bank debited ₹590.00: ₹500.00 of charges and ₹90.00 of GST."
			a.ActionNote = "Book the charge and the GST."
			a.CitedAmountsPaise = []money.Paise{59000, 50000, 9000}
			a.Proposal.Lines = []store.JournalLine{line(bank, 50000, 0), line(bank, 9000, 0), line(cash, 0, 59000)}
		}, true, ""},
		{"a GST split derived by difference", &gstLine, func(a *ExplanationArtifact) {
			a.Explanation = "The bank debited ₹590.00."
			a.ActionNote = "Book it."
			a.CitedAmountsPaise = []money.Paise{59000}
			// 41000 = 50000 - 9000 is a pair; the total 59000 is single.
			a.Proposal.Lines = []store.JournalLine{line(bank, 41000, 0), line(bank, 18000, 0), line(cash, 0, 59000)}
		}, false, "proposal.lines[1].debit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newVerifyFixture(t)
			tmp := fx.req
			if tt.evidence != nil {
				fx.swapEvidence(t, &tmp, *tt.evidence)
			}
			req := fx.explanation(t, tt.mutate)
			req.Finding, req.EvidenceRefs = tmp.Finding, tmp.EvidenceRefs
			_, v := fx.verify(t, req)
			if v.Pass != tt.pass {
				t.Fatalf("pass %v, want %v: %+v", v.Pass, tt.pass, v.Violations)
			}
			if tt.pass {
				return
			}
			if got := codesOf(v.Violations); !slices.Equal(got, []string{ViolationProposalAmountNotInEvidence}) {
				t.Errorf("codes %v: %+v", got, v.Violations)
			}
			if !slices.ContainsFunc(v.Violations, func(x Violation) bool { return x.Field == tt.field }) {
				t.Errorf("no violation on %s: %+v", tt.field, v.Violations)
			}
		})
	}
}

func TestScopeOutOfScope(t *testing.T) {
	sc, err := newScope(verifyCompany, verifyMonth)
	if err != nil {
		t.Fatal(err)
	}
	n := func(i int) json.Number { return json.Number(strconv.Itoa(i)) }
	for _, tt := range []struct {
		args map[string]any
		bad  []string
	}{
		{map[string]any{"company": verifyCompany, "from_date": "2026-09-01", "to_date": "2026-09-30"}, nil},
		{map[string]any{"company": verifyCompany, "from_date": "", "to_date": "2026-09-30"}, nil},
		{map[string]any{"company": otherCompany, "from_date": "2026-09-01", "to_date": "2026-10-01"}, []string{"company", "to_date"}},
		{map[string]any{"company": 7, "from": "2026-09-xx"}, []string{"company", "from"}},
		{map[string]any{"company": verifyCompany, "period": "2026-09"}, nil},
		{map[string]any{"company": verifyCompany, "period": "2026-08"}, []string{"period"}},
		{map[string]any{"through_month": "2026-09", "months": n(12)}, nil},
		{map[string]any{"through_month": "2026-10", "months": n(30)}, []string{"months", "through_month"}},
		{map[string]any{"before_month": "2026-10", "lookback_months": n(6)}, nil},
		{map[string]any{"before_month": "2024-08", "lookback_months": n(0)}, []string{"before_month", "lookback_months"}},
		{map[string]any{"through_month": "2026-9"}, []string{"through_month"}},
		{map[string]any{"account": "Rent - STPL", "limit": n(5)}, nil},
	} {
		if got := sc.outOfScope(tt.args); !slices.Equal(got, tt.bad) {
			t.Errorf("outOfScope(%v) = %v, want %v", tt.args, got, tt.bad)
		}
	}
}
