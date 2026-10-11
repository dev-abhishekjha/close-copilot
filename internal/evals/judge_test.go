package evals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func judgeCall(args string) llm.Response {
	return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: JudgeToolName, Args: json.RawMessage(args)}}}
}

func judgeFinding(typ, key, val string) store.Finding {
	ex := "Bank charge of 590.00 debited on 2025-04-03."
	return store.Finding{Type: typ, Keys: map[string]string{key: val}, Title: "t", Explanation: &ex}
}

func TestJudgeToolListAndForce(t *testing.T) {
	fp := llm.NewFakeProvider(llm.ScriptedCall{Response: judgeCall(`{"verdict":"supported","reason":"ok"}`)})
	j := &Judge{Provider: fp, Model: "strong-x", Rubric: "rubric"}
	g, err := j.Judge(context.Background(), judgeFinding("bank_charge", "txn", "T1"))
	if err != nil || g.Verdict != VerdictSupported || g.Item != "bank_charge|txn=T1" {
		t.Fatalf("got %+v, %v", g, err)
	}
	req := fp.Calls()[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "emit_judgement" || req.ForceTool != "emit_judgement" {
		t.Fatalf("tools %+v force %q", req.Tools, req.ForceTool)
	}
	if req.Model != "strong-x" || req.System[0].Text != "rubric" {
		t.Fatalf("request %+v", req)
	}
}

func TestJudgeMalformedRetriedOnce(t *testing.T) {
	good := judgeCall(`{"verdict":"partially","reason":"minor"}`)
	bad := judgeCall(`{"verdict":"great","reason":"x"}`)
	fp := llm.NewFakeProvider(llm.ScriptedCall{Response: bad}, llm.ScriptedCall{Response: good})
	j := &Judge{Provider: fp, Model: "m", Rubric: "r"}
	g, err := j.Judge(context.Background(), judgeFinding("a", "k", "v"))
	if err != nil || g.Verdict != VerdictPartially || fp.CallCount() != 2 {
		t.Fatalf("got %+v, %v, calls %d", g, err, fp.CallCount())
	}

	fp = llm.NewFakeProvider(llm.ScriptedCall{Response: bad}, llm.ScriptedCall{Response: llm.Response{Text: "no tool"}}, llm.ScriptedCall{Response: good})
	j.Provider = fp
	g, err = j.Judge(context.Background(), judgeFinding("a", "k", "v"))
	if err != nil || g.Verdict != "" || !strings.Contains(g.Err, "malformed") || fp.CallCount() != 2 {
		t.Fatalf("got %+v, %v, calls %d", g, err, fp.CallCount())
	}
}

func TestJudgeProviderErrorAndNoExplanation(t *testing.T) {
	boom := errors.New("boom")
	j := &Judge{Provider: llm.NewFakeProvider(llm.ScriptedCall{Err: boom}), Model: "m"}
	if _, err := j.Judge(context.Background(), judgeFinding("a", "k", "v")); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	gs, err := j.JudgeAll(context.Background(), []store.Finding{{Type: "x"}})
	if err != nil || len(gs) != 0 {
		t.Fatalf("got %v, %v", gs, err)
	}
}

func TestLoadLabels(t *testing.T) {
	l, err := LoadLabels("testdata/judge/labels.yaml")
	if err != nil || len(l.Labels) != 4 {
		t.Fatalf("got %+v, %v", l, err)
	}
	if _, err := LoadLabels("testdata/judge/labels_bad.yaml"); err == nil || !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("bad verdict accepted: %v", err)
	}
	if _, err := LoadLabels("testdata/judge/missing.yaml"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestAgreement(t *testing.T) {
	l, err := LoadLabels("testdata/judge/labels.yaml")
	if err != nil {
		t.Fatal(err)
	}
	js := []Judgement{
		{Item: "bank_charge|txn=T1", Verdict: VerdictSupported},
		{Item: "bank_charge|txn=T2", Verdict: VerdictSupported}, // disagree
		{Item: "duplicate|inv=I1", Verdict: VerdictUnsupported},
		// I2 not judged
	}
	a := Agreement(l, js)
	if a.Rate.Num != 2 || a.Rate.Den != 4 || !a.BelowThreshold() {
		t.Fatalf("rate %v", a.Rate)
	}
	if a.Confusion[VerdictPartially][VerdictSupported] != 1 || a.Confusion[VerdictSupported]["none"] != 1 {
		t.Fatalf("confusion %v", a.Confusion)
	}
	if len(a.Unjudged) != 1 || a.Unjudged[0] != "duplicate|inv=I2" || len(a.Disagree) != 2 {
		t.Fatalf("keys %v %v", a.Unjudged, a.Disagree)
	}
}

func TestJudgeReportRender(t *testing.T) {
	l, _ := LoadLabels("testdata/judge/labels.yaml")
	js := []Judgement{
		{Item: "bank_charge|txn=T1", Verdict: VerdictSupported},
		{Item: "bank_charge|txn=T2", Verdict: VerdictPartially},
		{Item: "duplicate|inv=I1", Verdict: VerdictUnsupported},
		{Item: "duplicate|inv=I2", Verdict: VerdictSupported},
		{Item: "z", Err: "malformed"},
	}
	r := NewJudgeReport(js, &l)
	out := RenderJudge(r)
	for _, want := range []string{"| Faithfulness (supported) | 2/4 (50.0%) |", "| Judge agreement | 4/4 (100.0%) |", "| Judge errors | 1 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "refine the rubric") || out != RenderJudge(r) {
		t.Error("unexpected warning or unstable render")
	}
	low := NewJudgeReport(js[:2], &l)
	if !strings.Contains(RenderJudge(low), "refine the rubric") {
		t.Error("no warning under 80%")
	}
	if !strings.Contains(RenderJudge(NewJudgeReport(js, nil)), "not measured") {
		t.Error("no-label case")
	}
}
