package gates

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ReadyContext is what G0 needs beyond the spec itself.
type ReadyContext struct {
	// Base is the ref dependencies must be merged into, for messages.
	Base string
	// Done is the set of tickets merged into Base.
	Done map[string]bool
	// InFlight is every ticket in progress, possibly including this one.
	InFlight []InFlightTicket
	// Root is the repository root, for acceptance scripts.
	Root string
	// LookPath resolves a command name; exec.LookPath in production.
	LookPath func(string) (string, error)
}

// shellBuiltins may start an acceptance line though they need not be on PATH.
var shellBuiltins = []string{"test", "[", "cd"}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// SpecID returns the ticket ID a spec's file name implies: specs/CC-602.md
// gives CC-602.
func SpecID(specPath string) string {
	return strings.TrimSuffix(filepath.Base(specPath), ".md")
}

// CheckReady runs G0 on a parsed spec and returns one problem per failure.
// specPath is the spec as given on the command line, used in evidence.
func CheckReady(specPath string, s Spec, rc ReadyContext) []Problem {
	var out []Problem
	add := func(check, field, format string, args ...any) {
		out = append(out, Problem{Check: check, Message: fmt.Sprintf(format, args...), Evidence: specPath + "#" + field})
	}

	if want := SpecID(specPath); s.ID != want {
		add("id", "id", "id %q does not match the file name, which implies %q", s.ID, want)
	}

	for _, f := range s.missingFields() {
		add("required", f, "required field %s is empty", f)
	}
	if s.Risk != "" && !validRisk(s.Risk) {
		add("required", "risk", "risk %q is not one of %s", s.Risk, strings.Join(Risks, ", "))
	}
	if s.OwnerRole != "" && !validOwner(s.OwnerRole) {
		add("required", "owner_role", "owner_role %q is not one of %s", s.OwnerRole, strings.Join(Owners, ", "))
	}

	if len(s.Gates) > 0 {
		want := []string{"G1", "G2", "G3"}
		if s.Risk == RiskDataSensitive || s.Risk == RiskRegulated {
			want = append(want, "G5")
		}
		if s.Risk == RiskRegulated {
			want = append(want, "G6")
		}
		var missing []string
		for _, g := range want {
			if !slices.Contains(s.Gates, g) {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			add("gates", "gates", "gates lacks %s, required for a %s ticket", strings.Join(missing, ", "), s.Risk)
		}
	}

	var validFiles []string
	for i, f := range s.Files {
		if err := ValidatePattern(f); err != nil {
			add("files", fmt.Sprintf("files[%d]", i), "files entry %q is not a valid glob: %v", f, err)
			continue
		}
		validFiles = append(validFiles, f)
	}

	for _, d := range s.DependsOn {
		if !rc.Done[d] {
			out = append(out, Problem{
				Check:    "depends_on",
				Message:  fmt.Sprintf("dependency %s is not merged into %s (no commit subject starting with %q)", d, rc.Base, d+":"),
				Evidence: fmt.Sprintf("git log %s --format=%%s", rc.Base),
			})
		}
	}

	// The spec's own ticket is in flight too, under its id or its file name.
	for _, t := range rc.InFlight {
		if t.ID == s.ID || t.ID == SpecID(specPath) || len(t.Files) == 0 {
			continue
		}
		if Overlaps(validFiles, t.Files) {
			out = append(out, Problem{
				Check: "overlap",
				Message: fmt.Sprintf("declared files overlap in-flight %s (%s): %s vs %s",
					t.ID, strings.Join(t.Sources, ", "), strings.Join(validFiles, " "), strings.Join(t.Files, " ")),
				Evidence: strings.Join(t.Sources, ", ") + ": specs/" + t.ID + ".md#files",
			})
		}
	}

	for i, line := range s.Acceptance {
		if !runnable(line, validFiles, rc) {
			add("acceptance", fmt.Sprintf("acceptance[%d]", i),
				"acceptance line %q is not a runnable command: its first word is not on PATH, a shell builtin, an existing script or a script the spec declares", line)
		}
	}

	if s.Risk == RiskRegulated && strings.TrimSpace(s.ApprovedBy) == "" {
		add("approved_by", "approved_by", "a regulated spec needs the owner's approval in approved_by before a worker starts")
	}
	return out
}

func (s Spec) missingFields() []string {
	var missing []string
	check := func(name string, empty bool) {
		if empty {
			missing = append(missing, name)
		}
	}
	check("id", strings.TrimSpace(s.ID) == "")
	check("title", strings.TrimSpace(s.Title) == "")
	check("phase", s.Phase == nil)
	check("owner_role", strings.TrimSpace(s.OwnerRole) == "")
	check("risk", strings.TrimSpace(s.Risk) == "")
	check("files", len(s.Files) == 0)
	check("acceptance", len(s.Acceptance) == 0)
	check("gates", len(s.Gates) == 0)
	check("budget", s.Budget == nil || s.Budget.MaxAttempts <= 0 || s.Budget.MaxWallMinutes <= 0)
	return missing
}

// AcceptanceCommand returns the word an acceptance line runs: leading '!'
// and VAR=value assignments are stripped. It returns "" for an empty line.
func AcceptanceCommand(line string) string {
	line = strings.TrimSpace(line)
	for strings.HasPrefix(line, "!") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
	}
	for _, w := range strings.Fields(line) {
		if !assignment.MatchString(w) {
			return w
		}
	}
	return ""
}

func runnable(line string, files []string, rc ReadyContext) bool {
	word := AcceptanceCommand(line)
	switch {
	case word == "":
		return false
	case slices.Contains(shellBuiltins, word):
		return true
	case filepath.IsAbs(word):
		_, err := rc.LookPath(word)
		return err == nil
	case strings.Contains(word, "/"):
		rel := path.Clean(word)
		if info, err := os.Stat(filepath.Join(rc.Root, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
			return true
		}
		ok, err := MatchAny(files, rel)
		return err == nil && ok
	default:
		_, err := rc.LookPath(word)
		return err == nil
	}
}
