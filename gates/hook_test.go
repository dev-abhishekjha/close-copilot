package gates

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookEnv is a main checkout with a ticket worktree under .worktrees/, as
// /build lays them out, for exercising .claude/hooks/guard-edit.sh.
type hookEnv struct {
	t        *testing.T
	hook     string
	main     *testRepo
	worktree string
	outside  string // a directory in no git checkout
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	hook, err := filepath.Abs("../.claude/hooks/guard-edit.sh")
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRepo(t)
	r.write("specs/CC-500.md", specWith(t, "CC-500", "internal/foo/**", "*.go"))
	r.write(".gitignore", ".worktrees/\n")
	r.commit("CC-500: spec")
	r.git("worktree", "add", "-q", "-b", "cc-501-wt", ".worktrees/wt")
	wt := filepath.Join(r.dir, ".worktrees", "wt")
	writeFile(t, filepath.Join(wt, "specs", "CC-501.md"), specWith(t, "CC-501", "internal/bar/**"))
	writeFile(t, filepath.Join(wt, "tmp", "current-task"), "CC-501\n")
	return &hookEnv{t: t, hook: hook, main: r, worktree: wt, outside: t.TempDir()}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setMainTask writes or removes the main checkout's tmp/current-task.
func (h *hookEnv) setMainTask(id string) {
	h.t.Helper()
	p := filepath.Join(h.main.dir, "tmp", "current-task")
	if id == "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.t.Fatal(err)
		}
		return
	}
	writeFile(h.t, p, id+"\n")
}

// run feeds the hook an Edit of path as role and returns its exit code and
// stderr.
func (h *hookEnv) run(role, path string) (int, string) {
	h.t.Helper()
	input, err := json.Marshal(map[string]any{
		"tool_name":  "Edit",
		"tool_input": map[string]string{"file_path": path, "old_string": "a", "new_string": "b"},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.runInput(role, input)
}

// runInput feeds the hook raw stdin as role, with extra environment
// variables (later ones win), and returns its exit code and stderr.
func (h *hookEnv) runInput(role string, input []byte, env ...string) (int, string) {
	h.t.Helper()
	args := []string{h.hook}
	if role != "" {
		args = append(args, role)
	}
	cmd := exec.Command("bash", args...)
	cmd.Dir = h.main.dir
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(),
		"CLAUDE_PROJECT_DIR="+h.main.dir,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	default:
		h.t.Fatalf("run hook: %v", err)
		return 0, ""
	}
}

// runJSON feeds the hook the JSON encoding of input as role.
func (h *hookEnv) runJSON(role string, input any) (int, string) {
	h.t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.runInput(role, b)
}

// wantExit fails the test unless the hook exited 2 (block) or 0, with msg
// in stderr when given.
func wantExit(t *testing.T, code int, stderr string, block bool, msg string) {
	t.Helper()
	want := 0
	if block {
		want = 2
	}
	if code != want {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, want, stderr)
	}
	if msg != "" && !strings.Contains(stderr, msg) {
		t.Errorf("stderr lacks %q: %s", msg, stderr)
	}
}

// TestGuardHook runs .claude/hooks/guard-edit.sh in a temporary main
// checkout and a worktree of it, each with its own tmp/current-task
// (finding 7).
func TestGuardHook(t *testing.T) {
	h := newHookEnv(t)
	m, wt := h.main.dir, h.worktree
	base := filepath.Base(m)

	// A directory symlink and a file symlink into the seeder.
	if err := os.MkdirAll(filepath.Join(m, "internal", "seed"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(m, "internal", "seed"), filepath.Join(m, "seedlink")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(m, "internal", "seed", "s.go"), "package seed\n")
	if err := os.Symlink(filepath.Join("internal", "seed", "s.go"), filepath.Join(m, "notes.go")); err != nil {
		t.Fatal(err)
	}

	type hookCase struct {
		name     string
		mainTask string // main checkout's tmp/current-task; the worktree always runs CC-501
		role     string
		path     string
		block    bool
		msg      string // substring of the block message
	}
	cases := []hookCase{
		// Normalisation: ".." can't walk out of the tmp/ allowance.
		{name: "tmp/.. back into the checkout is blocked", mainTask: "CC-500", path: m + "/tmp/../internal/bar/x.go", block: true, msg: "internal/bar/x.go is not in CC-500"},
		{name: "tmp/../../<checkout> is blocked", mainTask: "CC-500", path: m + "/tmp/../../" + base + "/internal/bar/x.go", block: true, msg: "internal/bar/x.go is not in CC-500"},
		{name: "worktree tmp/../../x lands in the main checkout and is blocked", mainTask: "CC-500", path: wt + "/tmp/../../x", block: true, msg: ".worktrees/x is not in CC-500"},
		{name: "./ segments are dropped", mainTask: "CC-500", path: m + "/./internal/./foo/x.go"},
		{name: "a real tmp/ file is allowed", mainTask: "CC-500", path: m + "/tmp/notes.txt"},
		{name: "a relative path resolves against the project", mainTask: "CC-500", path: "internal/bar/x.go", block: true, msg: "not in CC-500"},

		// Outside every checkout: allowed, task or not, role or not.
		{name: "a path outside the root is allowed", mainTask: "CC-500", path: filepath.Join(h.outside, "memory", "MEMORY.md")},
		{name: "a .. out of the root is allowed", mainTask: "CC-500", path: m + "/../outside-" + base + ".md"},
		{name: "outside the root, implementer", mainTask: "CC-500", role: "implementer", path: filepath.Join(h.outside, ".claude", "x.md")},
		{name: "outside the root, security-reviewer", role: "security-reviewer", path: filepath.Join(h.outside, "notes.md")},

		// Declared files.
		{name: "a declared file is allowed", mainTask: "CC-500", path: m + "/internal/foo/deep/x.go"},
		{name: "an undeclared file is blocked", mainTask: "CC-500", path: m + "/internal/bar/x.go", block: true, msg: "not in CC-500"},
		{name: "*.go matches b.go", mainTask: "CC-500", path: m + "/b.go"},
		{name: "*.go does not match a/b.go", mainTask: "CC-500", path: m + "/a/b.go", block: true, msg: "a/b.go is not in CC-500"},
		{name: "the ticket's own spec is allowed", mainTask: "CC-500", path: m + "/specs/CC-500.md"},
		{name: "edits inside .git are blocked", path: m + "/.git/hooks/pre-commit", block: true, msg: ".git"},
		{name: "edits inside .git are blocked with a task", mainTask: "CC-500", path: m + "/.git/config", block: true, msg: ".git"},

		// Worktrees enforce their own task, and never the main checkout's.
		{name: "worktree: its declared file is allowed", mainTask: "CC-500", path: wt + "/internal/bar/x.go"},
		{name: "worktree: the main task's file is blocked", mainTask: "CC-500", path: wt + "/internal/foo/x.go", block: true, msg: "not in CC-501"},
		{name: "worktree task does not reach main (no main task)", path: m + "/internal/qux/x.go"},
		{name: "worktree task does not reach main (main task)", mainTask: "CC-500", path: m + "/internal/bar/x.go", block: true, msg: "not in CC-500"},
		{name: "worktree without a main task still enforces its own", path: wt + "/internal/qux/x.go", block: true, msg: "not in CC-501"},

		// Symlinks are followed.
		{name: "a directory symlink into the seeder is blocked for implementer", role: "implementer", path: m + "/seedlink/x.go", block: true, msg: "implementer never edits"},
		{name: "a file symlink into the seeder is blocked for implementer", role: "implementer", path: m + "/notes.go", block: true, msg: "implementer never edits"},
		{name: "a case variant of the seeder is blocked for implementer", role: "implementer", path: m + "/INTERNAL/Seed/x.go", block: true, msg: "implementer never edits"},
		{name: "implementer may edit its own code", role: "implementer", path: m + "/internal/money/x.go"},

		// Non-ASCII paths: APFS folds more than ASCII case, so they are
		// refused before any role or declared-file match.
		{name: "evals/baſeline.json is blocked for implementer", role: "implementer", path: m + "/evals/ba\u017feline.json", block: true, msg: "non-ASCII"},
		{name: "evals/baſeline.json is blocked for eval-engineer", role: "eval-engineer", path: m + "/evals/ba\u017feline.json", block: true, msg: "non-ASCII"},
		{name: "evals/baſeline.json is blocked for llm-engineer in the worktree", role: "llm-engineer", path: wt + "/evals/ba\u017feline.json", block: true, msg: "non-ASCII"},
		{name: "internal/ſeed/x.go is blocked for implementer", role: "implementer", path: m + "/internal/\u017feed/x.go", block: true}, // APFS may resolve it to internal/seed first; blocked either way
		{name: "a not-yet-existing aliased directory is blocked", role: "implementer", path: wt + "/internal/eval\u017f/x.go", block: true, msg: "non-ASCII"},
		{name: "a zero-width space is blocked", path: m + "/internal/money/x\u200b.go", block: true, msg: "non-ASCII"},
		{name: "a tab is blocked", path: m + "/internal/money/x\t.go", block: true, msg: "non-ASCII"},
		{name: "a full-width path is blocked", path: m + "/\uff44\uff4f\uff43\uff53/x.md", block: true, msg: "non-ASCII"},

		// .git as any segment: a nested .git file would redirect git.
		{name: "a nested .git file is blocked", path: m + "/internal/.git", block: true, msg: "inside a .git"},
		{name: "a file inside a nested .git is blocked", path: m + "/internal/x/.git/config", block: true, msg: "inside a .git"},
		{name: "a file inside a nested .git in the worktree is blocked", path: wt + "/internal/x/.git/config", block: true, msg: "inside a .git"},
		{name: "a .GIT case variant is blocked", path: m + "/internal/.GIT", block: true, msg: ".git"},
		{name: ".gitignore is not .git", path: m + "/.gitignore"},

		// The build state belongs to the orchestrator.
		{name: "implementer can't rewrite tmp/current-task", mainTask: "CC-500", role: "implementer", path: m + "/tmp/current-task", block: true, msg: "orchestrator"},
		{name: "implementer can't rewrite the worktree's tmp/current-task", role: "implementer", path: wt + "/tmp/current-task", block: true, msg: "orchestrator"},
		{name: "a case variant of tmp/current-task is blocked", role: "eval-engineer", path: wt + "/TMP/Current-Task", block: true, msg: "orchestrator"},
		{name: "llm-engineer can't touch tmp/erpnext.lock", role: "llm-engineer", path: m + "/tmp/erpnext.lock", block: true, msg: "orchestrator"},
		{name: "a role may still write other tmp/ files", mainTask: "CC-500", role: "implementer", path: m + "/tmp/notes.txt"},
		{name: "without a role tmp/current-task is allowed", mainTask: "CC-500", path: m + "/tmp/current-task"},
	}

	forbidden := map[string][]string{
		"domain-data-engineer": {"internal/checks/x.go", "internal/agent/x.go", "internal/evals/x.go"},
		"eval-engineer":        {"internal/checks/x.go", "internal/agent/x.go", "internal/retrieval/x.go", "evals/baseline.json"},
		"llm-engineer":         {"internal/evals/x.go", "evals/golden/x.json", "evals/baseline.json", "evals/scenarios/x.yaml"},
		"implementer":          {"internal/seed/x.go", "evals/scenarios/x.yaml", "evals/baseline.json"},
		"security-reviewer":    {"internal/foo/x.go", "README.md", "tmp/notes.txt", "specs/CC-500.md"},
	}
	for role, paths := range forbidden {
		for _, p := range paths {
			for _, checkout := range []struct{ name, dir string }{{"main", m}, {"worktree", wt}} {
				cases = append(cases, hookCase{
					name: role + " in " + checkout.name + ": " + p, role: role,
					path: checkout.dir + "/" + p, block: true, msg: role,
				})
			}
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.setMainTask(tc.mainTask)
			code, stderr := h.run(tc.role, tc.path)
			want := 0
			if tc.block {
				want = 2
			}
			if code != want {
				t.Fatalf("exit %d, want %d for %s\nstderr: %s", code, want, tc.path, stderr)
			}
			if tc.msg != "" && !strings.Contains(stderr, tc.msg) {
				t.Errorf("stderr lacks %q: %s", tc.msg, stderr)
			}
		})
	}
}

// TestGuardHookIgnoresNonEdits: input without a file path is not judged.
func TestGuardHookIgnoresNonEdits(t *testing.T) {
	h := newHookEnv(t)
	h.setMainTask("CC-500")
	if code, stderr := h.run("security-reviewer", ""); code != 0 {
		t.Errorf("exit %d for an empty path: %s", code, stderr)
	}
}

// TestGuardHookFailsClosed: Claude Code lets an edit through on any exit
// but 2 and on a timeout, so every failure mode must exit 2, quickly.
func TestGuardHookFailsClosed(t *testing.T) {
	h := newHookEnv(t)
	m := h.main.dir

	t.Run("malformed stdin", func(t *testing.T) {
		h.setMainTask("")
		for _, in := range []string{`{"tool_input": {"file_path": "x"`, "not json", `{"tool_input": [}`} {
			if code, stderr := h.runInput("", []byte(in)); code != 2 {
				t.Errorf("exit %d for stdin %q, want 2: %s", code, in, stderr)
			}
		}
	})

	t.Run("unreadable task file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		h.setMainTask("CC-500")
		p := filepath.Join(m, "tmp", "current-task")
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600); h.setMainTask("") })
		code, stderr := h.run("", m+"/internal/foo/x.go")
		if code != 2 || !strings.Contains(stderr, "current-task") {
			t.Errorf("exit %d, want 2 naming tmp/current-task: %s", code, stderr)
		}
	})

	h.setMainTask("CC-500")
	long := m + "/tmp/" + strings.Repeat("zz/../", 5000) + "x" // ~30 KB
	cases := []struct {
		name, path, msg string
	}{
		{"a 30 KB path of zz/.. segments", long, "4096 bytes"},
		{"more than 256 segments", m + "/" + strings.Repeat("a/", 300) + "x", "256 segments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			code, stderr := h.run("", tc.path)
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("took %v; the hook's timeout is 10 s", d)
			}
			if code != 2 || !strings.Contains(stderr, tc.msg) {
				t.Errorf("exit %d, want 2 with %q: %s", code, tc.msg, stderr)
			}
		})
	}

	t.Run("just under 256 segments of .. resolves well within the timeout", func(t *testing.T) {
		p := m + "/tmp/" + strings.Repeat("zz/../", 115) + "notes.txt" // ~240 segments
		start := time.Now()
		code, stderr := h.run("", p)
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("took %v; the hook's timeout is 10 s", d)
		}
		if code != 0 {
			t.Errorf("exit %d, want 0 for tmp/notes.txt: %s", code, stderr)
		}
	})
}

// TestGuardHookWorktreeFallback: a bogus .git file under a worktree makes
// git fail there; the path must still be judged against that worktree, not
// as .worktrees/<wt>/... in the main checkout.
func TestGuardHookWorktreeFallback(t *testing.T) {
	h := newHookEnv(t)
	wt := h.worktree
	writeFile(t, filepath.Join(wt, "internal", ".git"), "gitdir: /nonexistent/close-copilot\n")
	if err := os.MkdirAll(filepath.Join(wt, "internal", "seed"), 0o750); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, role, path, msg string
		block                 bool
	}{
		{name: "implementer below the bogus .git is still blocked from the seeder", role: "implementer", path: wt + "/internal/seed/a.go", block: true, msg: "implementer never edits"},
		{name: "the worktree's task still applies", path: wt + "/internal/qux/x.go", block: true, msg: "internal/qux/x.go is not in CC-501"},
		{name: "the worktree's declared file is still allowed", path: wt + "/internal/bar/x.go"},
		{name: "the bogus .git itself is blocked", path: wt + "/internal/.git", block: true, msg: ".git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := h.run(tc.role, tc.path)
			want := 0
			if tc.block {
				want = 2
			}
			if code != want {
				t.Fatalf("exit %d, want %d for %s\nstderr: %s", code, want, tc.path, stderr)
			}
			if tc.msg != "" && !strings.Contains(stderr, tc.msg) {
				t.Errorf("stderr lacks %q: %s", tc.msg, stderr)
			}
		})
	}
}

// TestGuardHookCommittedSpec: the declared files come from the spec at
// HEAD, so widening the working copy's files: list widens nothing.
func TestGuardHookCommittedSpec(t *testing.T) {
	h := newHookEnv(t)
	m := h.main.dir
	h.setMainTask("CC-500")
	writeFile(t, filepath.Join(m, "specs", "CC-500.md"), specWith(t, "CC-500", "**"))

	if code, stderr := h.run("implementer", m+"/internal/bar/x.go"); code != 2 || !strings.Contains(stderr, "not in CC-500") {
		t.Errorf("a working-copy files: \"**\" widened the check: exit %d: %s", code, stderr)
	}
	if code, stderr := h.run("implementer", m+"/internal/foo/x.go"); code != 0 {
		t.Errorf("the committed glob no longer applies: exit %d: %s", code, stderr)
	}
	// Before the spec commit (the worktree's CC-501 is uncommitted), the
	// working copy is used.
	if code, stderr := h.run("", h.worktree+"/internal/bar/x.go"); code != 0 {
		t.Errorf("an uncommitted spec is not read: exit %d: %s", code, stderr)
	}
}

// TestGuardHookDotDotAfterSymlink: Claude Code normalises ".." lexically
// before writing, but the OS reads ".." after a symlink as the target's
// parent. The hook judges the lexical path and refuses a path where the two
// readings disagree (on macOS /tmp, /var and /etc are symlinks into
// /private).
func TestGuardHookDotDotAfterSymlink(t *testing.T) {
	h := newHookEnv(t)
	m := h.main.dir
	deep := filepath.Join(m, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deep, filepath.Join(m, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(deep, filepath.Join(h.outside, "olink")); err != nil {
		t.Fatal(err)
	}
	// From h.outside/olink, enough ".." to reach / lexically, then m: the
	// OS lands three levels above a/b/c's parent chain instead.
	ups := strings.Repeat("../", strings.Count(filepath.Clean(h.outside), "/")+1)

	cases := []struct {
		name, role, path, msg string
		block                 bool
	}{
		{name: "implementer on /var/..<root>/evals/baseline.json", role: "implementer", path: "/var/.." + m + "/evals/baseline.json", block: true, msg: "evals/baseline.json"},
		{name: "implementer on linkdir/../internal/seed/x.go", role: "implementer", path: m + "/linkdir/../internal/seed/x.go", block: true, msg: "ambiguous"},
		{name: "no role on linkdir/../x is ambiguous too", path: m + "/linkdir/../x.go", block: true, msg: "ambiguous"},
		{name: "security-reviewer on .git/hooks/pre-commit through /tmp/..", role: "security-reviewer", path: "/tmp/.." + m + "/.git/hooks/pre-commit", block: true},
		{name: "security-reviewer through /etc/..", role: "security-reviewer", path: "/etc/.." + m + "/README.md", block: true},
		{name: "an outside symlink whose .. lands in the root lexically", role: "implementer", path: h.outside + "/olink/" + ups + strings.TrimPrefix(m, "/") + "/internal/seed/x.go", block: true},
		{name: "an outside symlink whose .. lands in the root physically", role: "security-reviewer", path: h.outside + "/olink/../../../README.md", block: true, msg: "ambiguous"},
		{name: "a symlink then a plain .. that both readings agree on", role: "implementer", path: m + "/linkdir/x/../y.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := h.run(tc.role, tc.path)
			wantExit(t, code, stderr, tc.block, tc.msg)
		})
	}
}

// TestGuardHookInput covers how the path is read from the tool input.
func TestGuardHookInput(t *testing.T) {
	h := newHookEnv(t)
	m, wt := h.main.dir, h.worktree
	h.setMainTask("CC-500")
	edit := func(in map[string]any) map[string]any {
		return map[string]any{"tool_name": "Edit", "tool_input": in}
	}

	cases := []struct {
		name, role, msg string
		input           any
		block           bool
	}{
		{name: "an empty file_path falls through to notebook_path", role: "implementer", input: edit(map[string]any{"file_path": "", "notebook_path": m + "/internal/seed/n.ipynb"}), block: true, msg: "implementer never edits"},
		{name: "a null file_path falls through to notebook_path", role: "implementer", input: edit(map[string]any{"file_path": nil, "notebook_path": m + "/internal/seed/n.ipynb"}), block: true, msg: "implementer never edits"},
		{name: "a numeric file_path is refused", input: edit(map[string]any{"file_path": 5}), block: true, msg: "non-string"},
		{name: "an array file_path is refused", input: edit(map[string]any{"file_path": []string{m + "/internal/seed/x.go"}}), block: true, msg: "non-string"},
		{name: "an object notebook_path is refused", input: edit(map[string]any{"file_path": m + "/internal/foo/x.go", "notebook_path": map[string]string{"a": "b"}}), block: true, msg: "non-string"},
		{name: "a trailing newline is refused", input: edit(map[string]any{"file_path": m + "/internal/foo/x.go\n"}), block: true, msg: "line break"},
		{name: "a NUL is refused", input: edit(map[string]any{"file_path": m + "/internal/se\x00ed/x.go"}), block: true, msg: "NUL"},
		{name: "both empty is not an edit", role: "security-reviewer", input: edit(map[string]any{"file_path": "", "notebook_path": ""})},

		// Relative paths resolve against the session's cwd.
		{name: "relative path from a worktree cwd: its undeclared file is blocked", input: map[string]any{"cwd": wt, "tool_input": map[string]any{"file_path": "internal/qux/x.go"}}, block: true, msg: "internal/qux/x.go is not in CC-501"},
		{name: "relative path from a worktree cwd: its declared file is allowed", input: map[string]any{"cwd": wt, "tool_input": map[string]any{"file_path": "internal/bar/x.go"}}},
		{name: "relative path from a worktree cwd: role checks apply", role: "implementer", input: map[string]any{"cwd": wt, "tool_input": map[string]any{"file_path": "internal/seed/x.go"}}, block: true, msg: "implementer never edits"},
		{name: "relative path without cwd resolves against the project", input: edit(map[string]any{"file_path": "internal/bar/x.go"}), block: true, msg: "not in CC-500"},
		{name: "a relative cwd is ignored", input: map[string]any{"cwd": "relative/dir", "tool_input": map[string]any{"file_path": "internal/bar/x.go"}}, block: true, msg: "internal/bar/x.go is not in CC-500"},
		{name: "a non-string cwd is ignored", input: map[string]any{"cwd": 7, "tool_input": map[string]any{"file_path": "internal/foo/x.go"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := h.runJSON(tc.role, tc.input)
			wantExit(t, code, stderr, tc.block, tc.msg)
		})
	}

	t.Run("jq off PATH blocks", func(t *testing.T) {
		bin := t.TempDir()
		cat, err := exec.LookPath("cat")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cat, filepath.Join(bin, "cat")); err != nil {
			t.Fatal(err)
		}
		input, err := json.Marshal(edit(map[string]any{"file_path": h.outside + "/x.md"}))
		if err != nil {
			t.Fatal(err)
		}
		code, stderr := h.runInput("", input, "PATH="+bin)
		wantExit(t, code, stderr, true, "jq is required")
	})
}

// TestGuardHookTaskFileType: a tmp/current-task that exists but isn't a
// regular file must not switch the declared-files check off.
func TestGuardHookTaskFileType(t *testing.T) {
	h := newHookEnv(t)
	m := h.main.dir
	p := filepath.Join(m, "tmp", "current-task")

	t.Run("a directory", func(t *testing.T) {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(p) })
		code, stderr := h.run("", m+"/internal/bar/x.go")
		wantExit(t, code, stderr, true, "not a regular file")
	})
	t.Run("a dangling symlink", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(m, "nowhere"), p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(p) })
		code, stderr := h.run("", m+"/internal/bar/x.go")
		wantExit(t, code, stderr, true, "not a regular file")
	})
	t.Run("absent", func(t *testing.T) {
		code, stderr := h.run("", m+"/internal/bar/x.go")
		wantExit(t, code, stderr, false, "")
	})
}

// TestGuardHookStaleWorktreeDir: a directory under .worktrees/ that git
// doesn't know as a worktree would be judged as the main checkout's
// .worktrees/<name>/..., where no role pattern matches; it is refused.
func TestGuardHookStaleWorktreeDir(t *testing.T) {
	h := newHookEnv(t)
	m := h.main.dir
	if err := os.MkdirAll(filepath.Join(m, ".worktrees", "stale", "internal", "seed"), 0o750); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(m, ".worktrees", "other")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", other)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	cases := []struct {
		name, role, path, msg string
		block                 bool
	}{
		{name: "implementer in a stale dir", role: "implementer", path: m + "/.worktrees/stale/internal/seed/x.go", block: true, msg: "not in a registered worktree"},
		{name: "no role in a stale dir", path: m + "/.worktrees/stale/README.md", block: true, msg: "not in a registered worktree"},
		{name: "a not-yet-existing dir under .worktrees", role: "implementer", path: m + "/.worktrees/new/internal/seed/x.go", block: true, msg: "not in a registered worktree"},
		{name: "a foreign repository under .worktrees", role: "implementer", path: other + "/internal/seed/x.go", block: true, msg: "not in a registered worktree"},
		{name: "a case variant of .worktrees", role: "implementer", path: m + "/.WORKTREES/stale/internal/seed/x.go", block: true},
		{name: "the registered worktree is still its own root", path: h.worktree + "/internal/bar/x.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := h.run(tc.role, tc.path)
			wantExit(t, code, stderr, tc.block, tc.msg)
		})
	}
}
