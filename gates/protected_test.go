package gates

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func (r *testRepo) head() string {
	r.t.Helper()
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

// TestProtectedApprovedSHA: an approval covers exactly the head commit it
// was given for (finding 2).
func TestProtectedApprovedSHA(t *testing.T) {
	editBaseline := func(r *testRepo) {
		r.write("evals/baseline.json", "{}\n")
		r.commit("CC-500: baseline")
	}
	tests := []struct {
		name string
		// setup changes the repo and returns the --approved-sha value.
		setup    func(r *testRepo) string
		labels   string
		wantCode int
		want     []string // evidence
		message  string   // substring of the first blocking message
	}{
		{
			name:     "approved at the head passes",
			setup:    func(r *testRepo) string { editBaseline(r); return r.head() },
			labels:   "approved",
			wantCode: ExitPass,
		},
		{
			name: "a commit pushed after the approval fails",
			setup: func(r *testRepo) string {
				editBaseline(r)
				approved := r.head()
				r.write("evals/baseline.json", `{"recall": 0}`+"\n")
				r.commit("CC-500: lower the bar after review")
				return approved
			},
			labels:   "approved",
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
			message: "pushed after the approval",
		},
		{
			name: "an unprotected commit after the approval still fails",
			setup: func(r *testRepo) string {
				editBaseline(r)
				approved := r.head()
				r.write("internal/foo/more.go", "package foo\n")
				r.commit("CC-500: more")
				return approved
			},
			labels:   "approved",
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
		},
		{
			name:     "no approval fails",
			setup:    func(r *testRepo) string { editBaseline(r); return "" },
			labels:   "approved",
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
			message: "no approved commit",
		},
		{
			name:     "an approved SHA without the label fails",
			setup:    func(r *testRepo) string { editBaseline(r); return r.head() },
			wantCode: ExitFail, want: []string{"evals/baseline.json"},
			message: "label is absent",
		},
		{
			name: "an uncommitted protected edit is not covered by the approval",
			setup: func(r *testRepo) string {
				editBaseline(r)
				approved := r.head()
				r.write("evals/baseline.json", `{"recall": 0}`+"\n")
				r.write("gates/new.go", "package gates\n")
				return approved
			},
			labels:   "approved",
			wantCode: ExitFail, want: []string{"evals/baseline.json", "gates/new.go"},
			message: "not committed",
		},
		{
			name:     "no protected change needs no approval",
			setup:    func(*testRepo) string { return "" },
			wantCode: ExitPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := onTicketBranch(t)
			sha := tt.setup(r)
			args := []string{"--base", "main", "--approved-sha", sha}
			if tt.labels != "" {
				args = append(args, "--labels", tt.labels)
			}
			res := r.runCmd(RunProtected, nil, args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if got := evidence(res.report); !slices.Equal(got, tt.want) {
				t.Errorf("blocking evidence = %v, want %v", got, tt.want)
			}
			if tt.message != "" && (res.report == nil || !strings.Contains(res.report.Blocking[0].Message, tt.message)) {
				t.Errorf("first message lacks %q:\n%s", tt.message, res.stdout)
			}
		})
	}
}

// TestProtectedUnicode: no case or Unicode variant of a protected path gets
// past the matcher (finding 3). Non-ASCII paths fail even when approved;
// ASCII case variants are protected paths like any other.
func TestProtectedUnicode(t *testing.T) {
	tests := []struct {
		path     string
		nonASCII bool
	}{
		{"docſ/x", true},           // U+017F LATIN SMALL LETTER LONG S folds to s
		{"ｄｏｃｓ/x", true},           // full-width letters, NFKC docs
		{"docs/ｘ.md", true},        // a full-width letter under a protected dir
		{"gates/x\u200b.go", true}, // zero-width space
		{"DOCS/x", false},
		{"Gates/x.go", false},
		{".CLAUDE/x", false},
		{".GolangCI.yml", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			changed := []string{"internal/foo/foo.go", tt.path}

			// Unit level: unapproved, every variant is caught.
			probs, err := ProtectedProblems(changed, Approval{})
			if err != nil {
				t.Fatal(err)
			}
			if len(probs) == 0 || probs[0].Evidence != tt.path || probs[0].Check != "protected" {
				t.Fatalf("unapproved problems = %+v, want one naming %q", probs, tt.path)
			}

			// Approved at the head: only the non-ASCII paths still fail.
			approved := Approval{Labels: []string{ApprovedLabel}, SHA: strings.Repeat("a", 40), Head: strings.Repeat("a", 40)}
			probs, err = ProtectedProblems(changed, approved)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(probs) > 0; got != tt.nonASCII {
				t.Errorf("approved problems = %+v, want blocked=%v", probs, tt.nonASCII)
			}

			// End to end in a git repository.
			r := onTicketBranch(t)
			r.write(tt.path, "x\n")
			r.commit("CC-500: variant")
			res := r.runCmd(RunProtected, nil, "--base", "main")
			if res.code != ExitFail {
				t.Fatalf("exit %d, want %d\n%s%s", res.code, ExitFail, res.stdout, res.stderr)
			}
			if got := evidence(res.report); !slices.Contains(got, tt.path) {
				t.Errorf("blocking evidence = %v, want %q", got, tt.path)
			}
		})
	}
}

func TestProtectedNonASCIIPaths(t *testing.T) {
	got := NonASCIIPaths([]string{"a.go", "ok dir/b.go", "tab\tx", "é.go", "~ok"})
	if want := []string{"tab\tx", "é.go"}; !slices.Equal(got, want) {
		t.Errorf("NonASCIIPaths = %q, want %q", got, want)
	}
}

// TestPinnedSpec: the pinned spec is the one on base, else the one in the
// branch's first "<ID>: spec" commit, never the branch tip (finding 4).
func TestPinnedSpec(t *testing.T) {
	ctx := context.Background()
	orig := validSpec(t)
	edited := strings.Replace(orig, "risk: regulated", "risk: standard", 1)
	tests := []struct {
		name     string
		setup    func(r *testRepo)
		want     string // pinned content; "" when not found
		wantFrom string // "spec" means the first spec commit's short hash
	}{
		{
			name: "spec on main",
			setup: func(r *testRepo) {
				r.write("specs/CC-500.md", orig)
				r.commit("CC-101: specs")
				r.git("checkout", "-q", "-b", "cc-500-foo")
				r.write("specs/CC-500.md", edited)
				r.commit("CC-500: spec")
			},
			want: orig, wantFrom: "main:specs/CC-500.md",
		},
		{
			name: "spec only on the branch, edited after its spec commit",
			setup: func(r *testRepo) {
				r.git("checkout", "-q", "-b", "cc-500-foo")
				r.write("specs/CC-500.md", orig)
				r.commit("CC-500: spec")
				r.write("specs/CC-500.md", edited)
				r.commit("CC-500: widen")
				// A second "spec" commit does not move the pin.
				r.write("specs/CC-500.md", edited+"\nmore\n")
				r.commit("CC-500: spec")
			},
			want: orig, wantFrom: "spec",
		},
		{
			name: "unchanged spec on the branch",
			setup: func(r *testRepo) {
				r.git("checkout", "-q", "-b", "cc-500-foo")
				r.write("specs/CC-500.md", orig)
				r.commit("CC-500: spec")
				r.write("internal/foo/foo.go", "package foo\n")
				r.commit("CC-500: foo")
			},
			want: orig, wantFrom: "spec",
		},
		{
			name: "no spec commit",
			setup: func(r *testRepo) {
				r.git("checkout", "-q", "-b", "cc-500-foo")
				r.write("specs/CC-500.md", orig)
				r.commit("CC-500: foo and its spec")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRepo(t)
			tt.setup(r)
			repo := Repo{Dir: r.dir}
			commits, err := repo.BranchCommits(ctx, "main")
			if err != nil {
				t.Fatal(err)
			}
			data, from, found, err := repo.PinnedSpec(ctx, "main", "CC-500", commits)
			if err != nil {
				t.Fatal(err)
			}
			if found != (tt.want != "") || string(data) != tt.want {
				t.Fatalf("found %v, pinned spec:\n%s\nwant:\n%s", found, data, tt.want)
			}
			wantFrom := tt.wantFrom
			if wantFrom == "spec" {
				for _, c := range commits {
					if c.Subject == "CC-500: spec" {
						wantFrom = shortHash(c.Hash) + ":specs/CC-500.md"
						break
					}
				}
			}
			if from != wantFrom {
				t.Errorf("from = %q, want %q", from, wantFrom)
			}
		})
	}
}

// TestSpecPinEveryField: every front-matter field is pinned and reported by
// name (finding 4).
func TestSpecPinEveryField(t *testing.T) {
	base, err := ParseSpec([]byte(validSpec(t)))
	if err != nil {
		t.Fatal(err)
	}
	one, two := 1, 2
	tests := []struct {
		field  string
		mutate func(*Spec)
	}{
		{"id", func(s *Spec) { s.ID = "CC-501" }},
		{"title", func(s *Spec) { s.Title = "Something else" }},
		{"phase", func(s *Spec) { s.Phase = &two }},
		{"owner_role", func(s *Spec) { s.OwnerRole = "llm-engineer" }},
		{"risk", func(s *Spec) { s.Risk = RiskStandard }},
		{"approved_by", func(s *Spec) { s.ApprovedBy = "" }},
		{"depends_on", func(s *Spec) { s.DependsOn = nil }},
		{"needs_erpnext", func(s *Spec) { s.NeedsERPNext = true }},
		{"files", func(s *Spec) { s.Files = append(s.Files, "**") }},
		{"consumes", func(s *Spec) { s.Consumes = []string{"x"} }},
		{"produces", func(s *Spec) { s.Produces = []string{"y"} }},
		{"acceptance", func(s *Spec) { s.Acceptance = s.Acceptance[:1] }},
		{"gates", func(s *Spec) { s.Gates = []string{"G1"} }},
		{"budget", func(s *Spec) { s.Budget = &Budget{MaxAttempts: 9, MaxWallMinutes: 60} }},
		{"budget", func(s *Spec) { s.Budget = nil }},
		{"phase", func(s *Spec) { s.Phase = nil }},
	}
	clone := func(s Spec) Spec {
		c := s
		c.DependsOn = slices.Clone(s.DependsOn)
		c.Files = slices.Clone(s.Files)
		c.Consumes = slices.Clone(s.Consumes)
		c.Produces = slices.Clone(s.Produces)
		c.Acceptance = slices.Clone(s.Acceptance)
		c.Gates = slices.Clone(s.Gates)
		return c
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			cur := clone(base)
			tt.mutate(&cur)
			var got []string
			for _, p := range SpecPinProblems(cur, base, "pin") {
				got = append(got, p.Evidence)
				if p.Check != "spec_pin" || !strings.Contains(p.Message, tt.field) {
					t.Errorf("problem = %+v", p)
				}
			}
			if want := []string{"pin#" + tt.field}; !slices.Equal(got, want) {
				t.Errorf("evidence = %v, want %v", got, want)
			}
		})
	}

	t.Run("unchanged, reordered sets and nil versus empty lists pass", func(t *testing.T) {
		cur := clone(base)
		slices.Reverse(cur.Files)
		slices.Reverse(cur.Gates)
		cur.Phase = &one // same value, another pointer
		cur.Consumes, cur.Produces = nil, []string{}
		if p := SpecPinProblems(cur, base, "pin"); len(p) != 0 {
			t.Errorf("problems = %+v", p)
		}
	})
}

// TestSpecPinOnBranch runs the declared gate on a branch whose spec was
// edited after its spec commit, in fields the old pin ignored.
func TestSpecPinOnBranch(t *testing.T) {
	tests := []struct {
		name      string
		old, new  string
		wantField string
	}{
		{"acceptance weakened", "  - test -f internal/foo/foo.go\n", "", "acceptance"},
		{"gates dropped", "gates: [G1, G2, G3, G5, G6]", "gates: [G1]", "gates"},
		{"budget raised", "max_attempts: 3", "max_attempts: 30", "budget"},
		{"owner changed", "owner_role: implementer", "owner_role: llm-engineer", "owner_role"},
		{"unchanged", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := onTicketBranch(t)
			if tt.old != "" {
				s := validSpec(t)
				if !strings.Contains(s, tt.old) {
					t.Fatalf("fixture lacks %q", tt.old)
				}
				r.write("specs/CC-500.md", strings.Replace(s, tt.old, tt.new, 1))
				r.commit("CC-500: edit spec")
			}
			res := r.runCmd(RunDeclared, nil, "--spec", "specs/CC-500.md", "--base", "main")
			if tt.wantField == "" {
				if res.code != ExitPass {
					t.Fatalf("exit %d, want pass\n%s", res.code, res.stdout)
				}
				return
			}
			if res.code != ExitFail {
				t.Fatalf("exit %d, want %d\n%s%s", res.code, ExitFail, res.stdout, res.stderr)
			}
			ev := evidence(res.report)
			if len(ev) != 1 || !strings.HasSuffix(ev[0], ":specs/CC-500.md#"+tt.wantField) || strings.HasPrefix(ev[0], "main:") {
				t.Errorf("blocking evidence = %v, want the spec commit's #%s", ev, tt.wantField)
			}
		})
	}
}
