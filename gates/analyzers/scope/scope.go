// Package scope matches Go import paths against the repo's package
// directories (internal/agent, internal/httpx, ...) by path segments, never
// by raw substrings, so internal/agent covers internal/agent/sub but not
// internal/agentx.
//
// A scope is anchored at the module root: for this module's packages the
// module path is stripped first; any other path is taken as it is, which is
// how analysistest's GOPATH-style testdata names packages (internal/agent/bad).
// Third-party packages that happen to contain an internal/agent directory
// therefore never match.
package scope

import (
	"path/filepath"
	"strings"
)

// Module is this repository's module path (go.mod). A test keeps it in sync.
const Module = "github.com/abhishekjha/close-copilot"

// Rel returns path relative to the module root. A go list test-variant
// suffix ("foo [foo.test]") is dropped, because that variant compiles the
// same directory. A trailing _test is kept: a package path ending in _test
// names another directory (or an external test package, whose files are
// all *_test.go), so it never inherits the allowances of the package it
// resembles.
func Rel(path string) string {
	if i := strings.Index(path, " ["); i >= 0 {
		path = path[:i]
	}
	if path == Module {
		return ""
	}
	return strings.TrimPrefix(path, Module+"/")
}

// Is reports whether path is exactly the package dir (for example
// "internal/httpx"), not one of its subpackages. Use it for allowances and
// exemptions.
func Is(path, dir string) bool {
	return Rel(path) == dir
}

// In reports whether path is one of dirs or a subpackage of one of them.
// Use it for allowances and exemptions.
func In(path string, dirs ...string) bool {
	return within(Rel(path), dirs)
}

// InTested is In, except that an external test package (foo_test) also
// counts as the package it tests (foo). Use it only where matching widens
// a rule's reach (the packages a rule applies to), never for an allowance:
// there the trailing _test would let an unrelated directory such as
// internal/approvals_test inherit internal/approvals' exemption.
func InTested(path string, dirs ...string) bool {
	return within(strings.TrimSuffix(Rel(path), "_test"), dirs)
}

func within(rel string, dirs []string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// IsTestFile reports whether filename is a Go test file (*_test.go).
func IsTestFile(filename string) bool {
	return strings.HasSuffix(filename, "_test.go")
}

// Base returns the last element of filename ("auth.go").
func Base(filename string) string {
	return filepath.Base(filename)
}
