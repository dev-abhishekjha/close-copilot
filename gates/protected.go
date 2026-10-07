package gates

import (
	"encoding/json"
	"fmt"
	"os"
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
// CLAUDE.md.
var ProtectedPaths = []string{
	"evals/golden/**",
	"evals/scenarios/**",
	"evals/baseline.json",
	"gates/**",
	".github/**",
	".claude/**",
	"docs/**",
	"CLAUDE.md",
	"internal/books/admin.go",
	"internal/approvals/**",
	"internal/mcpkit/auth*.go",
	"config/users.yaml",
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

// ProtectedProblems returns one problem per changed protected file when the
// approved label is absent, and none when it is present.
func ProtectedProblems(changed, labels []string) ([]Problem, error) {
	hits, err := ProtectedChanges(changed)
	if err != nil {
		return nil, err
	}
	if slices.Contains(labels, ApprovedLabel) {
		return nil, nil
	}
	var out []Problem
	for _, f := range changed {
		p, ok := hits[f]
		if !ok {
			continue
		}
		out = append(out, Problem{
			Check:    "protected",
			Message:  fmt.Sprintf("%s is a protected path (%s) and the %q label is absent; the owner must approve this change", f, p, ApprovedLabel),
			Evidence: f,
		})
	}
	return out, nil
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
