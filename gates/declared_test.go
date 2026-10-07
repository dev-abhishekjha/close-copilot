package gates

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// onTicketBranch starts branch cc-500-foo with the spec and one declared
// file committed, plus tmp/ files that must be ignored.
func onTicketBranch(t *testing.T) *testRepo {
	t.Helper()
	r := newTestRepo(t)
	r.git("checkout", "-q", "-b", "cc-500-foo")
	r.write("specs/CC-500.md", validSpec(t))
	r.write("internal/foo/foo.go", "package foo\n")
	r.commit("CC-500: foo")
	r.write("tmp/current-task", "CC-500\n")
	r.write("tmp/reports/G1.json", "{}\n")
	return r
}

func TestRunDeclared(t *testing.T) {
	tests := []struct {
		name     string
		change   func(r *testRepo)
		wantCode int
		want     []string // evidence (file paths) of blocking entries
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
			wantCode: ExitFail, want: []string{"internal/bar/bar.go"},
		},
		{
			name:     "undeclared untracked file fails",
			change:   func(r *testRepo) { r.write("cmd/x/main.go", "package main\n") },
			wantCode: ExitFail, want: []string{"cmd/x/main.go"},
		},
		{
			name:     "uncommitted edit to a tracked file fails",
			change:   func(r *testRepo) { r.write("README.md", "edited\n") },
			wantCode: ExitFail, want: []string{"README.md"},
		},
		{
			name: "staged deletion fails",
			change: func(r *testRepo) {
				r.git("rm", "-q", "README.md")
			},
			wantCode: ExitFail, want: []string{"README.md"},
		},
		{
			name: "rename out of the declared files fails on the new path",
			change: func(r *testRepo) {
				r.git("mv", "internal/foo/foo.go", "internal/foo.go")
				r.commit("CC-500: move")
			},
			wantCode: ExitFail, want: []string{"internal/foo.go"},
		},
		{
			name:     "another ticket's spec fails",
			change:   func(r *testRepo) { r.write("specs/CC-501.md", "x\n") },
			wantCode: ExitFail, want: []string{"specs/CC-501.md"},
		},
		{
			name: "a * does not cross a slash",
			change: func(r *testRepo) {
				r.write("scripts/sub/check-foo.sh", "#!/bin/sh\n")
			},
			wantCode: ExitFail, want: []string{"scripts/sub/check-foo.sh"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := onTicketBranch(t)
			tt.change(r)
			res := r.runCmd(RunDeclared, nil, "--spec", "specs/CC-500.md", "--base", "main")
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := evidence(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking evidence = %v, want %v", got, tt.want)
			}
			if res.report != nil {
				for _, b := range res.report.Blocking {
					if b.Check != "declared" || b.Repro != "go run ./gates/cmd/declared --spec specs/CC-500.md --base main" {
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
		wantCode int
		want     []string
	}{
		{name: "baseline without the label fails", change: editBaseline, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{name: "baseline with --labels approved passes", change: editBaseline, args: []string{"--labels", "approved"}, wantCode: ExitPass},
		{name: "another label is not approval", change: editBaseline, args: []string{"--labels", "ready, approved-ish"}, wantCode: ExitFail, want: []string{"evals/baseline.json"}},
		{name: "label in a list", change: editBaseline, args: []string{"--labels", "wip,approved"}, wantCode: ExitPass},
		{
			name: "label from the GitHub event payload", change: editBaseline,
			env:      func(t *testing.T) map[string]string { return eventWith(t, `{"name":"bug"},{"name":"approved"}`) },
			wantCode: ExitPass,
		},
		{
			name: "event payload without the label", change: editBaseline,
			env:      func(t *testing.T) map[string]string { return eventWith(t, `{"name":"bug"}`) },
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
		},
		{name: "unprotected change passes", change: func(*testRepo) {}, wantCode: ExitPass},
		{
			name: "every protected file is listed",
			change: func(r *testRepo) {
				r.write("gates/x.go", "package gates\n")
				r.write(".github/CODEOWNERS", "* @x\n")
				r.write("internal/mcpkit/auth_token.go", "package mcpkit\n")
				r.write("internal/mcpkit/server.go", "package mcpkit\n")
				r.write("CLAUDE.md", "x\n")
			},
			wantCode: ExitFail,
			want:     []string{".github/CODEOWNERS", "CLAUDE.md", "gates/x.go", "internal/mcpkit/auth_token.go"},
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
