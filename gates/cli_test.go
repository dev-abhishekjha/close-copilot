package gates

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageAndIOErrorsExit2(t *testing.T) {
	r := onTicketBranch(t)
	tests := []struct {
		name string
		fn   func(context.Context, Env, []string) int
		args []string
	}{
		{"ready without a spec", RunReady, nil},
		{"ready with two specs", RunReady, []string{"specs/CC-500.md", "specs/CC-501.md"}},
		{"ready with a missing spec", RunReady, []string{"specs/CC-999.md"}},
		{"ready with an unknown flag", RunReady, []string{"specs/CC-500.md", "--bogus"}},
		{"ready with an option as base", RunReady, []string{"specs/CC-500.md", "--base", "--output=x"}},
		{"ready with an unknown base", RunReady, []string{"specs/CC-500.md", "--base", "nope"}},
		{"declared without --spec", RunDeclared, nil},
		{"declared with a positional argument", RunDeclared, []string{"--spec", "specs/CC-500.md", "extra"}},
		{"declared with a missing spec", RunDeclared, []string{"--spec", "specs/CC-999.md"}},
		{"declared with an unknown base", RunDeclared, []string{"--spec", "specs/CC-500.md", "--base", "nope"}},
		{"protected with a positional argument", RunProtected, []string{"x"}},
		{"protected with a short approved SHA", RunProtected, []string{"--approved-sha", "abc123"}},
		{"protected with an upper-case approved SHA", RunProtected, []string{"--approved-sha", strings.Repeat("A", 40)}},
		{"graph without a subcommand", RunGraph, nil},
		{"graph with an unknown subcommand", RunGraph, []string{"list"}},
		{"graph check without a path", RunGraph, []string{"check"}},
		{"graph check with a missing file", RunGraph, []string{"check", "tasks/none.yaml"}},
		{"graph ready with a missing graph", RunGraph, []string{"ready", "--graph", "tasks/none.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := r.runCmd(tt.fn, nil, tt.args...)
			if res.code != ExitUsage {
				t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, ExitUsage, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, `"level":"ERROR"`) {
				t.Errorf("stderr has no JSON error log: %s", res.stderr)
			}
		})
	}
}

func TestProtectedBadEventPayloadExits2(t *testing.T) {
	r := onTicketBranch(t)
	p := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(p, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := r.runCmd(RunProtected, map[string]string{"GITHUB_EVENT_PATH": p}); res.code != ExitUsage {
		t.Errorf("exit %d, want %d", res.code, ExitUsage)
	}
}

func TestReportFlagWritesReport(t *testing.T) {
	r := onTicketBranch(t)
	r.write("evals/baseline.json", "{}\n")
	r.commit("CC-500: baseline")
	path := filepath.Join(t.TempDir(), "out", "G1.json")

	res := r.runCmd(RunProtected, nil, "--report", path, "--task", "CC-500", "--attempt", "2")
	if res.code != ExitFail {
		t.Fatalf("exit %d, want %d", res.code, ExitFail)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != VerdictFail || rep.Task != "CC-500" || rep.Attempt != 2 || len(rep.Blocking) != 1 {
		t.Errorf("report = %+v", rep)
	}

	// A later pass overwrites the failure.
	res = r.runCmd(RunProtected, nil, "--report", path, "--labels", "approved", "--approved-sha", strings.TrimSpace(r.git("rev-parse", "HEAD")))
	if res.code != ExitPass {
		t.Fatalf("exit %d, want %d", res.code, ExitPass)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"verdict": "pass"`) {
		t.Errorf("report after pass = %s", data)
	}
	if strings.Count(res.stdout, "\n") != 1 {
		t.Errorf("a pass prints only the summary line, got %q", res.stdout)
	}
}

func TestRunGraph(t *testing.T) {
	r := newTestRepo(t)
	r.write("tasks/graph.yaml", `tasks:
  - {id: CC-101, title: Skeleton, phase: 0, owner: implementer, risk: standard, human_review: false, depends_on: [], needs_erpnext: false, files: ["Makefile"], check: c}
  - {id: CC-102, title: Stack, phase: 0, owner: integration-engineer, risk: standard, human_review: false, depends_on: [], needs_erpnext: true, files: ["deploy/**"], check: c}
  - {id: CC-301, title: Profiles, phase: 1, owner: domain-data-engineer, risk: standard, human_review: false, depends_on: [CC-101], needs_erpnext: false, files: ["config/**"], check: c}
  - {id: CC-002, title: Analyzers, phase: 0, owner: implementer, risk: regulated, human_review: true, depends_on: [CC-101], needs_erpnext: false, files: ["gates/analyzers/**"], check: c}
  - {id: CC-401, title: Store, phase: 1, owner: implementer, risk: data-sensitive, human_review: false, depends_on: [CC-301], needs_erpnext: false, files: ["internal/store/**"], check: c}
`)
	r.write("tasks/bad.yaml", `tasks:
  - {id: CC-1, title: A, phase: 0, owner: implementer, risk: regulated, human_review: false, depends_on: [CC-2], files: [], check: c}
`)
	r.write("tasks/broken.yaml", "tasks: [\n")
	r.commit("CC-101: graph")
	r.branchWithSpec("cc-002-analyzers", "CC-002", "gates/analyzers/**")

	t.Run("ready", func(t *testing.T) {
		res := r.runCmd(RunGraph, nil, "ready", "--base", "main")
		if res.code != ExitPass {
			t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
		}
		lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
		if len(lines) != 4 || !strings.HasPrefix(lines[1], "CC-102 ") || !strings.HasPrefix(lines[2], "CC-301 ") {
			t.Errorf("unexpected output:\n%s", res.stdout)
		}
		if lines[3] != "2 tickets ready on main (1 done, 1 in flight)" {
			t.Errorf("summary = %q", lines[3])
		}
	})
	t.Run("ready json", func(t *testing.T) {
		res := r.runCmd(RunGraph, nil, "ready", "--json")
		if res.code != ExitPass {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
		var rows []readyRow
		if err := json.Unmarshal([]byte(res.stdout), &rows); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, res.stdout)
		}
		if len(rows) != 2 || rows[0].ID != "CC-102" || !rows[0].NeedsERPNext || rows[1].Owner != "domain-data-engineer" {
			t.Errorf("rows = %+v", rows)
		}
	})
	t.Run("check passes", func(t *testing.T) {
		if res := r.runCmd(RunGraph, nil, "check", "tasks/graph.yaml"); res.code != ExitPass {
			t.Errorf("exit %d\n%s", res.code, res.stdout)
		}
	})
	t.Run("check fails", func(t *testing.T) {
		res := r.runCmd(RunGraph, nil, "check", "tasks/bad.yaml")
		if res.code != ExitFail {
			t.Fatalf("exit %d\n%s", res.code, res.stdout)
		}
		if got := strings.Join(checks(res.report), ","); got != "depends_on,human_review" {
			t.Errorf("checks = %s", got)
		}
	})
	t.Run("check reports a parse error", func(t *testing.T) {
		res := r.runCmd(RunGraph, nil, "check", "tasks/broken.yaml")
		if res.code != ExitFail || strings.Join(checks(res.report), ",") != "parse" {
			t.Errorf("exit %d, checks %v", res.code, checks(res.report))
		}
	})
}

func TestCommandLine(t *testing.T) {
	got := CommandLine("protected", []string{"--base", "origin/main", "--labels", "a b", "it's"})
	want := `go run ./gates/cmd/protected --base origin/main --labels 'a b' 'it'\''s'`
	if got != want {
		t.Errorf("CommandLine = %s, want %s", got, want)
	}
}
