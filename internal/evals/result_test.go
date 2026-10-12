package evals

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomicRefusesDotDot: the backstop for a caller that forgot
// CheckOutputPath. A path with a ".." component returns an error and
// creates nothing, not even the temp file.
func TestWriteFileAtomicRefusesDotDot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir + "/a/../x.json", dir + "/a/..", dir + "/a/../a/x.json", "../x.json"} {
		if err := writeFileAtomic(p, []byte("{}")); !errors.Is(err, ErrProtectedOutput) {
			t.Errorf("writeFileAtomic(%q) = %v, want ErrProtectedOutput", p, err)
		}
		if err := writeJSON(p, map[string]int{"a": 1}); !errors.Is(err, ErrProtectedOutput) {
			t.Errorf("writeJSON(%q) = %v, want ErrProtectedOutput", p, err)
		}
	}
	for _, d := range []string{dir, filepath.Join(dir, "a")} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if d == dir {
			want = 1 // a
		}
		if len(entries) != want {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s holds %v", d, names)
		}
	}
	// A name that only contains dots still writes.
	ok := filepath.Join(dir, "a", "x..y.json")
	if err := writeFileAtomic(ok, []byte("{}")); err != nil {
		t.Errorf("writeFileAtomic(%q) = %v", ok, err)
	}
}

func TestHasDotDot(t *testing.T) {
	for p, want := range map[string]bool{
		"":               false,
		".":              false,
		"..":             true,
		"../x":           true,
		"a/..":           true,
		"a/../b":         true,
		"/a/b/../c":      true,
		"a//..//b":       true,
		"a..b":           false,
		"a/...":          false,
		"a/.hidden":      false,
		"a/..x/y":        false,
		"a/x../y":        false,
		"/abs/ok/x.json": false,
	} {
		if got := hasDotDot(p); got != want {
			t.Errorf("hasDotDot(%q) = %v, want %v", p, got, want)
		}
	}
}
