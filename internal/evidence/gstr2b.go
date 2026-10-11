package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// GSTR2BLoadResult summarizes the outcome of loading a GSTR-2B file.
type GSTR2BLoadResult struct {
	Company    string `json:"company"`
	Month      string `json:"month"`
	SourceFile string `json:"source_file"`
	Total      int    `json:"total"`
	Inserted   int    `json:"inserted"`
	Updated    int    `json:"updated"`
}

type gstr2bFile struct {
	Data *struct {
		GSTIN   string `json:"gstin"`
		RtnPrd  string `json:"rtnprd"`
		DocData *struct {
			B2B []struct {
				CTIN  string `json:"ctin"`
				TrdNm string `json:"trdnm"`
				Inv   []struct {
					Inum   string      `json:"inum"`
					Dt     string      `json:"dt"`
					Val    json.Number `json:"val"`
					Txval  json.Number `json:"txval"`
					IGST   json.Number `json:"igst"`
					CGST   json.Number `json:"cgst"`
					SGST   json.Number `json:"sgst"`
					ItcAvl string      `json:"itcavl"`
				} `json:"inv"`
			} `json:"b2b"`
		} `json:"docdata"`
	} `json:"data"`
}

// ParseGSTR2B reads a simplified GSTR-2B (B2B section) and returns one store
// row per invoice. The period comes from rtnprd, which must equal month,
// never from the invoice date. All row errors are returned joined.
func ParseGSTR2B(r io.Reader, companyID, month string) ([]store.GSTR2BEntry, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var f gstr2bFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("gstr2b: decode json: %w", err)
	}
	if f.Data == nil || f.Data.DocData == nil {
		return nil, errors.New("gstr2b: missing data.docdata")
	}

	rt, err := time.Parse("012006", f.Data.RtnPrd)
	if err != nil {
		return nil, fmt.Errorf("gstr2b: rtnprd %q is not MMYYYY: %w", f.Data.RtnPrd, err)
	}
	if period := rt.Format("2006-01"); period != month {
		return nil, fmt.Errorf("gstr2b: rtnprd %q is period %s, want %s", f.Data.RtnPrd, period, month)
	}

	var (
		out     []store.GSTR2BEntry
		rowErrs []error
		seen    = map[string]bool{}
	)
	for si, sup := range f.Data.DocData.B2B {
		if strings.TrimSpace(sup.CTIN) == "" {
			rowErrs = append(rowErrs, fmt.Errorf("supplier %d: ctin is empty", si+1))
			continue
		}
		for ii, inv := range sup.Inv {
			pos := fmt.Sprintf("supplier %d (%s) invoice %d", si+1, sup.CTIN, ii+1)
			if strings.TrimSpace(inv.Inum) == "" {
				rowErrs = append(rowErrs, fmt.Errorf("%s: inum is empty", pos))
				continue
			}
			var rowErr []error
			dt, err := time.Parse("02-01-2006", inv.Dt)
			if err != nil {
				rowErr = append(rowErr, fmt.Errorf("dt %q is not dd-mm-yyyy", inv.Dt))
			}
			var itc bool
			switch inv.ItcAvl {
			case "Y":
				itc = true
			case "N":
			default:
				rowErr = append(rowErr, fmt.Errorf("itcavl %q is not Y or N", inv.ItcAvl))
			}
			amt := func(name string, n json.Number) money.Paise {
				p, err := money.FromJSONNumber(n)
				if err != nil {
					rowErr = append(rowErr, fmt.Errorf("%s: %w", name, err))
				}
				return p
			}
			txval := amt("txval", inv.Txval)
			igst := amt("igst", inv.IGST)
			cgst := amt("cgst", inv.CGST)
			sgst := amt("sgst", inv.SGST)

			norm := NormInvoiceNo(inv.Inum)
			key := sup.CTIN + "\x00" + norm
			if seen[key] {
				rowErr = append(rowErr, fmt.Errorf("duplicate normalised invoice number %q for this supplier", norm))
			}
			seen[key] = true

			if len(rowErr) > 0 {
				rowErrs = append(rowErrs, fmt.Errorf("%s: %w", pos, errors.Join(rowErr...)))
				continue
			}

			e := store.GSTR2BEntry{
				CompanyID:     companyID,
				Period:        month,
				SupplierGSTIN: sup.CTIN,
				InvoiceNo:     inv.Inum,
				InvoiceNoNorm: norm,
				InvoiceDate:   dt.UTC(),
				TaxablePaise:  txval,
				IGSTPaise:     igst,
				CGSTPaise:     cgst,
				SGSTPaise:     sgst,
				ITCAvailable:  itc,
			}
			if sup.TrdNm != "" {
				name := sup.TrdNm
				e.SupplierName = &name
			}
			out = append(out, e)
		}
	}

	if len(rowErrs) > 0 {
		return nil, fmt.Errorf("gstr2b validation failed with %d error(s):\n%w", len(rowErrs), errors.Join(rowErrs...))
	}
	return out, nil
}

// LoadGSTR2B parses a GSTR-2B and upserts its rows in one transaction.
func LoadGSTR2B(ctx context.Context, st *store.Store, companyID, month string, r io.Reader, sourceFile string) (GSTR2BLoadResult, error) {
	res := GSTR2BLoadResult{Company: companyID, Month: month, SourceFile: sourceFile}
	entries, err := ParseGSTR2B(r, companyID, month)
	if err != nil {
		return res, err
	}
	res.Total = len(entries)
	if len(entries) == 0 {
		return res, nil
	}

	var inserted, updated int
	for _, e := range entries {
		_, err := st.GetGSTR2BEntry(ctx, companyID, e.Period, e.SupplierGSTIN, e.InvoiceNoNorm)
		switch {
		case err == nil:
			updated++
		case errors.Is(err, store.ErrNotFound):
			inserted++
		default:
			return res, fmt.Errorf("check existing gstr2b entry: %w", err)
		}
	}
	if err := st.UpsertGSTR2BEntries(ctx, entries); err != nil {
		return res, fmt.Errorf("upsert gstr2b entries: %w", err)
	}
	res.Inserted = inserted
	res.Updated = updated
	return res, nil
}

// LoadGSTR2BFile reads a gstr2b.json file from disk and loads it.
func LoadGSTR2BFile(ctx context.Context, st *store.Store, companyID, month, filePath string) (GSTR2BLoadResult, error) {
	f, err := os.Open(filePath) //nolint:gosec // filePath is the user or caller supplied path
	if err != nil {
		return GSTR2BLoadResult{Company: companyID, Month: month, SourceFile: filepath.Base(filePath)},
			fmt.Errorf("open gstr2b %s: %w", filePath, err)
	}
	defer f.Close()
	return LoadGSTR2B(ctx, st, companyID, month, f, filepath.Base(filePath))
}
