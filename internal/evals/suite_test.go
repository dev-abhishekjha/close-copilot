package evals

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const scenariosDir = "../../evals/scenarios"

func keys(ms []SuiteMonth) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Key()
		if m.Control {
			out[i] += "*"
		}
	}
	return out
}

// TestLoadRealSuites parses the committed suite files, read only.
func TestLoadRealSuites(t *testing.T) {
	tests := []struct {
		name string
		want []string // * marks the clean control month
	}{
		{"suite-skeleton", []string{"sharma:2026-09", "sharma:2026-08*"}},
		{"suite-v1", []string{
			"sharma:2026-07", "sharma:2026-08", "sharma:2026-09",
			"mehta:2026-07", "mehta:2026-08", "mehta:2026-09",
			"sharma:2026-06*",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := SuitePath(scenariosDir, tt.name)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := LoadSuite(path)
			if err != nil {
				t.Fatal(err)
			}
			if s.Name != tt.name {
				t.Errorf("name %q, want %q", s.Name, tt.name)
			}
			if got := keys(s.Months); !slices.Equal(got, tt.want) {
				t.Errorf("months %v, want %v", got, tt.want)
			}
			sum := sha256.Sum256(before)
			if s.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("sha256 %s, want the file bytes' %x", s.SHA256, sum)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("suite file changed by LoadSuite (%v)", err)
			}
		})
	}
}

func writeSuite(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSuiteRejects(t *testing.T) {
	const good = "evaluated:\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n"
	tests := []struct {
		name, file, body string
		unknown          bool
		wantErr          string
	}{
		{"name differs from file", "s1", "suite: s2\n" + good, true, "names suite"},
		{"no suite name", "s1", good, true, "names suite"},
		{"bad month", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-9\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "YYYY-MM"},
		{"month 13", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-13\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "YYYY-MM"},
		{"bad control month", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"Aug\"}\n", false, "clean_control"},
		{"bad history month", "s1", "suite: s1\nhistory_months: [\"2026-4\"]\n" + good, false, "history_months[0]"},
		{"duplicate evaluated month", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-09\", \"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "listed twice"},
		{"duplicate company entry", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "listed twice"},
		{"control repeats an evaluated month", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-09\"}\n", false, "listed twice"},
		{"no evaluated months", "s1", "suite: s1\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "no evaluated"},
		{"company without months", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: []}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "no months"},
		{"no clean control", "s1", "suite: s1\nevaluated:\n  - {company: sharma, months: [\"2026-09\"]}\n", false, "clean_control: missing"},
		{"bad company", "s1", "suite: s1\nevaluated:\n  - {company: \"../x\", months: [\"2026-09\"]}\nclean_control: {company: sharma, month: \"2026-08\"}\n", false, "company ID"},
		{"not yaml", "s1", "suite: [\n", false, "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSuite(writeSuite(t, tt.file, tt.body))
			if err == nil {
				t.Fatal("LoadSuite accepted the file")
			}
			if errors.Is(err, ErrUnknownSuite) != tt.unknown {
				t.Errorf("errors.Is(ErrUnknownSuite) = %v, want %v: %v", !tt.unknown, tt.unknown, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSuitePathAndMissingSuite(t *testing.T) {
	for _, name := range []string{"", "../suite-v1", "a/b", "Suite", "x.yaml"} {
		if _, err := SuitePath(scenariosDir, name); !errors.Is(err, ErrUnknownSuite) {
			t.Errorf("SuitePath(%q) = %v, want ErrUnknownSuite", name, err)
		}
	}
	path, err := SuitePath(scenariosDir, "suite-nope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(path); !errors.Is(err, ErrUnknownSuite) {
		t.Errorf("LoadSuite of a missing file = %v, want ErrUnknownSuite", err)
	}
}

func TestFilter(t *testing.T) {
	s := Suite{Name: "s", Months: []SuiteMonth{
		{Company: "sharma", Month: "2026-07"}, {Company: "sharma", Month: "2026-08"},
		{Company: "mehta", Month: "2026-07"}, {Company: "sharma", Month: "2026-06", Control: true},
	}}
	all, err := s.Filter(nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("Filter(nil) = %v, %v", keys(all), err)
	}
	got, err := s.Filter([]string{"sharma:2026-06", "mehta:2026-07", "mehta:2026-07"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mehta:2026-07", "sharma:2026-06*"}; !slices.Equal(keys(got), want) {
		t.Errorf("Filter kept %v, want %v in suite order", keys(got), want)
	}
	if _, err := s.Filter([]string{"sharma:2026-07", "kaveri:2026-07"}); err == nil || !strings.Contains(err.Error(), "kaveri:2026-07") {
		t.Errorf("Filter with a month outside the suite = %v", err)
	}
}
