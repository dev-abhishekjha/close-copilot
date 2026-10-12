//go:build ignore

// gen regenerates the document corpus: the contract and close note
// Markdown from the company profiles and rules, then a PDF beside every
// Markdown file (the policy included).
//
//	go run corpus/gen.go          rewrite corpus/
//	go run corpus/gen.go -check   build into a temp directory and fail if
//	                              the committed files differ
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhishekjha/close-copilot/corpus/render"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

func main() {
	check := flag.Bool("check", false, "compare against the committed corpus instead of writing")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, "corpus/gen:", err)
		os.Exit(1)
	}
}

func run(check bool) error {
	profiles, err := seed.LoadProfiles(filepath.Join("config", "companies"))
	if err != nil {
		return err
	}
	rules, err := seed.LoadRules(filepath.Join("config", "rules.yaml"))
	if err != nil {
		return err
	}
	root := "corpus"
	out := root
	if check {
		out, err = os.MkdirTemp("", "corpus-check-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(out)
		policy, err := os.ReadFile(filepath.Join(root, "shared", "POL-001-accounting-policy.md"))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(out, "shared"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "shared", "POL-001-accounting-policy.md"), policy, 0o644); err != nil {
			return err
		}
	} else {
		// Drop stale generated files so a removed supplier leaves nothing behind.
		for _, p := range profiles {
			for _, sub := range []string{"contracts", "notes"} {
				if err := os.RemoveAll(filepath.Join(root, p.ID, sub)); err != nil {
					return err
				}
			}
		}
	}
	if err := seed.GenerateCorpus(profiles, rules, out); err != nil {
		return err
	}
	if err := render.Tree(out); err != nil {
		return err
	}
	if !check {
		return nil
	}
	want, err := files(out)
	if err != nil {
		return err
	}
	have, err := files(root)
	if err != nil {
		return err
	}
	var diffs []string
	for name, w := range want {
		h, ok := have[name]
		switch {
		case !ok:
			diffs = append(diffs, "missing: "+name)
		case !bytes.Equal(h, w):
			diffs = append(diffs, "differs: "+name)
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			diffs = append(diffs, "unexpected: "+name)
		}
	}
	if len(diffs) > 0 {
		return fmt.Errorf("committed corpus is stale (run go run corpus/gen.go): %d files, first: %s", len(diffs), minString(diffs))
	}
	return nil
}

func minString(s []string) string {
	m := s[0]
	for _, v := range s {
		if v < m {
			m = v
		}
	}
	return m
}

// files reads every .md and .pdf under root, keyed by slash-separated path.
func files(root string) (map[string][]byte, error) {
	m := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "render" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".pdf") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		m[filepath.ToSlash(rel)] = b
		return nil
	})
	return m, err
}
