package main

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseArgs(t *testing.T) {
	id := uuid.MustParse("6f1c2a52-7d1b-4c1e-9d1a-2b3c4d5e6f70")
	tests := []struct {
		name string
		args []string
		want command
	}{
		{"close with defaults", []string{"close", "--company", "sharma", "--month", "2026-09"},
			command{name: cmdClose, company: "sharma", month: "2026-09", resultsDir: "results", timeout: 10 * time.Minute, configDir: "config"}},
		{"close with every flag", []string{"close", "-company=sharma", "-month=2026-09", "--results-dir", "/tmp/r", "--timeout", "90s", "--config-dir", "/etc/cc"},
			command{name: cmdClose, company: "sharma", month: "2026-09", resultsDir: "/tmp/r", timeout: 90 * time.Second, configDir: "/etc/cc"}},
		{"resume", []string{"resume", id.String()},
			command{name: cmdResume, runID: id, resultsDir: "results", timeout: 10 * time.Minute, configDir: "config"}},
		{"resume with flags after the ID", []string{"resume", id.String(), "--results-dir", "out", "--timeout", "10m"},
			command{name: cmdResume, runID: id, resultsDir: "out", timeout: 10 * time.Minute, configDir: "config"}},
		{"resume with flags before the ID", []string{"resume", "--timeout", "5m", id.String()},
			command{name: cmdResume, runID: id, resultsDir: "results", timeout: 5 * time.Minute, configDir: "config"}},
		{"close without explaining", []string{"close", "--company", "sharma", "--month", "2026-09", "--no-explain"},
			command{name: cmdClose, company: "sharma", month: "2026-09", resultsDir: "results", timeout: 10 * time.Minute, configDir: "config", noExplain: true}},
		{"resume without explaining", []string{"resume", id.String(), "--no-explain"},
			command{name: cmdResume, runID: id, resultsDir: "results", timeout: 10 * time.Minute, configDir: "config", noExplain: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseArgsRejects(t *testing.T) {
	id := uuid.New().String()
	for _, args := range [][]string{
		nil,
		{"explain"},
		{"close"},
		{"close", "--company", "sharma"},
		{"close", "--month", "2026-09"},
		{"close", "--company", "sharma", "--month", "2026-9"},
		{"close", "--company", "sharma", "--month", "2026-13"},
		{"close", "--company", "sharma", "--month", "2026-09", "extra"},
		{"close", "--company", "sharma", "--month", "2026-09", "--timeout", "11m"},
		{"close", "--company", "sharma", "--month", "2026-09", "--timeout", "0s"},
		{"close", "--company", "sharma", "--month", "2026-09", "--timeout", "-1m"},
		{"close", "--company", "sharma", "--month", "2026-09", "--results-dir", ""},
		{"close", "--company", "sharma", "--month", "2026-09", "--unknown"},
		{"close", "--company", "sharma", "--month", "2026-09", "--no-explain=maybe"},
		{"resume"},
		{"resume", "not-a-uuid"},
		{"resume", "00000000-0000-0000-0000-000000000000"},
		{"resume", id, id},
		{"resume", id, "--timeout", "1h"},
	} {
		if _, err := parseArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(%q): %v, want a usage error", args, err)
		}
	}
}
