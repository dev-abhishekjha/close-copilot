package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
)

const testSuiteYAML = `suite: tiny
history_months: ["2026-07"]
evaluated:
  - {company: sharma, months: ["2026-09", "2026-08"]}
clean_control: {company: sharma, month: "2026-07"}
`

func writeTinySuite(t *testing.T) (suitesDir string, suite evals.Suite) {
	t.Helper()
	suitesDir = t.TempDir()
	path := filepath.Join(suitesDir, "tiny.yaml")
	if err := os.WriteFile(path, []byte(testSuiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := evals.LoadSuite(path)
	if err != nil {
		t.Fatal(err)
	}
	return suitesDir, s
}

func writeMonth(t *testing.T, root, company, month string, files ...string) {
	t.Helper()
	dir := filepath.Join(root, company, month)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunAllArgumentErrors(t *testing.T) {
	suitesDir, _ := writeTinySuite(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no suite", nil, "--suite is required"},
		{"unknown suite", []string{"--suite", "nope", "--suites-dir", suitesDir}, "unknown suite"},
		{"not a suite name", []string{"--suite", "../x", "--suites-dir", suitesDir}, "not a suite name"},
		{"bad flag", []string{"--bogus"}, "bogus"},
	}
	run := newRun(io.Discard)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), config.Config{}, log, append([]string{"all"}, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadSuiteOrderAndMissingFile(t *testing.T) {
	_, suite := writeTinySuite(t)
	root := t.TempDir()
	writeMonth(t, root, "sharma", "2026-07", "bank.csv", "gstr2b.json")
	writeMonth(t, root, "sharma", "2026-08", "bank.csv", "gstr2b.json")
	writeMonth(t, root, "sharma", "2026-09", "bank.csv") // gstr2b.json missing

	var calls []string
	rec := func(kind string) monthLoader {
		return func(_ context.Context, company, month, path string) (any, error) {
			calls = append(calls, kind+":"+company+":"+month+":"+filepath.Base(path))
			return map[string]string{"kind": kind}, nil
		}
	}
	var out bytes.Buffer
	err := loadSuite(context.Background(), suite, root, rec("bank"), rec("gstr2b"), &out)
	if err == nil || !strings.Contains(err.Error(), "sharma 2026-09") {
		t.Fatalf("err = %v, want it to name sharma 2026-09", err)
	}
	want := []string{
		"bank:sharma:2026-07:bank.csv", "gstr2b:sharma:2026-07:gstr2b.json",
		"bank:sharma:2026-08:bank.csv", "gstr2b:sharma:2026-08:gstr2b.json",
	}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	// Months loaded before the failure are reported; no total on failure.
	if n := strings.Count(out.String(), `"month"`); n != 2 {
		t.Errorf("reported %d months, want 2:\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "company_months") {
		t.Errorf("total printed despite error")
	}

	writeMonth(t, root, "sharma", "2026-09", "gstr2b.json")
	out.Reset()
	calls = nil
	if err := loadSuite(context.Background(), suite, root, rec("bank"), rec("gstr2b"), &out); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 || !strings.Contains(out.String(), `"company_months": 3`) {
		t.Errorf("calls = %d, out:\n%s", len(calls), out.String())
	}
}
