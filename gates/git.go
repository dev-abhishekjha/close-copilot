package gates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Repo runs read-only git commands in Dir, the repository root.
type Repo struct {
	Dir string
	Log *slog.Logger
}

// InFlightTicket is a ticket being built: named in tmp/current-task or on an
// unmerged local branch cc-<n>-*. Files come from its spec, when one exists.
type InFlightTicket struct {
	ID      string
	Sources []string
	Files   []string
}

var (
	doneSubject = regexp.MustCompile(`^(CC-[0-9]+[a-z]?):`)
	branchName  = regexp.MustCompile(`^cc-([0-9]+[a-z]?)-.`)
)

// TicketFromBranch derives the ticket ID from a branch name:
// "cc-602-bank-rec" gives "CC-602". It returns "" for other names.
func TicketFromBranch(branch string) string {
	m := branchName.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return "CC-" + m[1]
}

func (r Repo) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary; refs are validated by checkRef
	cmd.Dir = r.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// checkRef rejects a ref git could read as an option.
func checkRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	return nil
}

// Done returns the tickets that have a commit on base whose subject starts
// with "CC-xxx:".
func (r Repo) Done(ctx context.Context, base string) (map[string]bool, error) {
	if err := checkRef(base); err != nil {
		return nil, err
	}
	out, err := r.git(ctx, "log", "--format=%s", base, "--")
	if err != nil {
		return nil, fmt.Errorf("list done tickets: %w", err)
	}
	done := make(map[string]bool)
	for line := range strings.SplitSeq(string(out), "\n") {
		if m := doneSubject.FindStringSubmatch(line); m != nil {
			done[m[1]] = true
		}
	}
	return done, nil
}

// InFlight returns the tickets in progress: the one named in
// tmp/current-task, and every local branch cc-<n>-* not merged into base
// that carries specs/CC-<n>.md. Branches without a spec are skipped.
func (r Repo) InFlight(ctx context.Context, base string) ([]InFlightTicket, error) {
	if err := checkRef(base); err != nil {
		return nil, err
	}
	byID := make(map[string]*InFlightTicket)
	add := func(id, source string, files []string) {
		t, ok := byID[id]
		if !ok {
			t = &InFlightTicket{ID: id}
			byID[id] = t
		}
		t.Sources = append(t.Sources, source)
		for _, f := range files {
			if !slices.Contains(t.Files, f) {
				t.Files = append(t.Files, f)
			}
		}
	}

	current, err := os.ReadFile(filepath.Join(r.Dir, "tmp", "current-task"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read tmp/current-task: %w", err)
	default:
		if id := strings.TrimSpace(string(current)); id != "" {
			var files []string
			specPath := filepath.Join(r.Dir, "specs", id+".md")
			if s, err := LoadSpec(specPath); err == nil {
				files = s.Files
			} else {
				r.logger().Warn("in-flight ticket without a readable spec", "ticket", id, "source", "tmp/current-task", "err", err)
			}
			add(id, "tmp/current-task", files)
		}
	}

	out, err := r.git(ctx, "for-each-ref", "--format=%(refname:short)", "--no-merged="+base, "refs/heads/")
	if err != nil {
		return nil, fmt.Errorf("list unmerged branches: %w", err)
	}
	for branch := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		id := TicketFromBranch(branch)
		if id == "" {
			continue
		}
		data, err := r.git(ctx, "show", branch+":specs/"+id+".md")
		if err != nil {
			r.logger().Debug("branch has no spec, skipped", "branch", branch, "ticket", id)
			continue
		}
		s, err := ParseSpec(data)
		if err != nil {
			r.logger().Warn("in-flight branch with an unreadable spec", "branch", branch, "ticket", id, "err", err)
		}
		add(id, "branch "+branch, s.Files)
	}

	tickets := make([]InFlightTicket, 0, len(byID))
	for _, t := range byID {
		tickets = append(tickets, *t)
	}
	slices.SortFunc(tickets, func(a, b InFlightTicket) int { return CompareIDs(a.ID, b.ID) })
	return tickets, nil
}

// ChangedFiles returns the files changed on HEAD against base (three-dot
// diff), plus uncommitted and untracked files, sorted and without tmp/.
// Renames count as a deletion and an addition, so both paths are checked.
func (r Repo) ChangedFiles(ctx context.Context, base string) ([]string, error) {
	if err := checkRef(base); err != nil {
		return nil, err
	}
	diff, err := r.git(ctx, "diff", "--name-only", "-z", "--no-renames", base+"...HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("diff against %s: %w", base, err)
	}
	status, err := r.git(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	set := make(map[string]bool)
	for f := range strings.SplitSeq(string(diff), "\x00") {
		set[f] = true
	}
	// Each status entry is "XY path".
	for entry := range strings.SplitSeq(string(status), "\x00") {
		if len(entry) > 3 {
			set[entry[3:]] = true
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		if f == "" || f == "tmp" || strings.HasPrefix(f, "tmp/") {
			continue
		}
		files = append(files, f)
	}
	slices.Sort(files)
	return files, nil
}

// ShortCommit returns HEAD's abbreviated hash, or "" when there is none.
func (r Repo) ShortCommit(ctx context.Context) string {
	out, err := r.git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
