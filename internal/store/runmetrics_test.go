package store

import (
	"slices"
	"testing"
)

func TestRunMetricsStepFamily(t *testing.T) {
	tests := []struct{ kind, want string }{
		{"explain", "explain"}, {"investigate", "investigate"}, {"verify", "verify"},
		{"router", "other"}, {"check.bankrec", "other"}, {"retrieve", "other"},
		{"synthesize", "other"}, {"", "other"},
	}
	for _, tt := range tests {
		if got := stepFamily(tt.kind); got != tt.want {
			t.Errorf("stepFamily(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestRunMetricsOrder(t *testing.T) {
	in := []StepUsage{{Kind: "other"}, {Kind: "verify"}, {Kind: "explain"}, {Kind: "investigate"}}
	var got []string
	for _, u := range sortStepUsage(in) {
		got = append(got, u.Kind)
	}
	if want := []string{"explain", "investigate", "verify", "other"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if got := sortStepUsage([]StepUsage{{Kind: "verify"}}); len(got) != 1 {
		t.Errorf("single = %v", got)
	}
	if got := sortStepUsage(nil); len(got) != 0 {
		t.Errorf("nil = %v", got)
	}
}
