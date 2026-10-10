package agent

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

var updateGolden = flag.Bool("update", false, "rewrite the report golden files")

func strp(s string) *string { return &s }

func paise(p money.Paise) *money.Paise { return &p }

// goldenReport is a fixed run: three planted bank charges, one with a
// title that tries to break the Markdown.
func goldenReport(outcome string) ReportData {
	run := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	ids := []uuid.UUID{
		uuid.MustParse("00000000-0000-4000-8000-0000000000a1"),
		uuid.MustParse("00000000-0000-4000-8000-0000000000a2"),
		uuid.MustParse("00000000-0000-4000-8000-0000000000a3"),
	}
	snap := strings.Repeat("ab", 32)
	ev := func(txn string) []store.EvidenceRef {
		return []store.EvidenceRef{{Server: "evidence", Tool: "list_bank_lines", Args: json.RawMessage(`{"company":"sharma"}`), IDs: []string{txn}, Artifact: snap}}
	}
	findings := []store.Finding{
		{ID: ids[0], RunID: run, Type: "unrecorded_bank_charge", Severity: "low", Title: "Unrecorded bank charge: NEFT CHARGES INCL GST (₹5.90)",
			AmountPaise: paise(590), Evidence: ev("HDFC-20260915-C1"), Action: strp("book_entry"), Status: "open"},
		{ID: ids[1], RunID: run, Type: "unrecorded_bank_charge", Severity: "medium", Title: "Unrecorded bank charge: DEBIT CARD | ANNUAL `FEE`\n# not a heading",
			AmountPaise: paise(59000), Evidence: ev("HDFC-20260920-C2"), Action: strp("book_entry"), Status: "open"},
		{ID: ids[2], RunID: run, Type: "unrecorded_bank_charge", Severity: "low", Title: "Unrecorded bank charge: SMS CHGS JUL-SEP 2026 (₹17.70)",
			AmountPaise: paise(1770), Evidence: ev("HDFC-20260930-C3"), Action: strp("book_entry"), Status: "open"},
	}
	steps := []store.Step{
		{RunID: run, Kind: "router", Subject: "close", Status: "done"},
		{RunID: run, Kind: "check.bankrec", Subject: "bankrec", Status: "done"},
		{RunID: run, Kind: "retrieve", Subject: "close", Status: "skipped", Error: strp(reasonRetrieve)},
		{RunID: run, Kind: "investigate", Subject: "close", Status: "skipped", Error: strp(reasonInvestigate)},
	}
	d := ReportData{RunID: run, Company: "sharma", Month: "2026-09", Outcome: outcome, Findings: findings}
	switch outcome {
	case store.RunPartial:
		d.Reason = "explanations missing for 3 of 3 findings"
		d.Usage = store.RunUsage{CostUSD: "0"}
		for _, f := range findings {
			steps = append(steps,
				store.Step{RunID: run, Kind: "explain", Subject: f.ID.String(), Status: "skipped", Error: strp(reasonNoExplainer)},
				store.Step{RunID: run, Kind: "verify", Subject: f.ID.String(), Status: "skipped", Error: strp(reasonNoExplanation)})
		}
	default:
		d.Usage = store.RunUsage{InputTokens: 2400, OutputTokens: 300, CacheReadTokens: 1200, CostUSD: "0.0042"}
		d.Findings[0].Explanation = strp("The bank debited ₹5.90 of NEFT charges on 15 Sep;\nthe books have no matching entry.")
		d.Findings[0].Verified = true
		d.Findings[0].Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
		d.Findings[0].Proposal = &store.JournalProposal{Status: "proposed", CompanyID: "sharma", Payload: store.JournalPayload{
			PostingDate: "2026-09-15", Remark: "NEFT charges\nper statement",
			Lines: []store.JournalLine{{Account: "Bank Charges - STPL", DebitPaise: 590}, {Account: "HDFC Current 0001 - STPL", CreditPaise: 590}},
		}}
		d.Findings[1].Status = "needs_review"
		d.Fault = "corrupt_explanation"
		for i, f := range findings {
			explain := store.Step{RunID: run, Kind: "explain", Subject: f.ID.String(), Status: "done", Attempt: 1, OutputRefs: []string{strings.Repeat("c", 63) + string(rune('0'+i))}}
			verify := store.Step{RunID: run, Kind: "verify", Subject: f.ID.String(), Status: "done", OutputRefs: []string{strings.Repeat("d", 63) + string(rune('0'+i))}}
			switch i {
			case 1: // failed verification three times: needs review
				explain.Attempt = 3
				explain.Feedback = json.RawMessage(`{"violations":[{"code":"amount_not_in_evidence"}]}`)
				verify.Error = strp("verification failed: amount_not_in_evidence after 3 explain attempts")
			case 2: // passed on the second attempt
				explain.Attempt = 2
				explain.Feedback = json.RawMessage(`{"violations":[{"code":"amount_not_in_evidence"}]}`)
			}
			steps = append(steps, explain, verify)
		}
	}
	d.Steps = steps
	return d
}

func TestRenderReportGolden(t *testing.T) {
	for _, outcome := range []string{store.RunDone, store.RunPartial} {
		t.Run(outcome, func(t *testing.T) {
			got := RenderReport(goldenReport(outcome))
			path := filepath.Join("testdata", "report", outcome+".md")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("report differs from %s (run with -update after checking):\n%s", path, got)
			}
		})
	}
}

func TestRenderReportKeepsLayout(t *testing.T) {
	md := RenderReport(goldenReport(store.RunDone))
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "# not a heading") {
			t.Errorf("a title broke onto its own line: %q", line)
		}
	}
	if strings.Count(md, "\n# ") != 0 || !strings.HasPrefix(md, "# Close report: sharma, 2026-09\n") {
		t.Error("report has more than one top-level heading")
	}
	// Findings with no explanation and no steps still render.
	d := goldenReport(store.RunPartial)
	d.Steps = nil
	if md := RenderReport(d); !strings.Contains(md, "- Explanation: missing\n") || !strings.Contains(md, "| explain | 3 findings | 3 not run |") {
		t.Errorf("report without steps:\n%s", md)
	}
}

func TestReportPathAndRestore(t *testing.T) {
	dir := t.TempDir()
	run := uuid.New()
	if got, want := ReportPath(dir, run), filepath.Join(dir, "runs", run.String()+".md"); got != want {
		t.Errorf("ReportPath %s, want %s", got, want)
	}
	if got := ReportPath("", run); got != filepath.Join("results", "runs", run.String()+".md") {
		t.Errorf("default ReportPath %s", got)
	}

	st := newFakeArtifactStore()
	mem := &memStore{fakeArtifactStore: st}
	sha, err := st.PutArtifact(context.Background(), store.ArtifactReport, run, uuid.Nil, ReportArtifact{RunID: run.String(), Markdown: "# report\n"})
	if err != nil {
		t.Fatal(err)
	}
	path, err := restoreReport(t.Context(), mem, dir, run, []string{sha})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "# report\n" {
		t.Errorf("restored %q, %v", b, err)
	}
	// Another run's report is refused.
	other := uuid.New()
	if _, err := restoreReport(t.Context(), mem, dir, other, []string{sha}); err == nil {
		t.Error("restored another run's report")
	}
	// A non-report artifact is refused.
	snap, _ := st.PutArtifact(context.Background(), store.ArtifactToolResult, run, uuid.Nil, map[string]string{"a": "b"})
	if _, err := restoreReport(t.Context(), mem, dir, uuid.New(), []string{snap}); err == nil {
		t.Error("restored a tool result as a report")
	}
}

func TestAverageUSD(t *testing.T) {
	for _, tt := range []struct {
		total string
		n     int
		want  string
	}{
		{"0.0042", 3, "0.0014"},
		{"0.0042", 1, "0.0042"},
		{"0", 3, "0"},
		{"1", 3, "0.333333"},
		{"2", 3, "0.666667"},
		{"12.5", 2, "6.25"},
		{"0.0000005", 1, "0"},
		{"0.000001", 2, "0.000001"},
		{"-1", 1, "unknown"},
		{"1e3", 1, "unknown"},
		{"", 1, "unknown"},
		{"1", 0, "unknown"},
	} {
		if got := averageUSD(tt.total, tt.n); got != tt.want {
			t.Errorf("averageUSD(%q, %d) = %q, want %q", tt.total, tt.n, got, tt.want)
		}
	}
}
