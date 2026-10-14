package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
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
		{"serve with defaults", []string{"serve"},
			command{name: cmdServe, resultsDir: "results", timeout: 10 * time.Minute, configDir: "config"}},
		{"serve with every flag", []string{"serve", "--results-dir", "/tmp/r", "--config-dir", "/etc/cc", "--no-explain"},
			command{name: cmdServe, resultsDir: "/tmp/r", timeout: 10 * time.Minute, configDir: "/etc/cc", noExplain: true}},
		{"mint-scope with the default ttl", []string{"mint-scope", "--company", "sharma", "--month", "2026-09"},
			command{name: cmdMintScope, company: "sharma", month: "2026-09", ttl: time.Hour}},
		{"mint-scope with a ttl", []string{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "24h"},
			command{name: cmdMintScope, company: "sharma", month: "2026-09", ttl: 24 * time.Hour}},
		{"mint-scope for a company with no profile", []string{"mint-scope", "--company", "nobody", "--month", "2026-09"},
			command{name: cmdMintScope, company: "nobody", month: "2026-09", ttl: time.Hour}},
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
		{"serve", "extra"},
		{"serve", "--unknown"},
		{"serve", "--company", "sharma"},
		{"serve", "--month", "2026-09"},
		{"serve", "--timeout", "5m"},
		{"serve", "--addr", ":9000"},
		{"serve", "--results-dir", ""},
		{"mint-scope"},
		{"mint-scope", "--company", "sharma"},
		{"mint-scope", "--month", "2026-09"},
		{"mint-scope", "--company", "sharma", "--month", "2026-9"},
		{"mint-scope", "--company", "sharma", "--month", "2026-13"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "25h"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "24h1s"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "0s"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "-1h"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "extra"},
		{"mint-scope", "--company", "sharma", "--month", "2026-09", "--results-dir", "x"},
	} {
		if _, err := parseArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(%q): %v, want a usage error", args, err)
		}
	}
}

// TestMintScopeVerifies runs mint-scope, captures what it prints, and
// verifies it with the same key: one line, nothing else, carrying the
// company, the month's window and the ttl.
func TestMintScopeVerifies(t *testing.T) {
	var out bytes.Buffer
	old := stdout
	stdout = &out
	t.Cleanup(func() { stdout = old })

	const key = "mint-scope-test-key"
	cfg := config.Config{MCPScopeKey: config.NewSecret(key)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	before := time.Now()
	if err := run(t.Context(), cfg, log, []string{"mint-scope", "--company", "sharma", "--month", "2026-09", "--ttl", "2h"}); err != nil {
		t.Fatalf("mint-scope: %v", err)
	}
	printed := out.String()
	if strings.Count(printed, "\n") != 1 || !strings.HasSuffix(printed, "\n") {
		t.Fatalf("mint-scope printed %q, want exactly one line", printed)
	}
	sc, err := mcpkit.Verify([]byte(key), strings.TrimSuffix(printed, "\n"), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sc.Company != "sharma" || sc.From != "2025-09-01" || sc.To != "2026-10-31" || !strings.HasPrefix(sc.RunID, "manual-") {
		t.Errorf("scope %+v, want sharma 2025-09-01..2026-10-31 for a manual run", sc)
	}
	if exp := time.Unix(sc.Exp, 0); exp.Before(before.Add(2*time.Hour-time.Second)) || exp.After(time.Now().Add(2*time.Hour)) {
		t.Errorf("exp %s, want two hours from now", exp)
	}
	if _, err := mcpkit.Verify([]byte("another-key"), strings.TrimSpace(printed), time.Now()); err == nil {
		t.Error("the scope verifies under another key")
	}

	out.Reset()
	if err := run(t.Context(), config.Config{}, log, []string{"mint-scope", "--company", "sharma", "--month", "2026-09"}); err == nil ||
		!strings.Contains(err.Error(), config.EnvMCPScopeKey) || out.Len() != 0 {
		t.Errorf("mint-scope without a key: %v, printed %q; want an error naming %s and no output", err, out.String(), config.EnvMCPScopeKey)
	}
}
