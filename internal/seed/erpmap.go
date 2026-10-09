package seed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ERPMapFile is the file name of a company-month's ERP map.
const ERPMapFile = "erp_map.json"

// ERPRef is the ERPNext document an event was posted as.
type ERPRef struct {
	DocType string `json:"doctype"`
	Name    string `json:"name"`
}

// ERPMap maps an event's ExtID to the ERPNext document it was posted as.
// Ground truth (CC-306) and the next months' Refs resolve through it.
type ERPMap map[string]ERPRef

// ERPMapDir is the directory of one company-month's outputs:
// <out>/<suite>/<company>-<month>. Suite, company and month must each be a
// single path element.
func ERPMapDir(out, suite, company, month string) (string, error) {
	for _, p := range []struct{ what, s string }{{"suite", suite}, {"company", company}, {"month", month}} {
		if err := checkPathPart(p.what, p.s); err != nil {
			return "", err
		}
	}
	return filepath.Join(out, suite, company+"-"+month), nil
}

// JSON returns the map as WriteERPMap writes it: sorted by ExtID (JSON
// object keys are sorted), 2-space indent and a trailing newline.
func (m ERPMap) JSON() ([]byte, error) {
	if m == nil {
		m = ERPMap{}
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal erp map: %w", err)
	}
	return append(b, '\n'), nil
}

func (m ERPMap) validate() error {
	var errs []error
	for ext, ref := range m {
		if ext == "" || ref.DocType == "" || ref.Name == "" {
			errs = append(errs, fmt.Errorf("erp map entry %q has doctype %q and name %q", ext, ref.DocType, ref.Name))
		}
	}
	return errors.Join(errs...)
}

// WriteERPMap writes m to dir/erp_map.json, creating dir. It writes a
// temporary file and renames it, so a crash never leaves a partial map
// under the final name. Write the map only after a successful run.
func WriteERPMap(dir string, m ERPMap) error {
	b, err := m.JSON()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("write erp map: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".erp_map-*.json")
	if err != nil {
		return fmt.Errorf("write erp map: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write erp map: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write erp map: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, ERPMapFile)); err != nil {
		return fmt.Errorf("write erp map: %w", err)
	}
	return nil
}

// LoadERPMap reads a map written by WriteERPMap. Unknown fields and empty
// entries are errors.
func LoadERPMap(path string) (ERPMap, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the seeder's own output
	if err != nil {
		return nil, fmt.Errorf("load erp map: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m ERPMap
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("load erp map %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("load erp map %s: trailing data after the object", path)
	}
	if m == nil {
		return nil, fmt.Errorf("load erp map %s: not a JSON object", path)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("load erp map %s: %w", path, err)
	}
	return m, nil
}

// checkPathPart rejects a suite, company or month that would escape the
// output directory.
func checkPathPart(what, s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%s %q can't be part of a path", what, s)
	}
	return nil
}
