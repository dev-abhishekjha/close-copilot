package gates

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestRunReady(t *testing.T) {
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				t.Fatalf("fixture lacks %q", old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	tests := []struct {
		name     string
		file     string // spec path, default specs/CC-500.md
		mutate   func(string) string
		setup    func(r *testRepo)
		wantCode int
		want     []string // blocking checks
	}{
		{name: "valid spec passes", wantCode: ExitPass},
		{
			name:     "unmerged dependency",
			mutate:   replace("depends_on: [CC-101]", "depends_on: [CC-101, CC-102]"),
			wantCode: ExitFail, want: []string{"depends_on"},
		},
		{
			name: "dependency merged by its branch counts as done",
			setup: func(r *testRepo) {
				r.git("checkout", "-q", "-b", "cc-102-stack")
				r.write("deploy/x.yml", "x\n")
				r.commit("CC-102: stack")
				r.git("checkout", "-q", "main")
				r.git("merge", "-q", "--no-ff", "-m", "Merge cc-102-stack", "cc-102-stack")
			},
			mutate:   replace("depends_on: [CC-101]", "depends_on: [CC-101, CC-102]"),
			wantCode: ExitPass,
		},
		{
			name:     "overlap with an in-flight branch",
			setup:    func(r *testRepo) { r.branchWithSpec("cc-778-clash", "CC-778", "internal/foo/bar.go") },
			wantCode: ExitFail, want: []string{"overlap"},
		},
		{
			name: "own ticket's branch is not an overlap",
			setup: func(r *testRepo) {
				r.branchWithSpec("cc-500-foo", "CC-500", "internal/foo/**")
			},
			wantCode: ExitPass,
		},
		{
			name:     "prose acceptance line",
			mutate:   replace("  - test -f internal/foo/foo.go\n", "  - test -f internal/foo/foo.go\n  - Debits equal credits\n"),
			wantCode: ExitFail, want: []string{"acceptance"},
		},
		{
			name:     "undeclared missing script",
			mutate:   replace("  - scripts/check-foo.sh\n  - test", "  - scripts/other.sh\n  - test"),
			wantCode: ExitFail, want: []string{"acceptance"},
		},
		{
			name:     "existing undeclared script is runnable",
			setup:    func(r *testRepo) { r.write("scripts/other.sh", "#!/bin/sh\n") },
			mutate:   replace("  - scripts/check-foo.sh\n  - test", "  - ./scripts/other.sh\n  - test"),
			wantCode: ExitPass,
		},
		{
			name:     "regulated spec without approved_by",
			mutate:   replace("approved_by: owner 2026-10-07", "approved_by:"),
			wantCode: ExitFail, want: []string{"approved_by"},
		},
		{
			name: "standard spec without approved_by",
			mutate: func(s string) string {
				return replace("approved_by: owner 2026-10-07", "approved_by:")(replace("risk: regulated", "risk: standard")(s))
			},
			wantCode: ExitPass,
		},
		{
			name:     "braces in files",
			setup:    func(r *testRepo) { r.branchWithSpec("cc-777-other", "CC-777", "internal/bar/**") },
			mutate:   replace("  - internal/foo/**", "  - internal/{foo,bar}/**"),
			wantCode: ExitFail, want: []string{"files"},
		},
		{
			name:     "id does not match the file name",
			mutate:   replace("id: CC-500", "id: CC-501"),
			wantCode: ExitFail, want: []string{"id"},
		},
		{
			name:     "regulated spec without G6",
			mutate:   replace("gates: [G1, G2, G3, G5, G6]", "gates: [G1, G2, G3, G5]"),
			wantCode: ExitFail, want: []string{"gates"},
		},
		{
			name: "standard spec needs only G1 to G3",
			mutate: func(s string) string {
				return replace("gates: [G1, G2, G3, G5, G6]", "gates: [G1, G2, G3]")(replace("risk: regulated", "risk: standard")(s))
			},
			wantCode: ExitPass,
		},
		{
			name: "data-sensitive spec without G5",
			mutate: func(s string) string {
				return replace("gates: [G1, G2, G3, G5, G6]", "gates: [G1, G3]")(replace("risk: regulated", "risk: data-sensitive")(s))
			},
			wantCode: ExitFail, want: []string{"gates"},
		},
		{
			name: "missing title and budget",
			mutate: func(s string) string {
				return replace("title: Example ticket for the gate tests\n", "")(replace("budget: {max_attempts: 3, max_wall_minutes: 60}\n", "")(s))
			},
			wantCode: ExitFail, want: []string{"required", "required"},
		},
		{
			name:     "phase 0 is not missing",
			mutate:   replace("phase: 1", "phase: 0"),
			wantCode: ExitPass,
		},
		{
			name:     "unknown risk",
			mutate:   replace("risk: regulated", "risk: spicy"),
			wantCode: ExitFail, want: []string{"required"},
		},
		{
			name:     "front matter does not parse",
			mutate:   replace("phase: 1", "phase: [1"),
			wantCode: ExitFail, want: []string{"front_matter"},
		},
		{
			name:     "misspelt field",
			mutate:   replace("needs_erpnext: false", "need_erpnext: true"),
			wantCode: ExitFail, want: []string{"front_matter"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRepo(t)
			r.branchWithSpec("cc-777-unrelated", "CC-777", "internal/bar/**")
			r.write("tmp/current-task", "CC-500\n")
			if tt.setup != nil {
				tt.setup(r)
			}
			spec := validSpec(t)
			if tt.mutate != nil {
				spec = tt.mutate(spec)
			}
			r.write("specs/CC-500.md", spec)

			res := r.runCmd(RunReady, nil, "specs/CC-500.md", "--base", "main")
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := checks(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking checks = %v, want %v\n%s", got, tt.want, res.stdout)
			}
			if res.report != nil {
				if res.report.Gate != "G0" || res.report.Task == "" {
					t.Errorf("report header = %+v", res.report)
				}
				for _, b := range res.report.Blocking {
					if b.Repro != "go run ./gates/cmd/ready specs/CC-500.md --base main" {
						t.Errorf("repro = %q", b.Repro)
					}
				}
			}
			if first, _, _ := strings.Cut(res.stdout, "\n"); !strings.HasPrefix(first, "G0 ready ") {
				t.Errorf("summary line = %q", first)
			}
		})
	}
}

func TestAcceptanceCommand(t *testing.T) {
	tests := map[string]string{
		"go test ./...":                  "go",
		"! go run ./gates/cmd/protected": "go",
		"!go vet":                        "go",
		"FOO=1 BAR= go test":             "go",
		"! A=b ./scripts/x.sh --flag":    "./scripts/x.sh",
		"Debits equal credits":           "Debits",
		"   ":                            "",
		"X=1":                            "",
	}
	for line, want := range tests {
		if got := AcceptanceCommand(line); got != want {
			t.Errorf("AcceptanceCommand(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestInFlightAndDone(t *testing.T) {
	r := newTestRepo(t)
	r.branchWithSpec("cc-777-open", "CC-777", "internal/bar/**")
	r.git("checkout", "-q", "-b", "cc-780-nospec")
	r.write("x.go", "package x\n")
	r.commit("CC-780: no spec")
	r.git("checkout", "-q", "main")
	r.git("checkout", "-q", "-b", "feature-x")
	r.write("y.go", "package y\n")
	r.commit("not a ticket branch")
	r.git("checkout", "-q", "main")
	r.branchWithSpec("cc-779-done", "CC-779", "internal/baz/**")
	r.git("merge", "-q", "--no-ff", "-m", "Merge cc-779-done", "cc-779-done")
	r.write("specs/CC-500.md", validSpec(t))
	r.write("tmp/current-task", "CC-500\n")

	repo := Repo{Dir: r.dir}
	ctx := context.Background()
	done, err := repo.Done(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !done["CC-101"] || !done["CC-779"] || done["CC-777"] || done["CC-780"] || len(done) != 2 {
		t.Errorf("Done = %v, want CC-101 and CC-779", done)
	}

	got, err := repo.InFlight(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ticket := range got {
		ids = append(ids, ticket.ID)
	}
	if want := []string{"CC-500", "CC-777"}; !slices.Equal(ids, want) {
		t.Fatalf("InFlight IDs = %v, want %v", ids, want)
	}
	if !slices.Equal(got[0].Files, []string{"internal/foo/**", "scripts/check-foo.sh"}) {
		t.Errorf("CC-500 files = %v", got[0].Files)
	}
	if !slices.Equal(got[1].Files, []string{"internal/bar/**"}) || !slices.Equal(got[1].Sources, []string{"branch cc-777-open"}) {
		t.Errorf("CC-777 = %+v", got[1])
	}

	if _, err := repo.Done(ctx, "--output=x"); err == nil {
		t.Error("Done accepted a ref that git would read as an option")
	}
}
