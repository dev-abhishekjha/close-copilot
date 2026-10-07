package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a throwaway git repository in t.TempDir(). Tests never run
// git against this repository's real history.
type testRepo struct {
	t   *testing.T
	dir string
}

// newTestRepo creates a repo on main with one commit, "CC-101: skeleton".
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.write("README.md", "test repo\n")
	r.commit("CC-101: skeleton")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	// Isolate from the developer's git config (signing, hooks, templates).
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// commit stages everything except tmp/ and commits.
func (r *testRepo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A", "--", ".", ":!tmp")
	r.git("commit", "-q", "--no-verify", "--allow-empty", "-m", msg)
}

// branchWithSpec commits specs/<id>.md with the given files on a new branch
// and returns to main.
func (r *testRepo) branchWithSpec(branch, id string, files ...string) {
	r.t.Helper()
	r.git("checkout", "-q", "-b", branch)
	r.write("specs/"+id+".md", specWith(r.t, id, files...))
	r.commit(id + ": spec")
	r.git("checkout", "-q", "main")
}

// validSpec is testdata/CC-500.md.
func validSpec(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/CC-500.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// specWith is the valid spec with another ID and files list.
func specWith(t *testing.T, id string, files ...string) string {
	t.Helper()
	s := strings.Replace(validSpec(t), "id: CC-500", "id: "+id, 1)
	return strings.Replace(s, "  - internal/foo/**\n  - scripts/check-foo.sh\n", "  - "+strings.Join(files, "\n  - ")+"\n", 1)
}

// fakeLookPath knows only go and git, whatever the machine has installed.
func fakeLookPath(name string) (string, error) {
	switch name {
	case "go", "git":
		return "/usr/local/bin/" + name, nil
	}
	return "", exec.ErrNotFound
}

type run struct {
	code   int
	stdout string
	stderr string
	report *Report
}

// runCmd runs a gate command in the repo and decodes the report it prints
// on failure.
func (r *testRepo) runCmd(fn func(context.Context, Env, []string) int, env map[string]string, args ...string) run {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	e := Env{
		Dir: r.dir, Stdout: &stdout, Stderr: &stderr,
		Getenv:   func(k string) string { return env[k] },
		LookPath: fakeLookPath,
	}
	res := run{code: fn(context.Background(), e, args), stdout: stdout.String(), stderr: stderr.String()}
	if res.code == ExitFail {
		// The first line is the summary; the report JSON follows.
		_, js, _ := strings.Cut(res.stdout, "\n")
		var rep Report
		if err := json.Unmarshal([]byte(js), &rep); err != nil {
			r.t.Fatalf("failure output has no report JSON: %v\n%s", err, res.stdout)
		}
		res.report = &rep
	}
	return res
}

func checks(rep *Report) []string {
	if rep == nil {
		return nil
	}
	var out []string
	for _, b := range rep.Blocking {
		out = append(out, b.Check)
	}
	return out
}

func evidence(rep *Report) []string {
	if rep == nil {
		return nil
	}
	var out []string
	for _, b := range rep.Blocking {
		out = append(out, b.Evidence)
	}
	return out
}
