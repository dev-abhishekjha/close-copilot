package seed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// GroundTruth represents the expected detection results for a company-month scenario,
// conforming to evals/schema/ground_truth.schema.json.
type GroundTruth struct {
	Scenario       string              `json:"scenario"`
	Company        string              `json:"company"`
	Month          string              `json:"month"`
	Clean          bool                `json:"clean"`
	Planted        []PlantedFinding    `json:"planted"`
	Expected       []ExpectedFinding   `json:"expected"`
	Investigations []InvestigationItem `json:"investigations"`
}

// PlantedFinding describes one planted error in ground truth.
type PlantedFinding struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Keys        map[string]string `json:"keys"`
	AmountPaise money.Paise       `json:"amount_paise"`
}

// ExpectedFinding describes an expected genuine mismatch (e.g. late-filed invoices).
type ExpectedFinding struct {
	Type string            `json:"type"`
	Keys map[string]string `json:"keys"`
}

// InvestigationItem describes an expected investigation item (e.g. gateway settlement).
type InvestigationItem struct {
	ID                 string            `json:"id"`
	Keys               map[string]string `json:"keys"`
	ExpectedResolution string            `json:"expected_resolution"`
}

// GroundTruthOptions specifies parameters for BuildGroundTruth.
type GroundTruthOptions struct {
	PlantedWorld PlantedWorld
	BankLines    []BankLine // optional: computed from PlantedWorld.BankWorld if nil
	ERPMap       ERPMap     // optional: used to resolve ERPNext document names
	GSTR2B       *GSTR2B    // optional: used to populate expected late-filed invoices
}

// BuildGroundTruth constructs the GroundTruth structure for a planted company-month world.
func BuildGroundTruth(opt GroundTruthOptions) (GroundTruth, error) {
	pw := opt.PlantedWorld
	if pw.Company == "" || pw.Month == "" {
		return GroundTruth{}, errors.New("planted world must have company and month")
	}

	bankLines := opt.BankLines
	if len(bankLines) == 0 {
		var err error
		bankLines, err = BankLines(pw.BankWorld)
		if err != nil {
			return GroundTruth{}, fmt.Errorf("build ground truth bank lines: %w", err)
		}
	}

	byExt := make(map[string]BankLine, len(bankLines))
	for _, l := range bankLines {
		byExt[l.ExtID] = l
	}

	planted := make([]PlantedFinding, 0, len(pw.PlantedErrors))
	for _, pe := range pw.PlantedErrors {
		keys := make(map[string]string)
		switch pe.Type {
		case ErrorUnrecordedBankCharge:
			if len(pe.TouchedExtIDs) == 0 {
				return GroundTruth{}, fmt.Errorf("planted error %s has no touched ext_ids", pe.ID)
			}
			bl, ok := byExt[pe.TouchedExtIDs[0]]
			if !ok {
				return GroundTruth{}, fmt.Errorf("planted error %s ext_id %s not found in bank lines", pe.ID, pe.TouchedExtIDs[0])
			}
			keys["bank_txn_id"] = bl.TxnID
		default:
			if len(pe.TouchedExtIDs) > 0 {
				extID := pe.TouchedExtIDs[0]
				if opt.ERPMap != nil {
					if ref, ok := opt.ERPMap[extID]; ok {
						keys["invoice"] = ref.Name
					}
				}
				if _, ok := keys["invoice"]; !ok {
					if bl, ok := byExt[extID]; ok {
						keys["bank_txn_id"] = bl.TxnID
					}
				}
			}
		}

		planted = append(planted, PlantedFinding{
			ID:          pe.ID,
			Type:        pe.Type,
			Keys:        keys,
			AmountPaise: pe.AmountPaise,
		})
	}

	investigations := make([]InvestigationItem, 0)
	for _, ev := range pw.BankWorld.Events {
		if ev.Kind == EventGatewaySettlement {
			bl, ok := byExt[ev.ExtID]
			if ok {
				investigations = append(investigations, InvestigationItem{
					ID:                 fmt.Sprintf("X%02d", len(investigations)+1),
					Keys:               map[string]string{"bank_txn_id": bl.TxnID},
					ExpectedResolution: "gateway_settlement",
				})
			}
		}
	}

	expected := make([]ExpectedFinding, 0)
	if opt.GSTR2B != nil && len(opt.GSTR2B.Late) > 0 {
		type b2bInfo struct {
			ctin string
			inum string
		}
		lateInvs := make(map[string]b2bInfo)
		for _, supp := range opt.GSTR2B.Data.DocData.B2B {
			for _, inv := range supp.Inv {
				lateInvs[inv.ExtID] = b2bInfo{ctin: supp.CTIN, inum: inv.Inum}
			}
		}
		for _, extID := range opt.GSTR2B.Late {
			if info, ok := lateInvs[extID]; ok {
				expected = append(expected, ExpectedFinding{
					Type: "gstr2b_wrong_period",
					Keys: map[string]string{
						"supplier_gstin":  info.ctin,
						"invoice_no_norm": NormInvoiceNo(info.inum),
					},
				})
			}
		}
	}

	return GroundTruth{
		Scenario:       pw.Scenario,
		Company:        pw.Company,
		Month:          pw.Month,
		Clean:          pw.Clean,
		Planted:        planted,
		Expected:       expected,
		Investigations: investigations,
	}, nil
}

// JSON returns the ground truth formatted as indented JSON with a trailing newline.
func (gt GroundTruth) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(gt, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal ground truth: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteGroundTruth writes gt to a destination file under outDir, returning the full path.
// It writes via an atomic temporary file to ensure no partial files are written on crash.
func WriteGroundTruth(outDir string, gt GroundTruth) (string, error) {
	b, err := gt.JSON()
	if err != nil {
		return "", err
	}
	if outDir == "" {
		outDir = "evals"
	}
	var filePath string
	switch {
	case strings.HasSuffix(outDir, ".json"):
		filePath = outDir
	case strings.HasSuffix(outDir, "ground_truth"):
		filePath = filepath.Join(outDir, fmt.Sprintf("%s-%s.json", gt.Company, gt.Month))
	default:
		filePath = filepath.Join(outDir, "scenarios", gt.Scenario, "ground_truth", fmt.Sprintf("%s-%s.json", gt.Company, gt.Month))
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("write ground truth dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ground_truth-*.json")
	if err != nil {
		return "", fmt.Errorf("write ground truth temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write ground truth: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write ground truth: %w", err)
	}
	if err := os.Rename(tmp.Name(), filePath); err != nil {
		return "", fmt.Errorf("write ground truth rename: %w", err)
	}
	return filePath, nil
}

// LoadGroundTruth reads a GroundTruth file written by WriteGroundTruth.
func LoadGroundTruth(path string) (GroundTruth, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the target file
	if err != nil {
		return GroundTruth{}, fmt.Errorf("load ground truth: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var gt GroundTruth
	if err := dec.Decode(&gt); err != nil {
		return GroundTruth{}, fmt.Errorf("load ground truth %s: %w", path, err)
	}
	if gt.Scenario == "" || gt.Company == "" || gt.Month == "" {
		return GroundTruth{}, fmt.Errorf("load ground truth %s: missing required fields", path)
	}
	return gt, nil
}

// NormInvoiceNo normalises an invoice number according to CC-403 shared spec:
// uppercase; drop spaces, '/', '-', '_' and '.'; strip leading zeros from each run of digits.
func NormInvoiceNo(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	b.Grow(len(s))
	var digitRun strings.Builder

	flushDigits := func() {
		if digitRun.Len() > 0 {
			str := digitRun.String()
			trimmed := strings.TrimLeft(str, "0")
			if trimmed == "" {
				trimmed = "0"
			}
			b.WriteString(trimmed)
			digitRun.Reset()
		}
	}

	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digitRun.WriteRune(r)
		case r == ' ' || r == '\t' || r == '/' || r == '-' || r == '_' || r == '.':
			flushDigits()
		default:
			flushDigits()
			b.WriteRune(r)
		}
	}
	flushDigits()
	return b.String()
}
