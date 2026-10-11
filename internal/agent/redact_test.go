package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/llm"
)

// Synthetic identifiers: they match the patterns but belong to no one.
const (
	testGSTIN  = "29ABCPK1234F1Z5"
	testPAN    = "ABCPK1234F" // the PAN inside testGSTIN
	otherGSTIN = "27QWERT6789Y2ZA"
	barePAN    = "ZXCVB5678N"
	testPerson = "Ramesh Iyer"
)

var (
	testKey  = bytes.Repeat([]byte{0x5a}, 32)
	testRun  = uuid.MustParse("00000000-0000-4000-8000-0000000000f1")
	tokenFmt = regexp.MustCompile(`^(GSTIN|PAN|PERSON)_[0-9a-f]{6}$`)
)

func TestRedactStableTokens(t *testing.T) {
	a := NewRedactor(testKey, testRun, []string{testPerson})
	b := NewRedactor(testKey, testRun, []string{testPerson})
	for _, v := range []string{testGSTIN, barePAN, testPerson} {
		t1, t2, t3 := a.MaskString(v), a.MaskString(v), b.MaskString(v)
		if t1 != t2 || t1 != t3 {
			t.Errorf("%s: tokens %s %s %s, want one stable token", tokenFmt.FindString(t1), t1, t2, t3)
		}
		if !tokenFmt.MatchString(t1) {
			t.Errorf("token %q is not PREFIX_ and six hex digits", t1)
		}
		otherRun := NewRedactor(testKey, uuid.MustParse("00000000-0000-4000-8000-0000000000f2"), []string{testPerson}).MaskString(v)
		otherKey := NewRedactor(bytes.Repeat([]byte{0x11}, 32), testRun, []string{testPerson}).MaskString(v)
		if otherRun == t1 || otherKey == t1 {
			t.Errorf("token %s does not change with the run (%s) or key (%s)", t1, otherRun, otherKey)
		}
	}
	// Distinct values, distinct tokens.
	seen := map[string]string{}
	for _, v := range []string{testGSTIN, otherGSTIN, testPAN, barePAN, "PQRST1111U", testPerson} {
		tok := a.MaskString(v)
		if prev, ok := seen[tok]; ok {
			t.Errorf("values %q and %q share token %s", prev, v, tok)
		}
		seen[tok] = v
	}
	if a.Len() != 6 {
		t.Errorf("Len %d, want 6", a.Len())
	}
}

func TestRedactCollisionLengthensToken(t *testing.T) {
	r := NewRedactor(testKey, testRun, nil)
	// Every value digests to the same bytes.
	r.sum = func(string, string) []byte { return bytes.Repeat([]byte{0xab}, 32) }
	first, second := r.MaskString(barePAN), r.MaskString("PQRST1111U")
	if first != "PAN_ababab" || second != "PAN_abababab" {
		t.Errorf("tokens %s and %s, want PAN_ababab then a longer one", first, second)
	}
	if r.MaskString(barePAN) != first || r.MaskString("PQRST1111U") != second {
		t.Error("tokens changed on reuse")
	}
	if got := r.UnmaskString(first + " " + second); got != barePAN+" PQRST1111U" {
		t.Errorf("unmask %q", got)
	}
}

func TestRedactGSTINBeforePAN(t *testing.T) {
	r := NewRedactor(testKey, testRun, nil)
	got := r.MaskString("GSTIN " + testGSTIN + "; PAN " + barePAN + "; invoice ABCDEFGHIJ; amount 590.00; ref 1234567890; ₹5,900")
	if strings.Count(got, "GSTIN_") != 1 || strings.Count(got, "PAN_") != 1 {
		t.Errorf("masked %q: want one GSTIN_ and one PAN_ token", got)
	}
	if strings.Contains(got, "ABCPK") || strings.Contains(got, barePAN) {
		t.Errorf("identifier left in %q", got)
	}
	for _, keep := range []string{"invoice ABCDEFGHIJ", "amount 590.00", "ref 1234567890", "₹5,900"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was touched: %q", keep, got)
		}
	}
	// The GSTIN's token is a GSTIN token, and unmasking restores it whole.
	if tok := r.MaskString(testGSTIN); !strings.HasPrefix(tok, "GSTIN_") || r.UnmaskString(tok) != testGSTIN {
		t.Errorf("GSTIN token %q", tok)
	}
	if r.UnmaskString(got) != "GSTIN "+testGSTIN+"; PAN "+barePAN+"; invoice ABCDEFGHIJ; amount 590.00; ref 1234567890; ₹5,900" {
		t.Errorf("round trip %q", r.UnmaskString(got))
	}
}

func TestRedactNames(t *testing.T) {
	r := NewRedactor(testKey, testRun, []string{"Ramesh", testPerson, " ", "ramesh iyer"})
	full := r.MaskString(testPerson)
	short := r.MaskString("Ramesh")
	if !strings.HasPrefix(full, "PERSON_") || !strings.HasPrefix(short, "PERSON_") || full == short {
		t.Fatalf("tokens %q %q", full, short)
	}
	tests := []struct{ in, want string }{
		{"NEFT/RAMESH IYER/SEP", "NEFT/" + full + "/SEP"},                 // upper case narration, longest first
		{"paid ramesh iyer and Ramesh", "paid " + full + " and " + short}, // case-insensitive
		{"RAMESHWARAM traders", "RAMESHWARAM traders"},                    // not a whole word
		{"Rameshiyer", "Rameshiyer"},
	}
	for _, tt := range tests {
		if got := r.MaskString(tt.in); got != tt.want {
			t.Errorf("MaskString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Restored in the profile's casing.
	if got := r.UnmaskString("NEFT/" + full + "/SEP"); got != "NEFT/Ramesh Iyer/SEP" {
		t.Errorf("unmask %q", got)
	}
	// An unknown token-shaped string is left alone.
	if got := r.UnmaskString("PERSON_000000 and GSTIN_ffffff"); got != "PERSON_000000 and GSTIN_ffffff" {
		t.Errorf("unknown tokens changed: %q", got)
	}
}

func TestRedactMaskJSON(t *testing.T) {
	name := `O"Brien <Rao>`
	r := NewRedactor(testKey, testRun, []string{name})
	in := []byte(`{"FINDING":{"title":"Supplier ` + testGSTIN + `","amount_paise":12345678901234,"rate":0.18,"ok":true,"none":null},` +
		`"EVIDENCE":[{"party":"O\"Brien \u003cRao\u003e","` + barePAN + `":"key","narration":"x\n` + barePAN + `"}],"DOCUMENTS":[]}`)
	out, unmask, err := r.Mask(in)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("masked JSON is invalid: %s", out)
	}
	s := string(out)
	for _, leak := range []string{testGSTIN, barePAN, "Brien"} {
		if strings.Contains(s, leak) {
			t.Errorf("masked JSON holds %q: %s", leak, s)
		}
	}
	for _, keep := range []string{`"amount_paise":12345678901234`, `"rate":0.18`, `"ok":true`, `"none":null`} {
		if !strings.Contains(s, keep) {
			t.Errorf("masked JSON lost %s: %s", keep, s)
		}
	}
	// Key order and top-level keys are kept.
	if !strings.HasPrefix(s, `{"FINDING":{"title":`) || !strings.Contains(s, `,"EVIDENCE":[`) || !strings.HasSuffix(s, `,"DOCUMENTS":[]}`) {
		t.Errorf("structure changed: %s", s)
	}
	// unmask is for text; JSON is restored string by string.
	if got := unmask([]byte("paid " + r.MaskString(name))); string(got) != "paid "+name {
		t.Errorf("unmask text %q", got)
	}
	var back map[string]any
	if err := json.Unmarshal(r.unmaskJSON(out), &back); err != nil {
		t.Fatalf("unmasked JSON invalid: %v", err)
	}
	if party := back["EVIDENCE"].([]any)[0].(map[string]any)["party"]; party != name {
		t.Errorf("party %q, want %q", party, name)
	}
	if _, _, err := r.Mask([]byte(`{"a":`)); err == nil {
		t.Error("broken JSON: want an error")
	}
	if _, _, err := r.Mask([]byte(`{} {}`)); err == nil {
		t.Error("trailing JSON: want an error")
	}
	// A list (ACCOUNTS) keeps its length.
	list := r.MaskStrings([]string{"Bank Charges - STPL", name + " Capital - STPL"})
	if len(list) != 2 || list[0] != "Bank Charges - STPL" || !strings.HasPrefix(list[1], "PERSON_") || r.UnmaskString(list[1]) != name+" Capital - STPL" {
		t.Errorf("MaskStrings %q", list)
	}
}

func TestRedactWrapProvider(t *testing.T) {
	r := NewRedactor(testKey, testRun, []string{testPerson})
	gTok, pTok, nTok := r.MaskString(testGSTIN), r.MaskString(barePAN), r.MaskString(testPerson)
	args := json.RawMessage(`{"party":"` + nTok + `","gstin":"` + gTok + `","amount_paise":590}`)
	inner := llm.NewFakeProvider(llm.ScriptedCall{Response: llm.Response{
		Model: "m", Text: "Paid " + nTok + " (" + pTok + ")",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "lookup", Args: args}},
		Usage:     llm.Usage{InputTokens: 7},
	}})
	p := WrapRedacting(inner, r)
	tools := []llm.ToolSpec{{Name: "lookup", Description: "Look up " + testGSTIN, InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req := llm.Request{
		Model:  "claude-haiku-5-5",
		System: []llm.Block{{Text: "Company GSTIN " + testGSTIN, Cacheable: true}},
		Messages: []llm.Message{
			llm.NewUserTextMessage("Supplier " + testPerson + " PAN " + barePAN),
			llm.NewAssistantToolCallMessage(llm.ToolCall{ID: "c0", Name: "lookup", Args: json.RawMessage(`{"gstin":"` + testGSTIN + `"}`)}),
			llm.NewUserToolResultMessage(llm.ToolResultBlock{ToolCallID: "c0", Content: `[{"supplier":"RAMESH IYER","supplier_gstin":"` + testGSTIN + `"}]`}),
		},
		Tools: tools, MaxTokens: 100,
	}
	resp, err := p.Complete(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := inner.LastCall()
	raw, _ := json.Marshal(struct {
		S []llm.Block
		M []llm.Message
	}{sent.System, sent.Messages})
	for _, leak := range []string{testGSTIN, barePAN, "Ramesh", "RAMESH"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("request holds %q: %s", leak, raw)
		}
	}
	if !json.Valid(sent.Messages[1].ToolCalls[0].Args) {
		t.Errorf("masked tool-call args invalid: %s", sent.Messages[1].ToolCalls[0].Args)
	}
	if sent.Model != req.Model || sent.MaxTokens != 100 || sent.Tools[0].Description != tools[0].Description || !sent.System[0].Cacheable {
		t.Errorf("untouchable fields changed: %+v", sent)
	}
	// The caller's request is not modified.
	if !strings.Contains(req.Messages[0].Content, testPerson) {
		t.Error("the caller's request was masked in place")
	}
	if resp.Text != "Paid "+testPerson+" ("+barePAN+")" || resp.Usage.InputTokens != 7 {
		t.Errorf("response text %q", resp.Text)
	}
	var got map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &got); err != nil || got["party"] != testPerson || got["gstin"] != testGSTIN {
		t.Errorf("tool-call args %s, %v", resp.ToolCalls[0].Args, err)
	}

	// Inner errors pass through, with their response.
	unpriced := llm.NewFakeProvider(llm.ScriptedCall{Response: llm.Response{Text: nTok}, Err: llm.ErrUnpricedModel})
	resp, err = WrapRedacting(unpriced, r).Complete(t.Context(), llm.Request{Model: "x"})
	if !errors.Is(err, llm.ErrUnpricedModel) || resp.Text != testPerson {
		t.Errorf("unpriced: %v, %q", err, resp.Text)
	}
	boom := errors.New("synthetic failure")
	if _, err := WrapRedacting(llm.NewFakeProvider(llm.ScriptedCall{Err: boom}), r).Complete(t.Context(), llm.Request{}); err != boom { //nolint:errorlint // must be the same error
		t.Errorf("err %v, want it unchanged", err)
	}
	if _, err := WrapRedacting(nil, r).Complete(t.Context(), llm.Request{}); err == nil {
		t.Error("no inner provider: want an error")
	}
}

// TestRedactLogsNoValues checks the explainer's pseudonymisation log
// line carries counts, never values or tokens.
func TestRedactLogsNoValues(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fx, _ := newRedactFixture(t, answer(withField(t, "proposal", "")))
	fx.ex.Log = log
	if _, err := fx.ex.Explain(t.Context(), fx.req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "identifiers pseudonymised") {
		t.Errorf("no count line in the log:\n%s", buf.String())
	}
	for _, leak := range []string{testGSTIN, testPAN, "Ramesh", "RAMESH", "GSTIN_", "PERSON_", "PAN_"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log holds %q:\n%s", leak, buf.String())
		}
	}
}
