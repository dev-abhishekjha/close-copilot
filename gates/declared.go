package gates

import (
	"fmt"
	"slices"
	"strings"
)

// alwaysAllowed may change on any ticket's branch, besides its own spec.
var alwaysAllowed = []string{"go.mod", "go.sum"}

// specGlob matches every ticket spec, compared in lower case.
const specGlob = "specs/cc-*.md"

// DeclaredProblems returns one problem per changed file that matches none of
// the spec's files globs. go.mod, go.sum and specs/<ID>.md are always
// allowed. Another ticket's spec (specs/CC-*.md, in any case) is never
// allowed, whatever the globs say. An invalid glob in the spec is an error:
// G0 should have caught it.
func DeclaredProblems(s Spec, changed []string) ([]Problem, error) {
	for _, f := range s.Files {
		if err := ValidatePattern(f); err != nil {
			return nil, fmt.Errorf("spec %s: %w", s.ID, err)
		}
	}
	own := "specs/" + s.ID + ".md"
	var out []Problem
	for _, f := range changed {
		if f == own || slices.Contains(alwaysAllowed, f) {
			continue
		}
		isSpec, err := Match(specGlob, strings.ToLower(f))
		if err != nil {
			return nil, err
		}
		if isSpec {
			out = append(out, Problem{
				Check:    "spec",
				Message:  fmt.Sprintf("%s changed on %s's branch; a branch may change only its own spec, %s", f, s.ID, own),
				Evidence: f,
			})
			continue
		}
		ok, err := MatchAny(s.Files, f)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, Problem{
				Check:    "declared",
				Message:  fmt.Sprintf("%s changed but matches none of %s's declared files: %s", f, s.ID, strings.Join(s.Files, " ")),
				Evidence: f,
			})
		}
	}
	return out, nil
}

// SpecPinProblems returns one problem per field the branch's spec changed
// against its pinned version (see Repo.PinnedSpec): files, risk and
// approved_by bound what the gates allow, so a branch may not edit them.
// The order of files does not matter. from names the pinned version.
func SpecPinProblems(current, pinned Spec, from string) []Problem {
	var out []Problem
	add := func(field, was, now string) {
		out = append(out, Problem{
			Check:    "spec_pin",
			Message:  fmt.Sprintf("the branch changed %s in specs/%s.md from %s to %s (pinned at %s); only the owner changes it, in its own commit", field, current.ID, was, now, from),
			Evidence: from + "#" + field,
		})
	}
	if a, b := sortedCopy(pinned.Files), sortedCopy(current.Files); !slices.Equal(a, b) {
		add("files", fmt.Sprintf("%q", a), fmt.Sprintf("%q", b))
	}
	if pinned.Risk != current.Risk {
		add("risk", fmt.Sprintf("%q", pinned.Risk), fmt.Sprintf("%q", current.Risk))
	}
	if pinned.ApprovedBy != current.ApprovedBy {
		add("approved_by", fmt.Sprintf("%q", pinned.ApprovedBy), fmt.Sprintf("%q", current.ApprovedBy))
	}
	return out
}

// CommitSubjectProblems returns one problem per commit whose subject does
// not start with "<task>:".
func CommitSubjectProblems(task string, commits []Commit) []Problem {
	var out []Problem
	for _, c := range commits {
		if strings.HasPrefix(c.Subject, task+":") {
			continue
		}
		out = append(out, Problem{
			Check:    "commit_subject",
			Message:  fmt.Sprintf("commit %s has subject %q; every commit on %s's branch starts with %q", shortHash(c.Hash), c.Subject, task, task+":"),
			Evidence: "commit " + shortHash(c.Hash),
		})
	}
	return out
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}
