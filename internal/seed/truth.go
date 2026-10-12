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
	// ERPMap resolves ERPNext document names; required for every type keyed
	// on an invoice.
	ERPMap ERPMap
	// GSTR2B is the month's return; it fills the expected late-filed
	// invoices and is required for the gstr2b_* types.
	GSTR2B *GSTR2B
	// Suppliers maps a profile supplier id to its ERPNext Supplier name
	// (BootstrapReport.Suppliers); required for missing_accrual.
	Suppliers map[string]string
}

// truthCtx resolves a planted error's keys. Every key must point at a real
// record (an ERP map name, a bank line or a GSTR-2B invoice); a key that
// can't be resolved is an error, never dropped.
type truthCtx struct {
	opt    GroundTruthOptions
	bank   map[string]BankLine
	books  map[string]Event
	g2bInv map[string]b2bRef
}

type b2bRef struct{ ctin, inum string }

func (c *truthCtx) invoice(pe PlantedError, ext string) (string, error) {
	ref, ok := c.opt.ERPMap[ext]
	if !ok || ref.DocType != DocPurchaseInvoice {
		return "", fmt.Errorf("planted error %s (%s): ext_id %s is not a Purchase Invoice in the ERP map", pe.ID, pe.Type, ext)
	}
	return ref.Name, nil
}

func (c *truthCtx) inGSTR2B(pe PlantedError, ext string) (b2bRef, error) {
	if c.opt.GSTR2B == nil {
		return b2bRef{}, fmt.Errorf("planted error %s (%s): no GSTR-2B to resolve its keys", pe.ID, pe.Type)
	}
	ref, ok := c.g2bInv[ext]
	if !ok {
		return b2bRef{}, fmt.Errorf("planted error %s (%s): ext_id %s is not in the GSTR-2B", pe.ID, pe.Type, ext)
	}
	return ref, nil
}

func gstr2bKeys(ctin, inum string) map[string]string {
	return map[string]string{"supplier_gstin": ctin, "invoice_no_norm": NormInvoiceNo(inum)}
}

// keys builds a planted error's keys per the shared spec's finding types.
func (c *truthCtx) keys(pe PlantedError, month string) (map[string]string, error) {
	if len(pe.TouchedExtIDs) == 0 {
		return nil, fmt.Errorf("planted error %s has no touched ext_ids", pe.ID)
	}
	ext := pe.TouchedExtIDs[0]
	switch pe.Type {
	case ErrorUnrecordedBankCharge:
		bl, ok := c.bank[ext]
		if !ok {
			return nil, fmt.Errorf("planted error %s ext_id %s not found in bank lines", pe.ID, ext)
		}
		return map[string]string{"bank_txn_id": bl.TxnID}, nil
	case ErrorDuplicateVendorPayment:
		if len(pe.TouchedExtIDs) < 2 {
			return nil, fmt.Errorf("planted error %s has no duplicate payment", pe.ID)
		}
		if _, ok := c.bank[pe.TouchedExtIDs[1]]; !ok {
			return nil, fmt.Errorf("planted error %s: duplicate payment %s is not in the bank lines", pe.ID, pe.TouchedExtIDs[1])
		}
		name, err := c.invoice(pe, ext)
		if err != nil {
			return nil, err
		}
		return map[string]string{"invoice": name}, nil
	case ErrorPrepaidNotSpread, ErrorMisclassifiedExpense, ErrorWrongPeriodPosting, ErrorPromptInjection:
		name, err := c.invoice(pe, ext)
		if err != nil {
			return nil, err
		}
		return map[string]string{"invoice": name}, nil
	case ErrorMissingAccrual:
		name := c.opt.Suppliers[pe.SupplierID]
		if pe.SupplierID == "" || name == "" {
			return nil, fmt.Errorf("planted error %s: supplier %q has no ERPNext name", pe.ID, pe.SupplierID)
		}
		return map[string]string{"supplier": name, "month": month}, nil
	case ErrorGSTR2BMissingIn2B:
		if _, err := c.invoice(pe, ext); err != nil {
			return nil, err
		}
		if c.opt.GSTR2B == nil {
			return nil, fmt.Errorf("planted error %s (%s): no GSTR-2B to check", pe.ID, pe.Type)
		}
		if _, ok := c.g2bInv[ext]; ok {
			return nil, fmt.Errorf("planted error %s: ext_id %s is still in the GSTR-2B", pe.ID, ext)
		}
		ev, ok := c.books[ext]
		if !ok || ev.Meta.SupplierGSTIN == "" || ev.InvoiceNo == "" {
			return nil, fmt.Errorf("planted error %s: ext_id %s is not a registered purchase in the books", pe.ID, ext)
		}
		return gstr2bKeys(ev.Meta.SupplierGSTIN, ev.InvoiceNo), nil
	case ErrorGSTR2BAmountMismatch:
		if _, err := c.invoice(pe, ext); err != nil {
			return nil, err
		}
		ref, err := c.inGSTR2B(pe, ext)
		if err != nil {
			return nil, err
		}
		return gstr2bKeys(ref.ctin, ref.inum), nil
	case ErrorGSTR2BMissingInBooks:
		if _, ok := c.opt.ERPMap[ext]; ok {
			return nil, fmt.Errorf("planted error %s: ext_id %s was booked", pe.ID, ext)
		}
		if _, ok := c.books[ext]; ok {
			return nil, fmt.Errorf("planted error %s: ext_id %s is in the books view", pe.ID, ext)
		}
		ref, err := c.inGSTR2B(pe, ext)
		if err != nil {
			return nil, err
		}
		return gstr2bKeys(ref.ctin, ref.inum), nil
	}
	return nil, fmt.Errorf("planted error %s: unknown type %q", pe.ID, pe.Type)
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

	ctx := &truthCtx{opt: opt, bank: byExt, books: map[string]Event{}, g2bInv: map[string]b2bRef{}}
	for _, ev := range pw.BooksWorld.Events {
		ctx.books[ev.ExtID] = ev
	}
	if opt.GSTR2B != nil {
		for _, supp := range opt.GSTR2B.Data.DocData.B2B {
			for _, inv := range supp.Inv {
				ctx.g2bInv[inv.ExtID] = b2bRef{ctin: supp.CTIN, inum: inv.Inum}
			}
		}
	}

	planted := make([]PlantedFinding, 0, len(pw.PlantedErrors))
	for _, pe := range pw.PlantedErrors {
		keys, err := ctx.keys(pe, pw.Month)
		if err != nil {
			return GroundTruth{}, fmt.Errorf("build ground truth %s %s: %w", pw.Company, pw.Month, err)
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
