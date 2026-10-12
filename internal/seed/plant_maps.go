package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// BootstrapReportPath is where a company's bootstrap report sits next to
// its month maps: <out>/<suite>/<company>-bootstrap.json. seed plant reads
// it for the ERPNext supplier names a missing_accrual key needs.
func BootstrapReportPath(out, suite, company string) (string, error) {
	for _, p := range []struct{ what, s string }{{"suite", suite}, {"company", company}} {
		if err := checkPathPart(p.what, p.s); err != nil {
			return "", err
		}
	}
	return filepath.Join(out, suite, company+"-bootstrap.json"), nil
}

// BootstrapReportJSON is the report as WriteBootstrapReport writes it:
// 2-space indent and a trailing newline.
func BootstrapReportJSON(rep BootstrapReport) ([]byte, error) {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal bootstrap report: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteBootstrapReport writes rep to path atomically.
func WriteBootstrapReport(path string, rep BootstrapReport) error {
	b, err := BootstrapReportJSON(rep)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, b); err != nil {
		return fmt.Errorf("write bootstrap report: %w", err)
	}
	return nil
}

// LoadBootstrapReport reads a report written by WriteBootstrapReport.
func LoadBootstrapReport(path string) (BootstrapReport, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the seeder's own output
	if err != nil {
		return BootstrapReport{}, fmt.Errorf("load bootstrap report: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rep BootstrapReport
	if err := dec.Decode(&rep); err != nil {
		return BootstrapReport{}, fmt.Errorf("load bootstrap report %s: %w", path, err)
	}
	return rep, nil
}

// SupplierNames maps each profile supplier id to its ERPNext Supplier
// name: the bootstrap report's when it has one, else the profile name
// (Bootstrap names every supplier after its profile name).
func SupplierNames(p Profile, rep *BootstrapReport) map[string]string {
	out := make(map[string]string, len(p.Suppliers))
	for _, s := range p.Suppliers {
		out[s.ID] = s.Name
		if rep != nil && rep.Suppliers[s.ID] != "" {
			out[s.ID] = rep.Suppliers[s.ID]
		}
	}
	return out
}
