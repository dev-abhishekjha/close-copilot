package evals

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// ErrFixtureLeak is a fixture file holding a secret-looking string or a
// GSTIN or PAN outside the synthetic allowlist.
var ErrFixtureLeak = errors.New("evals: fixture scan failed")

// secretPatterns are strings that look like credentials. Fixtures are
// read-only tool responses over synthetic data, so none should appear.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"anthropic key (sk-ant-)", regexp.MustCompile(`sk-ant-`)},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"token assignment (token=/token <value>)", regexp.MustCompile(`(?i)\btoken(=|\s+[A-Za-z0-9._~+/=-]{8,})`)},
	{"api_key", regexp.MustCompile(`(?i)api[_-]?key`)},
	{"password", regexp.MustCompile(`(?i)passw(or)?d`)},
	{"URL with embedded credentials", regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/@:"']+:[^\s/@"']*@`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`)},
	{"PEM block (-----BEGIN)", regexp.MustCompile(`-----BEGIN`)},
}

var (
	// gstinPatternRe matches the GSTIN shape: state code, PAN, entity
	// number, Z, check character.
	gstinPatternRe = regexp.MustCompile(`\b[0-9]{2}[A-Z]{3}[ABCFGHJLPT][A-Z][0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`)
	// panPatternRe matches a standalone PAN: three letters, the entity
	// letter (4th), a letter, four digits, a letter. A PAN inside a GSTIN
	// has no word boundary before it, so it is checked as the GSTIN.
	panPatternRe = regexp.MustCompile(`\b[A-Z]{3}[ABCFGHJLPT][A-Z][0-9]{4}[A-Z]\b`)
)

// maxScanReports bounds the problems one scan lists.
const maxScanReports = 50

// identifierAllowlist is the synthetic GSTINs and PANs the seeder derives
// from config/companies (Profile.GSTIN, SupplierPAN, SupplierGSTIN), plus
// extra. An extra GSTIN allows its PAN too.
func identifierAllowlist(profiles []company.Profile, extra []string) (map[string]bool, map[string]bool, error) {
	gstins, pans := map[string]bool{}, map[string]bool{}
	add := func(v string) {
		v = strings.ToUpper(strings.TrimSpace(v))
		switch len(v) {
		case 15:
			gstins[v] = true
			pans[v[2:12]] = true
		case 10:
			pans[v] = true
		}
	}
	for _, p := range profiles {
		add(p.PAN)
		g, err := p.GSTIN()
		if err != nil {
			return nil, nil, fmt.Errorf("evals: scan allowlist: company %s: %w", p.ID, err)
		}
		add(g)
		for _, s := range p.Suppliers {
			add(p.SupplierPAN(s))
			sg, err := p.SupplierGSTIN(s)
			switch {
			case errors.Is(err, company.ErrUnregistered):
			case err != nil:
				return nil, nil, fmt.Errorf("evals: scan allowlist: company %s supplier %s: %w", p.ID, s.ID, err)
			default:
				add(sg)
			}
		}
	}
	for _, v := range extra {
		add(v)
	}
	return gstins, pans, nil
}

// maskID shows the first four characters of an identifier.
func maskID(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return v[:4] + strings.Repeat("*", len(v)-4)
}

// ScanFixtures scans every file under dir and fails (ErrFixtureLeak) on a
// secret-looking string or on any GSTIN or PAN that isn't in the
// synthetic allowlist derived from profiles, plus extra. Only tests pass
// extra (the fake ERPNext's identifiers); the CLI passes none. The error
// names each file, line and pattern; identifiers are masked, secrets are
// never echoed.
func ScanFixtures(ctx context.Context, dir string, profiles []company.Profile, extra []string) error {
	gstins, pans, err := identifierAllowlist(profiles, extra)
	if err != nil {
		return err
	}
	var problems []string
	report := func(format string, a ...any) {
		if len(problems) < maxScanReports {
			problems = append(problems, fmt.Sprintf(format, a...))
		} else if len(problems) == maxScanReports {
			problems = append(problems, "...")
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("evals: scan %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			report("%s: not a regular file", p)
			return nil
		}
		data, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			for _, sp := range secretPatterns {
				if sp.re.MatchString(line) {
					report("%s:%d: secret-looking string: %s", p, n, sp.name)
				}
			}
			for _, g := range gstinPatternRe.FindAllString(line, -1) {
				if !gstins[g] {
					report("%s:%d: GSTIN %s is not a seeded synthetic GSTIN", p, n, maskID(g))
				}
			}
			for _, v := range panPatternRe.FindAllString(line, -1) {
				if !pans[v] {
					report("%s:%d: PAN %s is not a seeded synthetic PAN", p, n, maskID(v))
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		return fmt.Errorf("evals: scan %s: %w", dir, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s:\n  %s", ErrFixtureLeak, dir, strings.Join(problems, "\n  "))
	}
	return nil
}
