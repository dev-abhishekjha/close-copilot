package gates

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// ApprovedLabel is the pull request label with which the owner approves a
// change to a protected path.
const ApprovedLabel = "approved"

// ProtectedPaths need the owner's approval to change (CLAUDE.md, "Protected
// paths"). .github/CODEOWNERS names the owner on each of them. They are
// matched case-insensitively: on a case-insensitive checkout (macOS) a
// committed .CLAUDE/x or claude.md is the same file as .claude/x or
// CLAUDE.md. A changed path that is not printable ASCII is refused outright
// (see NonASCIIPaths), so no Unicode fold (docſ, full-width letters) can
// reach a protected file past the matcher.
var ProtectedPaths = []string{
	"evals/golden/**",
	"evals/scenarios/**",
	"evals/baseline.json",
	"gates/**",
	".github/**",
	".claude/**",
	"docs/**",
	"CLAUDE.md",
	".golangci.yml",
	"internal/books/admin.go",
	"internal/approvals/**",
	"internal/mcpkit/auth*.go",
	"config/users.yaml",
}

// Approval is the owner's approval of a pull request: the approved label
// and the head commit it was applied at. It covers exactly that commit; a
// commit pushed after it is not approved until the label is applied again.
type Approval struct {
	Labels      []string // pull request labels
	SHA         string   // head commit the owner approved; "" when not approved
	Head        string   // head commit being judged
	Uncommitted []string // changed files not in Head; no approval covers them
}

// approved reports whether the approval covers Head.
func (a Approval) approved() bool {
	return slices.Contains(a.Labels, ApprovedLabel) && a.SHA != "" && a.SHA == a.Head
}

// whyNot explains why the approval does not cover Head.
func (a Approval) whyNot() string {
	switch {
	case !slices.Contains(a.Labels, ApprovedLabel):
		return fmt.Sprintf("the %q label is absent", ApprovedLabel)
	case a.SHA == "":
		return fmt.Sprintf("no approved commit was given (the %q label covers only the head it was applied at; apply it again)", ApprovedLabel)
	default:
		return fmt.Sprintf("the owner approved %s but the head is %s; a commit was pushed after the approval, so apply the %q label again", shortHash(a.SHA), shortHash(a.Head), ApprovedLabel)
	}
}

// printableASCII reports whether every byte of s is in 0x20..0x7e.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// NonASCIIPaths returns the changed paths that are not printable ASCII, in
// order. Such a path could fold (NFKC, case folding, a filesystem's
// normalisation) onto a protected path that the ASCII matcher does not
// see, so the protected gate refuses it whatever the approval.
func NonASCIIPaths(changed []string) []string {
	var out []string
	for _, f := range changed {
		if !printableASCII(f) {
			out = append(out, f)
		}
	}
	return out
}

// ProtectedChanges returns, for each changed protected file, the first
// protected pattern it matches, ignoring case.
func ProtectedChanges(changed []string) (map[string]string, error) {
	hits := make(map[string]string)
	for _, f := range changed {
		for _, p := range ProtectedPaths {
			ok, err := Match(strings.ToLower(p), strings.ToLower(f))
			if err != nil {
				return nil, err
			}
			if ok {
				hits[f] = p
				break
			}
		}
	}
	return hits, nil
}

// ProtectedProblems returns one problem per changed path that is not
// printable ASCII, whatever the approval, and one per changed protected
// file unless the approval covers the head and the file is committed.
func ProtectedProblems(changed []string, a Approval) ([]Problem, error) {
	hits, err := ProtectedChanges(changed)
	if err != nil {
		return nil, err
	}
	var out []Problem
	for _, f := range NonASCIIPaths(changed) {
		out = append(out, Problem{
			Check:    "protected",
			Message:  fmt.Sprintf("%q is not a printable ASCII path; it could fold onto a protected path, so rename it to plain ASCII", f),
			Evidence: f,
		})
	}
	approved := a.approved()
	why := a.whyNot()
	for _, f := range changed {
		p, ok := hits[f]
		if !ok {
			continue
		}
		if approved {
			if !slices.Contains(a.Uncommitted, f) {
				continue
			}
			why = fmt.Sprintf("it is not committed; the approval covers only commit %s", shortHash(a.Head))
		}
		out = append(out, Problem{
			Check:    "protected",
			Message:  fmt.Sprintf("%s is a protected path (%s) and %s; the owner must approve this change", f, p, why),
			Evidence: f,
		})
	}
	return out, nil
}

// fullSHA matches a full SHA-1 or SHA-256 commit hash.
var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidApprovedSHA reports whether s is empty (not approved) or a full,
// lower-case commit hash.
func ValidApprovedSHA(s string) error {
	if s == "" || fullSHA.MatchString(s) {
		return nil
	}
	return fmt.Errorf("--approved-sha %q is not a full lower-case commit hash", s)
}

// SplitLabels splits a comma-separated --labels value.
func SplitLabels(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// LabelsFromEvent reads pull_request.labels[].name from a GitHub event
// payload ($GITHUB_EVENT_PATH).
func LabelsFromEvent(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is GITHUB_EVENT_PATH, set by the CI runner
	if err != nil {
		return nil, fmt.Errorf("read event payload: %w", err)
	}
	var ev struct {
		PullRequest struct {
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("parse event payload: %w", err)
	}
	var out []string
	for _, l := range ev.PullRequest.Labels {
		out = append(out, l.Name)
	}
	return out, nil
}
