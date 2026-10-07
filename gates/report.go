package gates

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Verdicts of a Report.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
)

// Blocking is one reason a gate failed. Repro is a command that reproduces
// the failure locally; Evidence points at what was checked (a file, a spec
// field, a git ref). Both are required.
type Blocking struct {
	Check    string `json:"check"`
	Message  string `json:"message"`
	Repro    string `json:"repro"`
	Evidence string `json:"evidence"`
}

// Report is the failure report: the only thing a worker receives back from
// a gate.
type Report struct {
	Gate        string     `json:"gate"`
	Task        string     `json:"task"`
	Commit      string     `json:"commit"`
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	Verdict     string     `json:"verdict"`
	Blocking    []Blocking `json:"blocking"`
}

// NewReport returns a passing report for gate and task.
func NewReport(gate, task string) *Report {
	return &Report{Gate: gate, Task: task, Verdict: VerdictPass, Blocking: []Blocking{}}
}

// Validate returns an error when b lacks a check, a repro command or an
// evidence pointer. Such an entry is a bug in the gate, not in the change.
func (b Blocking) Validate() error {
	switch {
	case b.Check == "":
		return errors.New("blocking entry without a check name")
	case b.Repro == "":
		return fmt.Errorf("blocking entry %q without a repro command", b.Check)
	case b.Evidence == "":
		return fmt.Errorf("blocking entry %q without an evidence pointer", b.Check)
	}
	return nil
}

// Add appends a blocking entry and marks the report failed. It rejects an
// entry without repro or evidence.
func (r *Report) Add(b Blocking) error {
	if err := b.Validate(); err != nil {
		return err
	}
	r.Blocking = append(r.Blocking, b)
	r.Verdict = VerdictFail
	return nil
}

// Failed reports whether the gate failed.
func (r *Report) Failed() bool { return r.Verdict == VerdictFail }

// Validate checks the verdict and every blocking entry.
func (r *Report) Validate() error {
	if r.Verdict != VerdictPass && r.Verdict != VerdictFail {
		return fmt.Errorf("verdict %q is neither %s nor %s", r.Verdict, VerdictPass, VerdictFail)
	}
	if r.Verdict == VerdictPass && len(r.Blocking) > 0 {
		return errors.New("a passing report has blocking entries")
	}
	if r.Verdict == VerdictFail && len(r.Blocking) == 0 {
		return errors.New("a failing report has no blocking entries")
	}
	for _, b := range r.Blocking {
		if err := b.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// JSON returns the indented report after validating it.
func (r *Report) JSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("invalid report: %w", err)
	}
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal report: %w", err)
	}
	return append(out, '\n'), nil
}

// Save writes the report to path, creating its directory.
func (r *Report) Save(path string) error {
	out, err := r.JSON()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
