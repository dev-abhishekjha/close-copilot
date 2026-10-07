package gates

import (
	"fmt"
	"slices"
	"strings"
)

// alwaysAllowed may change on any ticket's branch, besides its own spec.
var alwaysAllowed = []string{"go.mod", "go.sum"}

// DeclaredProblems returns one problem per changed file that matches none of
// the spec's files globs. go.mod, go.sum and specs/<ID>.md are always
// allowed. An invalid glob in the spec is an error: G0 should have caught it.
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
