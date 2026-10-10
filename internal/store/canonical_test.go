package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestCanonical(t *testing.T) {
	type inner struct {
		Zeta  string `json:"zeta"`
		Alpha int64  `json:"alpha"`
	}
	type outer struct {
		B     []inner        `json:"b"`
		A     map[string]any `json:"a"`
		Paise int64          `json:"paise"`
	}
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"object keys sorted", map[string]int{"b": 2, "a": 1, "c": 3}, `{"a":1,"b":2,"c":3}`},
		{"struct field order ignored", inner{Zeta: "z", Alpha: 1}, `{"alpha":1,"zeta":"z"}`},
		{
			"nested maps and arrays",
			outer{B: []inner{{Zeta: "y", Alpha: 2}}, A: map[string]any{"y": map[string]any{"q": 1, "p": []any{3, 2}}, "x": nil}, Paise: 5},
			`{"a":{"x":null,"y":{"p":[3,2],"q":1}},"b":[{"alpha":2,"zeta":"y"}],"paise":5}`,
		},
		{"int64 beyond 2^53 exact", map[string]int64{"n": 9007199254740993}, `{"n":9007199254740993}`},
		{"max int64 exact", []int64{9223372036854775807, -9223372036854775808}, `[9223372036854775807,-9223372036854775808]`},
		{"raw big integer beyond int64", json.RawMessage(`{"n":123456789012345678901234567890}`), `{"n":123456789012345678901234567890}`},
		{"whitespace removed", json.RawMessage("{ \"b\" : [ 1 , 2 ] ,\n \"a\" : true }"), `{"a":true,"b":[1,2]}`},
		{"exponent and trailing zeros normalised", json.RawMessage(`[1.50,15e-1,1E2,0.000,-0,1.5e-7,0.0010]`), `[1.5,1.5,100,0,0,0.00000015,0.001]`},
		{"decimal text kept exact", json.RawMessage(`{"rate":"2.5","amt":12345678901234567.89}`), `{"amt":12345678901234567.89,"rate":"2.5"}`},
		{"null", nil, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.Canonical(tt.in)
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Canonical = %s, want %s", got, tt.want)
			}
			// Canonical is idempotent.
			again, err := store.Canonical(json.RawMessage(got))
			if err != nil {
				t.Fatalf("Canonical of canonical: %v", err)
			}
			if string(again) != string(got) {
				t.Errorf("not idempotent: %s then %s", got, again)
			}
		})
	}
}

func TestCanonicalHashSameValue(t *testing.T) {
	a := map[string]any{"server": "evidence", "tool": "list_bank_lines", "args": map[string]any{"company": "sharma", "to_date": "2026-08-31", "from_date": "2026-08-01"}}
	b := json.RawMessage(`{"tool":"list_bank_lines","args":{"from_date":"2026-08-01","company":"sharma","to_date":"2026-08-31"},"server":"evidence"}`)
	ba, sa, err := store.CanonicalHash(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, sb, err := store.CanonicalHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if sa != sb || string(ba) != string(bb) {
		t.Fatalf("same value, different hashes: %s %s / %s %s", sa, ba, sb, bb)
	}
	sum := sha256.Sum256(ba)
	if want := hex.EncodeToString(sum[:]); sa != want {
		t.Errorf("hash %s, want lowercase hex sha256 %s", sa, want)
	}
	if sa != strings.ToLower(sa) || len(sa) != 64 {
		t.Errorf("hash %q is not 64 lowercase hex characters", sa)
	}
	_, sc, err := store.CanonicalHash(map[string]any{"server": "evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if sc == sa {
		t.Error("different values gave the same hash")
	}
}

func TestCanonicalErrors(t *testing.T) {
	tests := []struct {
		name string
		in   any
		is   error
	}{
		{"unmarshalable", map[string]any{"c": make(chan int)}, nil},
		{"huge exponent", json.RawMessage(`[1e999999]`), nil},
		{"NUL in a string", map[string]string{"narration": "SMS\x00CHGS"}, store.ErrUnstorableContent},
		{"escaped NUL in raw JSON", json.RawMessage(`{"narration":"SMS\u0000CHGS"}`), store.ErrUnstorableContent},
		{"NUL in a key", map[string]int{"a\x00b": 1}, store.ErrUnstorableContent},
		{"invalid UTF-8 in a string", []string{"ok", "bad \xff\xfe byte"}, store.ErrUnstorableContent},
		{"invalid UTF-8 in a key", map[string]int{"\xc3": 1}, store.ErrUnstorableContent},
		{"literal U+FFFD", map[string]string{"t": "\ufffd"}, store.ErrUnstorableContent},
		{"nested", map[string]any{"a": []any{map[string]any{"b": "x\x00"}}}, store.ErrUnstorableContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.Canonical(tt.in)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error %v, want %v", err, tt.is)
			}
			if strings.Contains(err.Error(), "SMS") || strings.Contains(err.Error(), "bad") {
				t.Errorf("error carries content: %v", err)
			}
		})
	}
	// Valid non-ASCII text is fine.
	if _, err := store.Canonical(map[string]string{"name": "Sharma Traders \u20b9 \u0936\u0930\u094d\u092e\u093e"}); err != nil {
		t.Errorf("valid UTF-8 rejected: %v", err)
	}
}

// TestStoreLinksNoLLMClient keeps the data layer free of the LLM client:
// the MCP servers use the store, and must not link the Anthropic SDK.
func TestStoreLinksNoLLMClient(t *testing.T) {
	const pkg = "github.com/abhishekjha/close-copilot/internal/store"
	out, err := exec.CommandContext(t.Context(), goTool(t), "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	for dep := range strings.FieldsSeq(string(out)) {
		if strings.HasSuffix(dep, "/internal/llm") || strings.Contains(dep, "/internal/llm/") || strings.Contains(dep, "anthropic") {
			t.Errorf("%s depends on %s; the store must not link an LLM client", pkg, dep)
		}
	}
}

func TestValidateKinds(t *testing.T) {
	tests := []struct {
		kind    string
		step    bool
		wantErr bool
	}{
		{"router", true, false},
		{"check.bankrec", true, false},
		{"check.gstr2b_recon", true, false},
		{"check.", true, true},
		{"check.Bad Name", true, true},
		{"synthesize", true, false},
		{"explainer", true, true},
		{"", true, true},
		{"tool_result", false, false},
		{"prompt", false, false},
		{"report", false, false},
		{"snapshot", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			var err error
			if tt.step {
				err = store.ValidateStepKind(tt.kind)
			} else {
				err = store.ValidateArtifactKind(tt.kind)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("validate %q: err %v, wantErr %v", tt.kind, err, tt.wantErr)
			}
		})
	}
	for _, s := range []string{"pending", "running", "done", "failed", "skipped"} {
		if err := store.ValidateStepStatus(s); err != nil {
			t.Errorf("status %q: %v", s, err)
		}
	}
	if err := store.ValidateStepStatus("finished"); err == nil {
		t.Error("status finished should be invalid")
	}
}
