package evals

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// TimestampLayout names a suite run's folder: results/<suite>/<timestamp>.
const TimestampLayout = "20060102T150405Z"

// ManifestFile is the name of a suite run's manifest.
const ManifestFile = "manifest.json"

// Result is one month's result file, <company>-<month>.json.
type Result struct {
	Suite   string `json:"suite"`
	Company string `json:"company"`
	Month   string `json:"month"`
	Control bool   `json:"control"`

	// RunID is null when the close never created a run.
	RunID  *string `json:"run_id"`
	Status string  `json:"status"` // done, partial or failed
	Reason string  `json:"reason"`
	// Error is the close's or the export's error text, "" when none.
	Error string `json:"error"`

	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationMS int64      `json:"duration_ms"`
	ReportPath string     `json:"report_path"`

	Findings []store.Finding `json:"findings"`
	Tokens   Tokens          `json:"tokens"`
	// CostUSD is the exact decimal sum of the run's llm_calls costs, as
	// text ("0" with no calls); never a float.
	CostUSD string `json:"cost_usd"`
	Models  Models `json:"models"`

	Commit  string  `json:"commit"`
	TraceID *string `json:"trace_id"`

	// VerifierRejects counts the verifier's failed verdicts in the run;
	// Retries counts the explain attempts after a finding's first (each one
	// follows a reject). Both are 0, and omitted, when the agent was off.
	VerifierRejects int64 `json:"verifier_rejects,omitempty"`
	Retries         int64 `json:"retries,omitempty"`
}

// Tokens are a run's model token totals.
type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
}

// Models are the configured model IDs and the ones the run called.
type Models struct {
	Fast   string   `json:"fast"`
	Strong string   `json:"strong"`
	Called []string `json:"called"` // distinct llm_calls models, sorted
}

// ManifestConfig is the non-secret configuration of a suite run. It is
// built field by field: no config.Secret is ever copied into it.
type ManifestConfig struct {
	LLMProvider    string `json:"llm_provider"`
	LLMModelFast   string `json:"llm_model_fast"`
	LLMModelStrong string `json:"llm_model_strong"`
	LLMRunTokenCap int64  `json:"llm_run_token_cap"`
	// LLMDailyBudgetUSD is the daily budget as decimal text.
	LLMDailyBudgetUSD string `json:"llm_daily_budget_usd"`
}

// Flags are the eval command's flags as given.
type Flags struct {
	Suite        string   `json:"suite"`
	Only         []string `json:"only"`
	ModelFast    string   `json:"model_fast"`
	NoAgent      bool     `json:"no_agent"`
	ResultsDir   string   `json:"results_dir"`
	ScenariosDir string   `json:"scenarios_dir"`
	ConfigDir    string   `json:"config_dir"`
	// Replay and Record are eval run --replay and --record; FixturesDir is
	// their --fixtures-dir. Omitted for a live run.
	Replay      bool   `json:"replay,omitempty"`
	Record      bool   `json:"record,omitempty"`
	FixturesDir string `json:"fixtures_dir,omitempty"`
}

// ManifestSuite identifies the suite file a run used.
type ManifestSuite struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ManifestEntry is one result file and how its run ended.
type ManifestEntry struct {
	Company string  `json:"company"`
	Month   string  `json:"month"`
	Control bool    `json:"control"`
	File    string  `json:"file"`
	RunID   *string `json:"run_id"`
	Status  string  `json:"status"`
	Reason  string  `json:"reason"`
	Failed  bool    `json:"failed"`
}

// Manifest is manifest.json, written last.
type Manifest struct {
	Suite      ManifestSuite   `json:"suite"`
	Config     ManifestConfig  `json:"config"`
	Flags      Flags           `json:"flags"`
	Agent      bool            `json:"agent"`
	Commit     string          `json:"commit"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Results    []ManifestEntry `json:"results"`
	Failed     int             `json:"failed"`
	// FixturesIndexSHA256 is the sha256 of the fixtures index a replay
	// served; omitted for a live or recording run. The scorer never reads
	// the fixtures themselves.
	FixturesIndexSHA256 string `json:"fixtures_index_sha256,omitempty"`
}

// marshalSorted encodes v as JSON with every object's keys sorted and a
// 2-space indent, plus a trailing newline. Numbers keep their exact text.
func marshalSorted(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("evals: encode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("evals: re-decode: %w", err)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(generic); err != nil { // maps encode with sorted keys
		return nil, fmt.Errorf("evals: encode: %w", err)
	}
	return b.Bytes(), nil
}

// writeJSON writes v to path atomically, with sorted keys and a 2-space
// indent.
func writeJSON(path string, v any) error {
	data, err := marshalSorted(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic writes data to a temp file in path's directory, syncs it
// and renames it over path, so a reader sees the old file or the whole new
// one, never a part. The file keeps CreateTemp's 0600 mode.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("evals: write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("evals: write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("evals: sync %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("evals: close %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("evals: rename %s: %w", path, err)
	}
	return nil
}

// sumDecimals adds decimal strings exactly (math/big rationals, no float)
// and returns the shortest exact decimal text: "0" for none, "0.0123",
// "1.5". An empty string counts as zero.
func sumDecimals(vals []string) (string, error) {
	total := new(big.Rat)
	scale := 0
	for _, v := range vals {
		if v == "" {
			continue
		}
		r, ok := new(big.Rat).SetString(v)
		if !ok || strings.ContainsAny(v, "eE/") {
			return "", fmt.Errorf("evals: cost %.40q is not a decimal", v)
		}
		if i := strings.IndexByte(v, '.'); i >= 0 && len(v)-i-1 > scale {
			scale = len(v) - i - 1
		}
		total.Add(total, r)
	}
	s := total.FloatString(scale) // exact: every addend has at most scale decimals
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s, nil
}
