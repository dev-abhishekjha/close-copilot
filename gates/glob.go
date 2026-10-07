// Package gates holds the build-time merge gates (CC-001): spec and task
// graph parsing, the glob semantics every gate shares, the failure report,
// and the G0 readiness and G1 declared-files and protected-path checks run
// by the commands under gates/cmd.
package gates

import (
	"errors"
	"fmt"
	"strings"
)

// Glob semantics, used by every gate (files in specs, files in the task
// graph, the protected list):
//
//   - Patterns are slash-separated paths relative to the repo root, such as
//     "internal/checks/*.go".
//   - '*' matches any run of characters within one path segment, dot files
//     included; '?' matches exactly one character within a segment. Every
//     other character is literal (there are no character classes or escapes).
//   - "**" as a whole segment matches zero or more whole segments. Inside a
//     segment ("a**b") it acts like '*'.
//   - A pattern ending in '/' matches everything under that directory:
//     "deploy/" is the same as "deploy/**".
//   - Braces are not supported: '{' anywhere in a pattern is an error.
//   - An empty pattern, a leading '/', and empty, "." or ".." segments are
//     errors.

// ErrBraces is returned for a pattern that uses '{'.
var ErrBraces = errors.New("braces are not supported in globs")

// ValidatePattern reports whether p is a valid glob under the semantics above.
func ValidatePattern(p string) error {
	_, err := splitPattern(p)
	return err
}

// Match reports whether the slash-separated path name matches pattern.
func Match(pattern, name string) (bool, error) {
	segs, err := splitPattern(pattern)
	if err != nil {
		return false, err
	}
	return matchSegs(segs, strings.Split(name, "/")), nil
}

// MatchAny reports whether name matches any of patterns. It returns the
// first invalid pattern's error.
func MatchAny(patterns []string, name string) (bool, error) {
	for _, p := range patterns {
		ok, err := Match(p, name)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// Overlaps reports whether some path could match a pattern in a and a
// pattern in b. It is conservative: an invalid pattern counts as overlapping
// everything, and "**" matching zero segments is taken into account even
// where the result would be a directory rather than a file.
func Overlaps(a, b []string) bool {
	for _, p := range a {
		for _, q := range b {
			if patternsOverlap(p, q) {
				return true
			}
		}
	}
	return false
}

func patternsOverlap(p, q string) bool {
	ps, err := splitPattern(p)
	if err != nil {
		return true
	}
	qs, err := splitPattern(q)
	if err != nil {
		return true
	}
	return overlapSegs(ps, qs)
}

func splitPattern(p string) ([]string, error) {
	switch {
	case p == "":
		return nil, errors.New("empty glob")
	case strings.Contains(p, "{"):
		return nil, fmt.Errorf("%q: %w", p, ErrBraces)
	case strings.HasPrefix(p, "/"):
		return nil, fmt.Errorf("%q: globs are relative to the repo root, drop the leading /", p)
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("%q: empty, . or .. path segment", p)
		}
	}
	return segs, nil
}

// matchSegs matches pattern segments against path segments.
func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 || !matchSeg([]rune(pat[0]), []rune(name[0])) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// matchSeg matches one segment pattern ('*', '?', literals) against s.
func matchSeg(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for i := 0; i <= len(s); i++ {
				if matchSeg(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// overlapSegs reports whether two segment patterns can match a common path.
func overlapSegs(a, b []string) bool {
	switch {
	case len(a) == 0 && len(b) == 0:
		return true
	case len(a) > 0 && a[0] == "**":
		// "**" matches nothing, or absorbs b's next segment.
		return overlapSegs(a[1:], b) || (len(b) > 0 && overlapSegs(a, b[1:]))
	case len(b) > 0 && b[0] == "**":
		return overlapSegs(a, b[1:]) || (len(a) > 0 && overlapSegs(a[1:], b))
	case len(a) == 0 || len(b) == 0:
		return false
	default:
		return overlapSeg([]rune(a[0]), []rune(b[0])) && overlapSegs(a[1:], b[1:])
	}
}

// overlapSeg reports whether two segment patterns can match a common string.
func overlapSeg(a, b []rune) bool {
	switch {
	case len(a) == 0 && len(b) == 0:
		return true
	case len(a) > 0 && a[0] == '*':
		// '*' matches nothing, or absorbs b's next character (or b's '*').
		return overlapSeg(a[1:], b) || (len(b) > 0 && overlapSeg(a, b[1:]))
	case len(b) > 0 && b[0] == '*':
		return overlapSeg(a, b[1:]) || (len(a) > 0 && overlapSeg(a[1:], b))
	case len(a) == 0 || len(b) == 0:
		return false
	case a[0] == '?' || b[0] == '?' || a[0] == b[0]:
		return overlapSeg(a[1:], b[1:])
	default:
		return false
	}
}
