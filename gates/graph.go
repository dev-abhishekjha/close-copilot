package gates

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Task is one ticket in tasks/graph.yaml.
type Task struct {
	ID           string   `yaml:"id" json:"id"`
	Title        string   `yaml:"title" json:"title"`
	Phase        int      `yaml:"phase" json:"phase"`
	Owner        string   `yaml:"owner" json:"owner"`
	DraftedBy    string   `yaml:"drafted_by,omitempty" json:"drafted_by,omitempty"`
	Risk         string   `yaml:"risk" json:"risk"`
	HumanReview  bool     `yaml:"human_review" json:"human_review"`
	DependsOn    []string `yaml:"depends_on" json:"depends_on"`
	NeedsERPNext bool     `yaml:"needs_erpnext" json:"needs_erpnext"`
	Phase2Pass   bool     `yaml:"phase_2_pass" json:"phase_2_pass"`
	Files        []string `yaml:"files" json:"files"`
	Check        string   `yaml:"check" json:"check"`
}

// Graph is tasks/graph.yaml.
type Graph struct {
	Tasks []Task `yaml:"tasks"`
}

// Problem is one thing a check found wrong, before it becomes a Blocking
// entry with the command's repro line.
type Problem struct {
	Check    string
	Message  string
	Evidence string
}

// ParseGraph parses a task graph. Unknown fields are an error.
func ParseGraph(data []byte) (Graph, error) {
	var g Graph
	if err := decodeStrict(data, &g); err != nil {
		return Graph{}, fmt.Errorf("task graph: %w", err)
	}
	return g, nil
}

// LoadGraph reads and parses a task graph file.
func LoadGraph(path string) (Graph, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's command-line argument
	if err != nil {
		return Graph{}, fmt.Errorf("read task graph: %w", err)
	}
	return ParseGraph(data)
}

// CheckGraph validates a graph: unique IDs, known dependencies, no cycles,
// known owners and risk classes, human review on every regulated ticket and
// valid globs. path is used in evidence pointers.
func CheckGraph(g Graph, path string) []Problem {
	var out []Problem
	add := func(check, id, format string, args ...any) {
		out = append(out, Problem{Check: check, Message: fmt.Sprintf(format, args...), Evidence: path + "#" + id})
	}

	byID := make(map[string]Task, len(g.Tasks))
	for i, t := range g.Tasks {
		if t.ID == "" {
			add("id", fmt.Sprintf("tasks[%d]", i), "task %d has no id", i)
			continue
		}
		if _, dup := byID[t.ID]; dup {
			add("unique_id", t.ID, "%s appears more than once", t.ID)
			continue
		}
		byID[t.ID] = t
	}
	for _, t := range g.Tasks {
		if t.ID == "" {
			continue
		}
		for _, d := range t.DependsOn {
			if _, ok := byID[d]; !ok {
				add("depends_on", t.ID, "%s depends on %s, which is not in the graph", t.ID, d)
			}
		}
		if !validOwner(t.Owner) {
			add("owner", t.ID, "%s has owner %q; want one of %s", t.ID, t.Owner, strings.Join(Owners, ", "))
		}
		if t.DraftedBy != "" && !validOwner(t.DraftedBy) {
			add("owner", t.ID, "%s has drafted_by %q; want one of %s", t.ID, t.DraftedBy, strings.Join(Owners, ", "))
		}
		if !validRisk(t.Risk) {
			add("risk", t.ID, "%s has risk %q; want one of %s", t.ID, t.Risk, strings.Join(Risks, ", "))
		}
		if t.Risk == RiskRegulated && !t.HumanReview {
			add("human_review", t.ID, "%s is regulated but human_review is not true", t.ID)
		}
		for _, f := range t.Files {
			if err := ValidatePattern(f); err != nil {
				add("files", t.ID, "%s: %v", t.ID, err)
			}
		}
	}
	for _, cycle := range findCycles(g.Tasks, byID) {
		add("cycle", cycle[0], "dependency cycle: %s", strings.Join(cycle, " -> "))
	}
	return out
}

// findCycles runs a depth-first search in file order and returns each cycle
// it closes, as the path from the repeated ID back to itself.
func findCycles(tasks []Task, byID map[string]Task) [][]string {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(byID))
	var stack []string
	var cycles [][]string
	var visit func(id string)
	visit = func(id string) {
		state[id] = visiting
		stack = append(stack, id)
		for _, d := range byID[id].DependsOn {
			if _, ok := byID[d]; !ok {
				continue
			}
			switch state[d] {
			case unvisited:
				visit(d)
			case visiting:
				start := slices.Index(stack, d)
				cycle := append(slices.Clone(stack[start:]), d)
				cycles = append(cycles, cycle)
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = visited
	}
	for _, t := range tasks {
		if t.ID != "" && state[t.ID] == unvisited {
			visit(t.ID)
		}
	}
	return cycles
}

// ReadyTasks returns the tasks whose dependencies are all done and that are
// neither done nor in flight, sorted by phase and then ID.
func ReadyTasks(g Graph, done, inFlight map[string]bool) []Task {
	var out []Task
	for _, t := range g.Tasks {
		if done[t.ID] || inFlight[t.ID] {
			continue
		}
		ready := true
		for _, d := range t.DependsOn {
			if !done[d] {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b Task) int {
		if c := cmp.Compare(a.Phase, b.Phase); c != 0 {
			return c
		}
		return CompareIDs(a.ID, b.ID)
	})
	return out
}

var idParts = regexp.MustCompile(`^CC-([0-9]+)(.*)$`)

// CompareIDs orders ticket IDs by number, then suffix: CC-101 < CC-504 <
// CC-504a < CC-1001.
func CompareIDs(a, b string) int {
	ma, mb := idParts.FindStringSubmatch(a), idParts.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	na, _ := strconv.Atoi(ma[1])
	nb, _ := strconv.Atoi(mb[1])
	if c := cmp.Compare(na, nb); c != 0 {
		return c
	}
	return strings.Compare(ma[2], mb[2])
}
