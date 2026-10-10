package gates

import (
	"encoding/json"
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

// SpecPinProblems returns one problem per front-matter field the branch's
// spec changed against its pinned version (see Repo.PinnedSpec), in the
// order of the spec template. Every field is pinned: files, risk and
// approved_by bound what the gates allow, and the rest (acceptance, gates,
// budget, depends_on, ...) is what the owner approved. The order of files,
// depends_on and gates does not matter. from names the pinned version.
func SpecPinProblems(current, pinned Spec, from string) []Problem {
	var out []Problem
	for _, f := range pinFields(pinned, current) {
		if f.was == f.now {
			continue
		}
		out = append(out, Problem{
			Check:    "spec_pin",
			Message:  fmt.Sprintf("the branch changed %s in specs/%s.md from %s to %s (pinned at %s); only the owner changes it, in its own commit", f.name, current.ID, f.was, f.now, from),
			Evidence: from + "#" + f.name,
		})
	}
	return out
}

// pinField is one front-matter field rendered as JSON in both versions.
type pinField struct {
	name, was, now string
}

func pinFields(pinned, current Spec) []pinField {
	field := func(name string, was, now any) pinField {
		return pinField{name: name, was: renderPin(was), now: renderPin(now)}
	}
	return []pinField{
		field("id", pinned.ID, current.ID),
		field("title", pinned.Title, current.Title),
		field("phase", pinned.Phase, current.Phase),
		field("owner_role", pinned.OwnerRole, current.OwnerRole),
		field("risk", pinned.Risk, current.Risk),
		field("approved_by", pinned.ApprovedBy, current.ApprovedBy),
		field("depends_on", sortedCopy(pinned.DependsOn), sortedCopy(current.DependsOn)),
		field("needs_erpnext", pinned.NeedsERPNext, current.NeedsERPNext),
		field("files", sortedCopy(pinned.Files), sortedCopy(current.Files)),
		field("consumes", pinned.Consumes, current.Consumes),
		field("produces", pinned.Produces, current.Produces),
		field("acceptance", pinned.Acceptance, current.Acceptance),
		field("gates", sortedCopy(pinned.Gates), sortedCopy(current.Gates)),
		field("budget", pinned.Budget, current.Budget),
	}
}

// renderPin renders a field value as JSON; an empty list and a missing one
// render alike, as [].
func renderPin(v any) string {
	if l, ok := v.([]string); ok && l == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Every field is a string, bool, []string, *int or *Budget.
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
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
