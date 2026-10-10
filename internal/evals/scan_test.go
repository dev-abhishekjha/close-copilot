package evals

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// seededProfiles loads config/companies, the allowlist's source.
func seededProfiles(t *testing.T) []company.Profile {
	t.Helper()
	ps, err := company.LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("no company profiles")
	}
	return ps
}

// scanOne scans a folder holding one file with body.
func scanOne(t *testing.T, body string, profiles []company.Profile, extra []string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sharma-2026-09"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sharma-2026-09", "list_bank_lines-0123456789abcdef.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ScanFixtures(t.Context(), dir, profiles, extra)
}

func TestScanFixtures(t *testing.T) {
	profiles := seededProfiles(t)
	p := profiles[0]
	seededGSTIN, err := p.GSTIN()
	if err != nil {
		t.Fatal(err)
	}
	var supplierPAN, supplierGSTIN string
	for _, s := range p.Suppliers {
		if g, err := p.SupplierGSTIN(s); err == nil {
			supplierPAN, supplierGSTIN = p.SupplierPAN(s), g
			break
		}
	}
	if supplierGSTIN == "" {
		t.Fatalf("company %s has no registered supplier", p.ID)
	}

	t.Run("seeded identifiers pass", func(t *testing.T) {
		body := `{"result":[{"company_gstin":"` + seededGSTIN + `","pan":"` + p.PAN + `","supplier_gstin":"` + supplierGSTIN +
			`","supplier_pan":"` + supplierPAN + `","narration":"NEFT CHARGES INCL GST","txn_id":"HDFC-20260915-C1"}]}`
		if err := scanOne(t, body, profiles, nil); err != nil {
			t.Errorf("seeded values: %v", err)
		}
	})

	// A checksum-valid GSTIN built on a PAN the seeder never derived.
	otherPAN := "ZZZCZ9999Z"
	otherGSTIN, err := company.GSTIN("27", otherPAN)
	if err != nil {
		t.Fatal(err)
	}
	if err := company.ValidGSTIN(otherGSTIN); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"anthropic key":          `{"note":"sk-ant-api03-abcdef"}`,
		"bearer":                 `{"h":"Authorization: Bearer abcdefgh12345678"}`,
		"token=":                 `{"u":"https://x/?token=abc"}`,
		"token value":            `{"note":"token abcdefgh12345678"}`,
		"api_key":                `{"api_key":"x"}`,
		"apikey":                 `{"ApiKey":"x"}`,
		"password":               `{"password":"x"}`,
		"dsn":                    `{"db":"postgres://user:pass@db:5432/c"}`,
		"aws":                    `{"k":"AKIAABCDEFGHIJKLMNOP"}`,
		"pem":                    `{"k":"-----BEGIN PRIVATE KEY-----"}`,
		"foreign GSTIN":          `{"supplier_gstin":"` + otherGSTIN + `"}`,
		"foreign PAN":            `{"pan":"` + otherPAN + `"}`,
		"GSTIN with another PAN": `{"g":"` + seededGSTIN[:2] + otherPAN + seededGSTIN[12:] + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := scanOne(t, body, profiles, nil)
			if !errors.Is(err, ErrFixtureLeak) {
				t.Fatalf("scan = %v", err)
			}
			if !strings.Contains(err.Error(), "sharma-2026-09") || !strings.Contains(err.Error(), ":1:") {
				t.Errorf("error does not name the file and line: %v", err)
			}
			for _, secret := range []string{"abcdefgh12345678", "user:pass", "AKIAABCDEFGHIJKLMNOP", otherGSTIN, otherPAN} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error echoes %q: %v", secret, err)
				}
			}
		})
	}

	t.Run("extra allowlist", func(t *testing.T) {
		body := `{"g":"` + otherGSTIN + `","p":"` + otherPAN + `"}`
		if err := scanOne(t, body, profiles, []string{otherGSTIN}); err != nil {
			t.Errorf("extra GSTIN (and its PAN): %v", err)
		}
		if err := scanOne(t, body, nil, nil); err == nil {
			t.Error("no allowlist passed a GSTIN")
		}
	})

	t.Run("PAN boundaries", func(t *testing.T) {
		// A non-entity 4th letter is not a PAN; letters glued on are not either.
		if err := scanOne(t, `{"x":"ABCDE1234F","y":"XABCPE1234FX"}`, profiles, nil); err != nil {
			t.Errorf("non-PAN text flagged: %v", err)
		}
	})

	t.Run("the committed fixtures", func(t *testing.T) {
		root := filepath.Join("..", "..", "evals", "fixtures")
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			t.Skip("no evals/fixtures yet")
		}
		if err := ScanFixtures(t.Context(), root, profiles, nil); err != nil {
			t.Error(err)
		}
	})
}
