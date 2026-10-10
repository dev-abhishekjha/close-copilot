package gates

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// onTicketBranch starts branch cc-500-foo with the spec committed as
// "CC-500: spec" and one declared file committed after it, plus untracked
// tmp/ files that must be ignored.
func onTicketBranch(t *testing.T) *testRepo {
	t.Helper()
	r := newTestRepo(t)
	r.git("checkout", "-q", "-b", "cc-500-foo")
	r.write("specs/CC-500.md", validSpec(t))
	r.commit("CC-500: spec")
	r.write("internal/foo/foo.go", "package foo\n")
	r.commit("CC-500: foo")
	r.write("tmp/current-task", "CC-500\n")
	r.write("tmp/reports/G1.json", "{}\n")
	return r
}

func TestRunDeclared(t *testing.T) {
	setSpec := func(old, new string) func(r *testRepo) {
		return func(r *testRepo) {
			s := validSpec(t)
			if !strings.Contains(s, old) {
				t.Fatalf("fixture lacks %q", old)
			}
			r.write("specs/CC-500.md", strings.Replace(s, old, new, 1))
		}
	}
	tests := []struct {
		name     string
		change   func(r *testRepo)
		env      map[string]string
		spec     string // --spec, default specs/CC-500.md
		wantCode int
		want     []string // checks of blocking entries
		evidence []string // evidence of blocking entries, when checked
	}{
		{name: "declared files pass", change: func(*testRepo) {}, wantCode: ExitPass},
		{
			name: "go.mod, go.sum and untracked declared files pass",
			change: func(r *testRepo) {
				r.write("go.mod", "module x\n")
				r.commit("CC-500: deps")
				r.write("go.sum", "")
				r.write("internal/foo/deep/bar_test.go", "package deep\n")
				r.write("scripts/check-foo.sh", "#!/bin/sh\n")
			},
			wantCode: ExitPass,
		},
		{
			name: "undeclared committed file fails",
			change: func(r *testRepo) {
				r.write("internal/bar/bar.go", "package bar\n")
				r.commit("CC-500: bar")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"internal/bar/bar.go"},
		},
		{
			name:     "undeclared untracked file fails",
			change:   func(r *testRepo) { r.write("cmd/x/main.go", "package main\n") },
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"cmd/x/main.go"},
		},
		{
			name:     "uncommitted edit to a tracked file fails",
			change:   func(r *testRepo) { r.write("README.md", "edited\n") },
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"README.md"},
		},
		{
			name: "staged deletion fails",
			change: func(r *testRepo) {
				r.git("rm", "-q", "README.md")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"README.md"},
		},
		{
			name: "rename out of the declared files fails on the new path",
			change: func(r *testRepo) {
				r.git("mv", "internal/foo/foo.go", "internal/foo.go")
				r.commit("CC-500: move")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"internal/foo.go"},
		},
		{
			name: "committed tmp/ file fails",
			change: func(r *testRepo) {
				r.write("tmp/x/x_test.go", "package x\n")
				r.git("add", "-f", "tmp/x/x_test.go")
				r.git("commit", "-q", "--no-verify", "-m", "CC-500: x")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"tmp/x/x_test.go"},
		},
		{
			name: "staged tmp/ file fails",
			change: func(r *testRepo) {
				r.write("tmp/x/x_test.go", "package x\n")
				r.git("add", "-f", "tmp/x/x_test.go")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"tmp/x/x_test.go"},
		},
		{
			name:     "another ticket's spec fails",
			change:   func(r *testRepo) { r.write("specs/CC-501.md", "x\n") },
			wantCode: ExitFail, want: []string{"spec"}, evidence: []string{"specs/CC-501.md"},
		},
		{
			name:     "another ticket's spec in another case fails",
			change:   func(r *testRepo) { r.write("specs/cc-501.md", "x\n") },
			wantCode: ExitFail, want: []string{"spec"}, evidence: []string{"specs/cc-501.md"},
		},
		{
			name: "a * does not cross a slash",
			change: func(r *testRepo) {
				r.write("scripts/sub/check-foo.sh", "#!/bin/sh\n")
			},
			wantCode: ExitFail, want: []string{"declared"}, evidence: []string{"scripts/sub/check-foo.sh"},
		},
		{
			name: "id that does not match the file name fails alone",
			change: func(r *testRepo) {
				r.write("specs/CC-501.md", validSpec(t))
				r.write("cmd/x/main.go", "package main\n")
			},
			spec:     "specs/CC-501.md",
			wantCode: ExitFail, want: []string{"id"}, evidence: []string{"specs/CC-501.md#id"},
		},
		{
			name:     "widened files after the spec commit fail",
			change:   setSpec("  - scripts/check-foo.sh\n", "  - scripts/check-foo.sh\n  - \"**\"\n"),
			wantCode: ExitFail, want: []string{"spec_pin"},
		},
		{
			name: "widened files committed after the spec commit fail",
			change: func(r *testRepo) {
				setSpec("  - scripts/check-foo.sh\n", "  - scripts/check-foo.sh\n  - cmd/**\n")(r)
				r.write("cmd/x/main.go", "package main\n")
				r.commit("CC-500: widen")
			},
			wantCode: ExitFail, want: []string{"spec_pin"},
		},
		{
			name:     "changed risk fails",
			change:   setSpec("risk: regulated", "risk: standard"),
			wantCode: ExitFail, want: []string{"spec_pin"},
		},
		{
			name:     "changed approved_by fails",
			change:   setSpec("approved_by: owner 2026-10-07", "approved_by: someone"),
			wantCode: ExitFail, want: []string{"spec_pin"},
		},
		{
			name:     "reordered files and other edits to the spec pass",
			change:   setSpec("  - internal/foo/**\n  - scripts/check-foo.sh\n", "  - scripts/check-foo.sh\n  - internal/foo/**\n"),
			wantCode: ExitPass,
		},
		{
			name: "commit with another ticket's subject fails",
			change: func(r *testRepo) {
				r.write("internal/foo/wip.go", "package foo\n")
				r.commit("CC-950: wip")
			},
			wantCode: ExitFail, want: []string{"commit_subject"},
		},
		{
			name: "commit without a ticket subject fails",
			change: func(r *testRepo) {
				r.write("internal/foo/wip.go", "package foo\n")
				r.commit("fix typo")
			},
			wantCode: ExitFail, want: []string{"commit_subject"},
		},
		{
			name: "merge commits are skipped",
			change: func(r *testRepo) {
				r.git("checkout", "-q", "main")
				r.write("other.txt", "x\n")
				r.commit("CC-101: other")
				r.git("checkout", "-q", "cc-500-foo")
				r.git("merge", "-q", "--no-ff", "--no-edit", "-m", "Merge main", "main")
			},
			wantCode: ExitPass,
		},
		{
			name: "branch not named for the spec fails",
			change: func(r *testRepo) {
				r.git("checkout", "-q", "-b", "feature-x")
			},
			wantCode: ExitFail, want: []string{"branch"}, evidence: []string{"branch feature-x"},
		},
		{
			name:     "GITHUB_HEAD_REF names the branch in CI",
			change:   func(r *testRepo) { r.git("checkout", "-q", "--detach") },
			env:      map[string]string{"GITHUB_HEAD_REF": "cc-500-foo"},
			wantCode: ExitPass,
		},
		{
			name:     "GITHUB_HEAD_REF for another ticket fails",
			change:   func(*testRepo) {},
			env:      map[string]string{"GITHUB_HEAD_REF": "cc-501-bar"},
			wantCode: ExitFail, want: []string{"branch"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := onTicketBranch(t)
			tt.change(r)
			spec := tt.spec
			if spec == "" {
				spec = "specs/CC-500.md"
			}
			res := r.runCmd(RunDeclared, tt.env, "--spec", spec, "--base", "main")
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := checks(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking checks = %v, want %v\n%s", got, tt.want, res.stdout)
			}
			if got := evidence(res.report); tt.evidence != nil && !slices.Equal(got, tt.evidence) {
				t.Errorf("blocking evidence = %v, want %v", got, tt.evidence)
			}
			if res.report != nil {
				for _, b := range res.report.Blocking {
					if b.Repro != "go run ./gates/cmd/declared --spec "+spec+" --base main" {
						t.Errorf("entry = %+v", b)
					}
				}
				if res.report.Gate != "G1" || res.report.Task != "CC-500" || res.report.MaxAttempts != 3 || res.report.Commit == "" {
					t.Errorf("report header = %+v", res.report)
				}
			}
		})
	}
}

// TestDeclaredSpecGlobsCannotAllowOtherSpecs: even a spec that declares
// specs/** may not change another ticket's spec.
func TestDeclaredSpecGlobsCannotAllowOtherSpecs(t *testing.T) {
	r := newTestRepo(t)
	r.git("checkout", "-q", "-b", "cc-500-foo")
	r.write("specs/CC-500.md", specWith(t, "CC-500", "specs/**", "internal/foo/**"))
	r.commit("CC-500: spec")
	r.write("specs/notes.md", "x\n")
	r.write("specs/CC-501.md", "x\n")
	res := r.runCmd(RunDeclared, nil, "--spec", "specs/CC-500.md", "--base", "main")
	if res.code != ExitFail {
		t.Fatalf("exit %d, want %d\n%s", res.code, ExitFail, res.stdout)
	}
	if got, want := evidence(res.report), []string{"specs/CC-501.md"}; !slices.Equal(got, want) {
		t.Errorf("blocking evidence = %v, want %v", got, want)
	}
}

func TestDeclaredSpecPinnedOnBase(t *testing.T) {
	tests := []struct {
		name     string
		edit     func(string) string
		wantCode int
		want     []string // evidence
	}{
		{name: "unchanged spec passes", edit: func(s string) string { return s }, wantCode: ExitPass},
		{
			name:     "prose edits pass",
			edit:     func(s string) string { return s + "\nMore notes.\n" },
			wantCode: ExitPass,
		},
		{
			name: "widened files fail",
			edit: func(s string) string {
				return strings.Replace(s, "  - internal/foo/**\n", "  - internal/foo/**\n  - \"**\"\n", 1)
			},
			wantCode: ExitFail, want: []string{"main:specs/CC-500.md#files"},
		},
		{
			name: "risk and approval changed fail",
			edit: func(s string) string {
				s = strings.Replace(s, "risk: regulated", "risk: data-sensitive", 1)
				return strings.Replace(s, "approved_by: owner 2026-10-07", "approved_by: \"\"", 1)
			},
			wantCode: ExitFail, want: []string{"main:specs/CC-500.md#risk", "main:specs/CC-500.md#approved_by"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRepo(t)
			r.write("specs/CC-500.md", validSpec(t))
			r.commit("CC-101: specs")
			// No "CC-500: spec" commit on the branch: the base pins the spec.
			r.git("checkout", "-q", "-b", "cc-500-foo")
			r.write("specs/CC-500.md", tt.edit(validSpec(t)))
			r.write("internal/foo/foo.go", "package foo\n")
			r.commit("CC-500: foo")
			res := r.runCmd(RunDeclared, nil, "--spec", "specs/CC-500.md", "--base", "main")
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\n%s%s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := evidence(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking evidence = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeclaredSpecWithoutPin(t *testing.T) {
	r := newTestRepo(t)
	r.git("checkout", "-q", "-b", "cc-500-foo")
	r.write("specs/CC-500.md", validSpec(t))
	r.write("internal/foo/foo.go", "package foo\n")
	r.commit("CC-500: foo and its spec")
	res := r.runCmd(RunDeclared, nil, "--spec", "specs/CC-500.md", "--base", "main")
	if res.code != ExitFail || !slices.Equal(checks(res.report), []string{"spec_pin"}) {
		t.Fatalf("exit %d, checks %v, want %d [spec_pin]\n%s", res.code, checks(res.report), ExitFail, res.stdout)
	}
}

func TestSpecPinProblems(t *testing.T) {
	base := Spec{ID: "CC-1", Files: []string{"a/**", "b.go"}, Risk: RiskStandard, ApprovedBy: ""}
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   []string
	}{
		{"same", func(*Spec) {}, nil},
		{"reordered", func(s *Spec) { s.Files = []string{"b.go", "a/**"} }, nil},
		{"title is pinned too", func(s *Spec) { s.Title = "x" }, []string{"x#title"}},
		{"added file", func(s *Spec) { s.Files = append(s.Files, "c.go") }, []string{"x#files"}},
		{"removed file", func(s *Spec) { s.Files = s.Files[:1] }, []string{"x#files"}},
		{"risk", func(s *Spec) { s.Risk = RiskRegulated }, []string{"x#risk"}},
		{"approval added", func(s *Spec) { s.ApprovedBy = "me" }, []string{"x#approved_by"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := base
			cur.Files = slices.Clone(base.Files)
			tt.mutate(&cur)
			var got []string
			for _, p := range SpecPinProblems(cur, base, "x") {
				got = append(got, p.Evidence)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("evidence = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommitSubjectProblems(t *testing.T) {
	commits := []Commit{
		{Hash: "1111111111111111", Subject: "CC-500: spec"},
		{Hash: "2222222222222222", Subject: "CC-5000: other ticket"},
		{Hash: "3333333333333333", Subject: "CC-500 no colon"},
		{Hash: "4444444444444444", Subject: "CC-500: work"},
		{Hash: "5555555555555555", Subject: "cc-500: lower case"},
	}
	var got []string
	for _, p := range CommitSubjectProblems("CC-500", commits) {
		got = append(got, p.Evidence)
	}
	want := []string{"commit 222222222222", "commit 333333333333", "commit 555555555555"}
	if !slices.Equal(got, want) {
		t.Errorf("evidence = %v, want %v", got, want)
	}
}

func TestRunProtected(t *testing.T) {
	eventWith := func(t *testing.T, labels string) map[string]string {
		p := filepath.Join(t.TempDir(), "event.json")
		body := `{"action":"labeled","pull_request":{"labels":[` + labels + `]}}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"GITHUB_EVENT_PATH": p}
	}
	editBaseline := func(r *testRepo) {
		r.write("evals/baseline.json", "{}\n")
		r.commit("CC-500: baseline")
	}
	tests := []struct {
		name     string
		change   func(r *testRepo)
		env      func(t *testing.T) map[string]string
		args     []string
		atHead   bool // pass --approved-sha with HEAD's hash
		wantCode int
		want     []string
	}{
		{name: "baseline without the label fails", change: editBaseline, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{name: "baseline with --labels approved at the head passes", change: editBaseline, args: []string{"--labels", "approved"}, atHead: true, wantCode: ExitPass},
		{name: "baseline with --labels approved but no approved SHA fails", change: editBaseline, args: []string{"--labels", "approved"}, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{name: "another label is not approval", change: editBaseline, args: []string{"--labels", "ready, approved-ish"}, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{name: "label in a list", change: editBaseline, args: []string{"--labels", "wip,approved"}, atHead: true, wantCode: ExitPass},
		{name: "approved SHA without the label fails", change: editBaseline, atHead: true, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{
			name: "label from the GitHub event payload", change: editBaseline,
			env:      func(t *testing.T) map[string]string { return eventWith(t, `{"name":"bug"},{"name":"approved"}`) },
			atHead:   true,
			wantCode: ExitPass,
		},
		{
			name: "event payload without the label", change: editBaseline,
			env:      func(t *testing.T) map[string]string { return eventWith(t, `{"name":"bug"}`) },
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
		},
		{name: "unprotected change passes", change: func(*testRepo) {}, wantCode: ExitPass},
		{
			name: "case variants of protected paths fail",
			change: func(r *testRepo) {
				r.write(".CLAUDE/hooks/guard-edit.sh", "#!/bin/sh\n")
				r.write("claude.md", "x\n")
				r.commit("CC-500: case")
				r.write("DOCS/x.md", "x\n")
			},
			wantCode: ExitFail,
			want:     []string{".CLAUDE/hooks/guard-edit.sh", "DOCS/x.md", "claude.md"},
		},
		{
			name: "--labels is refused in GitHub Actions", change: editBaseline,
			env:      func(*testing.T) map[string]string { return map[string]string{"GITHUB_ACTIONS": "true"} },
			args:     []string{"--labels", "approved"},
			wantCode: ExitUsage,
		},
		{
			name: "GitHub Actions takes the label from the event payload", change: editBaseline,
			env: func(t *testing.T) map[string]string {
				env := eventWith(t, `{"name":"approved"}`)
				env["GITHUB_ACTIONS"] = "true"
				return env
			},
			atHead:   true,
			wantCode: ExitPass,
		},
		{
			name: "every protected file is listed",
			change: func(r *testRepo) {
				r.write("gates/x.go", "package gates\n")
				r.write(".github/CODEOWNERS", "* @x\n")
				r.write("internal/mcpkit/auth_token.go", "package mcpkit\n")
				r.write("internal/mcpkit/server.go", "package mcpkit\n")
				r.write("CLAUDE.md", "x\n")
				r.write(".golangci.yml", "version: \"2\"\n")
			},
			wantCode: ExitFail,
			want:     []string{".github/CODEOWNERS", ".golangci.yml", "CLAUDE.md", "gates/x.go", "internal/mcpkit/auth_token.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := onTicketBranch(t)
			tt.change(r)
			var env map[string]string
			if tt.env != nil {
				env = tt.env(t)
			}
			args := append([]string{"--base", "main"}, tt.args...)
			if tt.atHead {
				args = append(args, "--approved-sha", strings.TrimSpace(r.git("rev-parse", "HEAD")))
			}
			res := r.runCmd(RunProtected, env, args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := evidence(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking evidence = %v, want %v", got, tt.want)
			}
			if got := checks(res.report); len(got) > 0 && got[0] != "protected" {
				t.Errorf("checks = %v", got)
			}
		})
	}
}

func TestSplitLabels(t *testing.T) {
	got := SplitLabels(" a, ,b ,approved,")
	if want := []string{"a", "b", "approved"}; !slices.Equal(got, want) {
		t.Errorf("SplitLabels = %v, want %v", got, want)
	}
}
