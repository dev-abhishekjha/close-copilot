package gates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReportAddRejectsIncompleteEntries(t *testing.T) {
	tests := []struct {
		name    string
		b       Blocking
		wantErr bool
	}{
		{"complete", Blocking{Check: "declared", Message: "m", Repro: "go run ./gates/cmd/declared", Evidence: "a.go"}, false},
		{"no repro", Blocking{Check: "declared", Message: "m", Evidence: "a.go"}, true},
		{"no evidence", Blocking{Check: "declared", Message: "m", Repro: "go run ./gates/cmd/declared"}, true},
		{"no check", Blocking{Message: "m", Repro: "r", Evidence: "e"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReport("G1", "CC-500")
			err := r.Add(tt.b)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Add() error = %v, wantErr %v", err, tt.wantErr)
			}
			wantVerdict, wantLen := VerdictFail, 1
			if tt.wantErr {
				wantVerdict, wantLen = VerdictPass, 0
			}
			if r.Verdict != wantVerdict || len(r.Blocking) != wantLen {
				t.Errorf("verdict %q with %d entries, want %q with %d", r.Verdict, len(r.Blocking), wantVerdict, wantLen)
			}
		})
	}
}

func TestReportValidate(t *testing.T) {
	good := Blocking{Check: "c", Message: "m", Repro: "r", Evidence: "e"}
	tests := []struct {
		name    string
		r       Report
		wantErr bool
	}{
		{"pass", Report{Verdict: VerdictPass}, false},
		{"fail", Report{Verdict: VerdictFail, Blocking: []Blocking{good}}, false},
		{"unknown verdict", Report{Verdict: "maybe"}, true},
		{"pass with entries", Report{Verdict: VerdictPass, Blocking: []Blocking{good}}, true},
		{"fail without entries", Report{Verdict: VerdictFail}, true},
		{"entry appended without repro", Report{Verdict: VerdictFail, Blocking: []Blocking{{Check: "c", Evidence: "e"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.r.JSON(); (err != nil) != tt.wantErr {
				t.Errorf("JSON() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReportSaveShape(t *testing.T) {
	r := NewReport("G1", "CC-500")
	r.Commit, r.Attempt, r.MaxAttempts = "abc1234", 2, 3
	if err := r.Add(Blocking{Check: "declared", Message: "m", Repro: "go run x", Evidence: "a.go"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reports", "G1.json")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"gate", "task", "commit", "attempt", "max_attempts", "verdict", "blocking"} {
		if _, ok := got[k]; !ok {
			t.Errorf("report JSON lacks %q: %s", k, data)
		}
	}
	entry := got["blocking"].([]any)[0].(map[string]any)
	for _, k := range []string{"check", "message", "repro", "evidence"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("blocking entry lacks %q: %s", k, data)
		}
	}
	if got["verdict"] != "fail" || got["attempt"] != 2.0 || got["max_attempts"] != 3.0 {
		t.Errorf("unexpected report: %s", data)
	}
}
