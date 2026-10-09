package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

var configDir = filepath.Join("..", "..", "config", "companies")

// emptyEnv loads the config from an environment with nothing set.
func emptyEnv(required ...string) (config.Config, error) {
	return config.Load(func(string) (string, bool) { return "", false }, required...)
}

func runSeed(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run("seed", args, &out, &errOut, emptyEnv, requiredEnv(args), newRun(&out))
	return code, out.String(), errOut.String()
}

func TestWorldRunsWithoutERPEnv(t *testing.T) {
	code, out, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("output doesn't end in a closing brace and a newline")
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var w seed.World
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("decode world: %v", err)
	}
	if w.Company != "sharma" || w.Month != "2026-09" || len(w.Events) == 0 {
		t.Errorf("world %s %s with %d events", w.Company, w.Month, len(w.Events))
	}

	p, err := seed.LoadProfile(filepath.Join(configDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := seed.Generate(p, "2026-09", seed.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, want) {
		t.Error("printed world doesn't decode to Generate's world")
	}
	golden, err := os.ReadFile(filepath.Join("..", "..", "internal", "seed", "testdata", "world-sharma-2026-09.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(golden) {
		t.Error("printed world differs from the golden file")
	}
}

func TestWorldSmall(t *testing.T) {
	_, full, _ := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--config", configDir)
	code, small, errOut := runSeed(t, "world", "--company", "sharma", "--month", "2026-09", "--small", "--config", configDir)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if len(small) >= len(full) {
		t.Errorf("--small printed %d bytes, full %d", len(small), len(full))
	}
}

func TestWorldErrors(t *testing.T) {
	cases := [][]string{
		{"world"},
		{"world", "--company", "sharma"},
		{"world", "--company", "sharma", "--month", "2026-13", "--config", configDir},
		{"world", "--company", "nobody", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "../sharma", "--month", "2026-09", "--config", configDir},
		{"world", "--company", "sharma", "--month", "2026-09", "--config", configDir, "extra"},
		{"world", "--bogus"},
	}
	for _, args := range cases {
		code, out, _ := runSeed(t, args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		if out != "" {
			t.Errorf("%v: printed %q on failure", args, out)
		}
	}
}

func TestRequiredEnv(t *testing.T) {
	if got := requiredEnv([]string{"world", "--company", "sharma"}); len(got) != 0 {
		t.Errorf("world requires %v, want nothing", got)
	}
	for _, args := range [][]string{nil, {"books"}, {"all"}, {"--version"}} {
		got := requiredEnv(args)
		if !slices.Equal(got, erpEnv) {
			t.Errorf("%v requires %v, want %v", args, got, erpEnv)
		}
	}
	// Other subcommands still need the ERPNext variables.
	if code, _, errOut := runSeed(t, "books"); code != 1 || !strings.Contains(errOut, config.EnvERPBaseURL) {
		t.Errorf("books with no env: exit %d, stderr %q", code, errOut)
	}
}
