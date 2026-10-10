package evals

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// SuiteMonth is one company-month a suite closes.
type SuiteMonth struct {
	Company string `json:"company"`
	Month   string `json:"month"` // YYYY-MM
	// Control marks the clean control month (no planted errors).
	Control bool `json:"control,omitempty"`
}

// Key is the month's "company:month" key, the form --only takes.
func (m SuiteMonth) Key() string { return m.Company + ":" + m.Month }

// FileName is the month's result file name, "<company>-<month>.json".
func (m SuiteMonth) FileName() string { return m.Company + "-" + m.Month + ".json" }

// Suite is what the runner needs from evals/scenarios/<suite>.yaml: the
// evaluated months, in file order, then the clean control month.
type Suite struct {
	Name   string       `json:"name"`
	Path   string       `json:"path"`
	SHA256 string       `json:"sha256"` // of the file bytes
	Months []SuiteMonth `json:"months"`
}

// suiteFile is the scenario file's shape. history_months, errors and small
// belong to the seeder and the scorer; they are parsed only so a malformed
// file fails here too.
type suiteFile struct {
	Suite         string   `yaml:"suite"`
	HistoryMonths []string `yaml:"history_months"`
	Evaluated     []struct {
		Company string   `yaml:"company"`
		Months  []string `yaml:"months"`
	} `yaml:"evaluated"`
	CleanControl *struct {
		Company string `yaml:"company"`
		Month   string `yaml:"month"`
	} `yaml:"clean_control"`
	Errors map[string]int `yaml:"errors"`
	Small  bool           `yaml:"small"`
}

// ErrUnknownSuite reports a suite with no scenario file, or a file whose
// suite name doesn't match its file name.
var ErrUnknownSuite = errors.New("evals: unknown suite")

// suiteNameRe is the shape of a suite name; it is also the file's base name.
var suiteNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// companyIDRe is the shape of a company ID (config/companies/<id>.yaml).
var companyIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SuitePath is the scenario file of suite name under dir. It rejects a
// name that isn't a plain suite name, so --suite can't point elsewhere.
func SuitePath(dir, name string) (string, error) {
	if !suiteNameRe.MatchString(name) {
		return "", fmt.Errorf("%w %.64q: not a suite name", ErrUnknownSuite, name)
	}
	return filepath.Join(dir, name+".yaml"), nil
}

// LoadSuite reads and validates a suite file. It rejects a missing file or
// one whose suite name isn't its base name (ErrUnknownSuite), a month that
// isn't YYYY-MM, a bad company ID, a suite with no evaluated month or no
// clean control month, and any company-month listed twice.
func LoadSuite(path string) (Suite, error) {
	path = filepath.Clean(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Suite{}, fmt.Errorf("%w: no scenario file %s", ErrUnknownSuite, path)
		}
		return Suite{}, fmt.Errorf("evals: read suite: %w", err)
	}
	var f suiteFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&f); err != nil {
		return Suite{}, fmt.Errorf("evals: suite %s: %w", path, err)
	}

	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if f.Suite == "" || f.Suite != base || !suiteNameRe.MatchString(f.Suite) {
		return Suite{}, fmt.Errorf("%w: %s names suite %.64q, want %q", ErrUnknownSuite, path, f.Suite, base)
	}

	sum := sha256.Sum256(data)
	s := Suite{Name: f.Suite, Path: path, SHA256: hex.EncodeToString(sum[:])}

	var errs []error
	seen := map[string]bool{}
	add := func(field string, m SuiteMonth) {
		switch {
		case !companyIDRe.MatchString(m.Company):
			errs = append(errs, fmt.Errorf("%s: company %.64q is not a company ID", field, m.Company))
			return
		case validMonth(m.Month) != nil:
			errs = append(errs, fmt.Errorf("%s: %w", field, validMonth(m.Month)))
			return
		case seen[m.Key()]:
			errs = append(errs, fmt.Errorf("%s: %s is listed twice", field, m.Key()))
			return
		}
		seen[m.Key()] = true
		s.Months = append(s.Months, m)
	}
	for i, e := range f.Evaluated {
		if len(e.Months) == 0 {
			errs = append(errs, fmt.Errorf("evaluated[%d]: company %.64q has no months", i, e.Company))
		}
		for j, m := range e.Months {
			add(fmt.Sprintf("evaluated[%d].months[%d]", i, j), SuiteMonth{Company: e.Company, Month: m})
		}
	}
	if len(f.Evaluated) == 0 {
		errs = append(errs, errors.New("evaluated: no evaluated months"))
	}
	if f.CleanControl == nil {
		errs = append(errs, errors.New("clean_control: missing"))
	} else {
		add("clean_control", SuiteMonth{Company: f.CleanControl.Company, Month: f.CleanControl.Month, Control: true})
	}
	for i, m := range f.HistoryMonths {
		if err := validMonth(m); err != nil {
			errs = append(errs, fmt.Errorf("history_months[%d]: %w", i, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Suite{}, fmt.Errorf("evals: suite %s: %w", path, err)
	}
	return s, nil
}

// validMonth checks a YYYY-MM month.
func validMonth(m string) error {
	if _, err := company.ParseMonth(m); err != nil {
		return fmt.Errorf("month: %w", err)
	}
	return nil
}

// Filter keeps the months whose key is in only ("company:month"), in suite
// order. An empty only keeps every month. A key that matches no month of
// the suite is an error, so a typo doesn't run nothing silently.
func (s Suite) Filter(only []string) ([]SuiteMonth, error) {
	if len(only) == 0 {
		return append([]SuiteMonth(nil), s.Months...), nil
	}
	want := map[string]bool{}
	for _, k := range only {
		want[k] = true
	}
	var out []SuiteMonth
	for _, m := range s.Months {
		if want[m.Key()] {
			out = append(out, m)
			delete(want, m.Key())
		}
	}
	if len(want) > 0 {
		var missing []string
		for _, k := range only {
			if want[k] {
				missing = append(missing, k)
				delete(want, k)
			}
		}
		return nil, fmt.Errorf("evals: --only %s: not a month of suite %s", strings.Join(missing, ", "), s.Name)
	}
	return out, nil
}
