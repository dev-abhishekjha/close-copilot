package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestNewFault(t *testing.T) {
	f, err := NewFault("")
	if err != nil || f != nil || f.Mode() != "" {
		t.Errorf("empty: %v %v", f, err)
	}
	f, err = NewFault(config.FaultCorruptExplanation)
	if err != nil || f.Mode() != config.FaultCorruptExplanation {
		t.Errorf("corrupt_explanation: %v %v", f, err)
	}
	for _, bad := range []string{"corrupt", "pass", "Corrupt_Explanation"} {
		if _, err := NewFault(bad); err == nil || !strings.Contains(err.Error(), config.EnvCopilotFault) {
			t.Errorf("%q: err %v, want a config error", bad, err)
		}
	}
	// The config rejects the same values.
	if _, err := config.Load(func(k string) (string, bool) {
		return "pass_everything", k == config.EnvCopilotFault
	}); err == nil {
		t.Error("config accepted an invalid COPILOT_FAULT")
	}
}

// TestFaultOnlyAppendsToTheExplanation checks the hard limits: the fault
// changes the explanation text and its mark, nothing else, once, and only
// on attempt 1.
func TestFaultOnlyAppendsToTheExplanation(t *testing.T) {
	base := ExplanationArtifact{
		Explanation: "The bank debited ₹5.90.", SuggestedAction: "book_entry", ActionNote: "Book it.",
		CitedAmountsPaise: []money.Paise{590}, Citations: []store.Citation{{DocID: "D", Section: "1"}},
		Proposal:    &store.JournalPayload{PostingDate: "2026-09-15", Lines: []store.JournalLine{{Account: "A", DebitPaise: 590}, {Account: "B", CreditPaise: 590}}},
		AccountsRef: "acc", DocumentRefs: []string{"doc"},
	}
	var off *Fault
	art := base
	if off.corruptExplanation(1, &art) || !reflect.DeepEqual(art, base) {
		t.Error("a nil fault changed the artifact")
	}
	f, _ := NewFault(config.FaultCorruptExplanation)
	art = base
	if f.corruptExplanation(2, &art) || !reflect.DeepEqual(art, base) {
		t.Error("the fault fired on attempt 2")
	}
	if !f.corruptExplanation(1, &art) {
		t.Fatal("the fault did not fire on attempt 1")
	}
	want := base
	want.Explanation += FaultSentence
	want.FaultInjected = true
	if !reflect.DeepEqual(art, want) {
		t.Errorf("fault changed more than the explanation:\n got %+v\nwant %+v", art, want)
	}
	again := base
	if f.corruptExplanation(1, &again) || !reflect.DeepEqual(again, base) {
		t.Error("the fault fired twice")
	}
	if f.corruptExplanation(1, nil) {
		t.Error("the fault fired on a nil artifact")
	}
}

// faultArtifacts returns the stored explanation artifacts marked
// fault_injected.
func faultArtifacts(t *testing.T, st *memStore) []ExplanationArtifact {
	t.Helper()
	st.fakeArtifactStore.mu.Lock()
	defer st.fakeArtifactStore.mu.Unlock()
	var out []ExplanationArtifact
	for sha, kind := range st.kinds {
		if kind != store.ArtifactExplanation {
			continue
		}
		var a ExplanationArtifact
		if err := json.Unmarshal(st.content[sha], &a); err != nil {
			t.Fatal(err)
		}
		if a.FaultInjected {
			out = append(out, a)
		}
	}
	return out
}

func TestFaultCorruptExplanation(t *testing.T) {
	t.Run("with the flag one finding is corrupted, rejected and retried", func(t *testing.T) {
		model := newAnsweringModel(nil)
		r, ex, _ := newVerifyRig(t, model)
		fault, err := NewFault(config.FaultCorruptExplanation)
		if err != nil {
			t.Fatal(err)
		}
		ex.Fault = fault
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil || res.Status != store.RunDone {
			t.Fatalf("RunClose %+v: %v", res, err)
		}
		corrupted := faultArtifacts(t, r.st)
		if len(corrupted) != 1 || !strings.HasSuffix(corrupted[0].Explanation, FaultSentence) {
			t.Fatalf("%d corrupted explanations, want 1", len(corrupted))
		}
		retried := 0
		for _, s := range r.st.stepsByKind(res.RunID)[store.StepKindExplain] {
			switch {
			case s.Subject == corrupted[0].FindingID:
				retried++
				var fb VerifierFeedback
				if err := json.Unmarshal(s.Feedback, &fb); err != nil || len(fb.Violations) != 1 ||
					fb.Violations[0].Code != ViolationAmountNotInEvidence || fb.Violations[0].ValuePaise == nil ||
					*fb.Violations[0].ValuePaise != 98765432 || s.Attempt != 2 {
					t.Errorf("corrupted finding's explain step %+v feedback %s", s, s.Feedback)
				}
			case s.Attempt != 1 || len(s.Feedback) != 0:
				t.Errorf("an uncorrupted finding was retried: %+v", s)
			}
		}
		if retried != 1 {
			t.Errorf("%d retried findings, want 1", retried)
		}
		fs, _ := r.st.ListFindingsByRun(t.Context(), res.RunID)
		for _, f := range fs {
			if !f.Verified || f.Status != "open" || f.Explanation == nil || strings.Contains(*f.Explanation, "9,87,654.32") {
				t.Errorf("finding %s: verified %v status %s", f.ID, f.Verified, f.Status)
			}
		}
		md := readFile(t, res.ReportPath)
		for _, want := range []string{
			"| Verification | verified 4 of 4 explanations (pass rate 100%), 1 retried, 0 needs_review |",
			"| Fault injection | COPILOT_FAULT=corrupt_explanation: one explanation was corrupted on purpose on its first attempt |",
		} {
			if !strings.Contains(md, want) {
				t.Errorf("report lacks %q:\n%s", want, md)
			}
		}
	})

	t.Run("without the flag nothing is altered", func(t *testing.T) {
		model := newAnsweringModel(nil)
		r, _, _ := newVerifyRig(t, model)
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil || res.Status != store.RunDone {
			t.Fatalf("RunClose %+v: %v", res, err)
		}
		if n := len(faultArtifacts(t, r.st)); n != 0 {
			t.Errorf("%d corrupted explanations without the flag", n)
		}
		for _, s := range r.st.stepsByKind(res.RunID)[store.StepKindExplain] {
			if s.Attempt != 1 || len(s.Feedback) != 0 {
				t.Errorf("explain step %+v", s)
			}
		}
		if model.count() != 4 || strings.Contains(readFile(t, res.ReportPath), "Fault injection") {
			t.Errorf("%d model calls", model.count())
		}
	})

	t.Run("the report's fault line comes from stored artifacts, not the process", func(t *testing.T) {
		model := newAnsweringModel(nil)
		r, ex, _ := newVerifyRig(t, model)
		fault, err := NewFault(config.FaultCorruptExplanation)
		if err != nil {
			t.Fatal(err)
		}
		// The process has the flag on, but its one corruption is spent,
		// so this run stores no corrupted explanation.
		if !fault.corruptExplanation(1, &ExplanationArtifact{}) {
			t.Fatal("the fault did not fire")
		}
		ex.Fault = fault
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil || res.Status != store.RunDone {
			t.Fatalf("RunClose %+v: %v", res, err)
		}
		if n := len(faultArtifacts(t, r.st)); n != 0 {
			t.Errorf("%d corrupted explanations", n)
		}
		if md := readFile(t, res.ReportPath); strings.Contains(md, "Fault injection") {
			t.Errorf("the report claims a fault no stored explanation shows:\n%s", md)
		}
	})

	t.Run("an invalid flag is a config error", func(t *testing.T) {
		if _, err := NewFault("corrupt_everything"); err == nil {
			t.Error("NewFault accepted an invalid value")
		}
		if _, err := config.Load(func(k string) (string, bool) {
			return "corrupt_everything", k == config.EnvCopilotFault
		}); err == nil || !strings.Contains(err.Error(), config.EnvCopilotFault) {
			t.Errorf("config: %v", err)
		}
	})
}
