package gates

import (
	"slices"
	"strings"
	"testing"
)

func TestRepoGraphIsValid(t *testing.T) {
	g, err := LoadGraph("../tasks/graph.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if problems := CheckGraph(g, "tasks/graph.yaml"); len(problems) > 0 {
		t.Errorf("tasks/graph.yaml has problems: %+v", problems)
	}
}

func TestCheckGraph(t *testing.T) {
	const header = "tasks:\n"
	task := func(id, owner, risk string, review bool, deps ...string) string {
		return "  - id: " + id + "\n    title: t\n    phase: 0\n    owner: " + owner + "\n    risk: " + risk +
			"\n    human_review: " + map[bool]string{true: "true", false: "false"}[review] +
			"\n    depends_on: [" + strings.Join(deps, ", ") + "]\n    files: [\"a/**\"]\n    check: c\n"
	}
	tests := []struct {
		name  string
		yaml  string
		check []string // expected problem checks, in order
	}{
		{"valid", header + task("CC-1", "implementer", "standard", false) + task("CC-2", "human", "regulated", true, "CC-1"), nil},
		{"duplicate id", header + task("CC-1", "implementer", "standard", false) + task("CC-1", "implementer", "standard", false), []string{"unique_id"}},
		{"unknown dependency", header + task("CC-1", "implementer", "standard", false, "CC-9"), []string{"depends_on"}},
		{"cycle", header + task("CC-1", "implementer", "standard", false, "CC-2") + task("CC-2", "implementer", "standard", false, "CC-1"), []string{"cycle"}},
		{"self cycle", header + task("CC-1", "implementer", "standard", false, "CC-1"), []string{"cycle"}},
		{"unknown owner", header + task("CC-1", "intern", "standard", false), []string{"owner"}},
		{"security reviewer owns nothing", header + task("CC-1", "security-reviewer", "standard", false), []string{"owner"}},
		{"unknown risk", header + task("CC-1", "implementer", "risky", false), []string{"risk"}},
		{"regulated without review", header + task("CC-1", "implementer", "regulated", false), []string{"human_review"}},
		{"braces in files", strings.Replace(header+task("CC-1", "implementer", "standard", false), `"a/**"`, `"a/{b,c}"`, 1), []string{"files"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := ParseGraph([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range CheckGraph(g, "graph.yaml") {
				got = append(got, p.Check)
				if p.Evidence == "" || p.Message == "" {
					t.Errorf("problem without evidence or message: %+v", p)
				}
			}
			if !slices.Equal(got, tt.check) {
				t.Errorf("checks = %v, want %v", got, tt.check)
			}
		})
	}
}

func TestParseGraphRejectsUnknownFields(t *testing.T) {
	if _, err := ParseGraph([]byte("tasks:\n  - id: CC-1\n    ownr: implementer\n")); err == nil {
		t.Error("ParseGraph accepted an unknown field")
	}
}

func TestReadyTasks(t *testing.T) {
	g := Graph{Tasks: []Task{
		{ID: "CC-1001", Phase: 1},
		{ID: "CC-101", Phase: 0},
		{ID: "CC-201", Phase: 1, DependsOn: []string{"CC-101"}},
		{ID: "CC-504b", Phase: 1, DependsOn: []string{"CC-101"}},
		{ID: "CC-504a", Phase: 1, DependsOn: []string{"CC-101"}},
		{ID: "CC-302", Phase: 1, DependsOn: []string{"CC-101", "CC-301"}},
		{ID: "CC-301", Phase: 1},
		{ID: "CC-002", Phase: 0},
	}}
	done := map[string]bool{"CC-101": true}
	inFlight := map[string]bool{"CC-301": true}
	var got []string
	for _, task := range ReadyTasks(g, done, inFlight) {
		got = append(got, task.ID)
	}
	want := []string{"CC-002", "CC-201", "CC-504a", "CC-504b", "CC-1001"}
	if !slices.Equal(got, want) {
		t.Errorf("ReadyTasks = %v, want %v", got, want)
	}
}

func TestCompareIDs(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"CC-101", "CC-1001", -1},
		{"CC-504", "CC-504a", -1},
		{"CC-504a", "CC-504b", -1},
		{"CC-002", "CC-002", 0},
		{"CC-900", "CC-0901", -1},
		{"X-1", "CC-1", 1},
	}
	for _, tt := range tests {
		if got := CompareIDs(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareIDs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTicketFromBranch(t *testing.T) {
	tests := map[string]string{
		"cc-602-bank-rec":  "CC-602",
		"cc-001-harness":   "CC-001",
		"cc-504a-x":        "CC-504a",
		"cc-602":           "",
		"main":             "",
		"feature/cc-602-x": "",
	}
	for branch, want := range tests {
		if got := TicketFromBranch(branch); got != want {
			t.Errorf("TicketFromBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}
