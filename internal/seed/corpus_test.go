package seed

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/corpus/render"
)

const corpusRoot = "../../corpus"

func corpusInputs(t *testing.T) ([]Profile, Rules) {
	t.Helper()
	ps, err := LoadProfiles(filepath.Join("..", "..", "config", "companies"))
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	r, err := LoadRules(filepath.Join("..", "..", "config", "rules.yaml"))
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	return ps, r
}

func readPolicy(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusRoot, "shared", "POL-001-accounting-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ruleValues(r Rules) map[string]int64 {
	return map[string]int64{
		"capitalisation.threshold_inr":  r.Capitalisation.ThresholdINR,
		"prepaid.min_amount_inr":        r.Prepaid.MinAmountINR,
		"variance.pct_threshold":        r.Variance.PctThreshold,
		"variance.abs_threshold_inr":    r.Variance.AbsThresholdINR,
		"bank_match.date_window_days":   int64(r.BankMatch.DateWindowDays),
		"gstr2b.amount_tolerance_paise": r.GSTR2B.AmountTolerancePaise,
		"accruals.lookback_months":      int64(r.Accruals.LookbackMonths),
		"accruals.min_occurrences":      int64(r.Accruals.MinOccurrences),
		"accruals.amount_band_pct":      int64(r.Accruals.AmountBandPct),
	}
}

var ruleMarker = regexp.MustCompile(`<!-- rule:([a-z_.0-9]+)=(-?\d+) -->`)

func policyMarkers(t *testing.T, md string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, m := range ruleMarker.FindAllStringSubmatch(md, -1) {
		v, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		got[m[1]] = v
	}
	return got
}

func TestCorpusPolicyMarkersMatchRules(t *testing.T) {
	_, rules := corpusInputs(t)
	md := readPolicy(t)
	got := policyMarkers(t, md)
	want := ruleValues(rules)
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("policy has no marker for rule %s", k)
		} else if g != v {
			t.Errorf("policy marker %s = %d, rules.yaml has %d", k, g, v)
		}
	}
	// A rule changed in memory must be caught.
	changed := rules
	changed.Capitalisation.ThresholdINR++
	if policyMarkers(t, md)["capitalisation.threshold_inr"] == ruleValues(changed)["capitalisation.threshold_inr"] {
		t.Error("policy comparison did not notice a changed rule")
	}
}

func TestCorpusPolicySections(t *testing.T) {
	md := readPolicy(t)
	for _, h := range []string{
		"## 1. Scope", "## 2. Close calendar", "## 3. Fixed assets", "### 3.1 ",
		"## 4. Prepaid expenses", "### 4.2 ", "## 5. Accruals", "### 5.1 ",
		"## 6. GST and input tax credit", "### 6.1 ", "### 6.2 ",
		"## 7. Bank reconciliation", "### 7.1 ", "### 7.2 ",
		"## 8. Cut-off", "### 8.1 ", "## 9. Journal approvals",
	} {
		if !regexp.MustCompile("(?m)^" + regexp.QuoteMeta(h)).MatchString(md) {
			t.Errorf("policy has no heading %q", h)
		}
	}
	if !strings.Contains(md, "₹50,000") {
		t.Error("section 3.1 does not state ₹50,000")
	}
	for _, bad := range []string{"TODO", "TBD", "lorem", "XXX", "placeholder"} {
		if strings.Contains(strings.ToLower(md), strings.ToLower(bad)) {
			t.Errorf("policy contains %q", bad)
		}
	}
}

var contractMarker = regexp.MustCompile(`<!-- contract:(.*?) -->`)

func markerFields(t *testing.T, s string) map[string]string {
	t.Helper()
	m := contractMarker.FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no contract marker")
	}
	out := map[string]string{}
	for _, kv := range strings.Fields(m[1]) {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func TestCorpusContractsMatchProfiles(t *testing.T) {
	ps, _ := corpusInputs(t)
	total := 0
	for _, p := range ps {
		for _, s := range p.Suppliers {
			total++
			path := filepath.Join(corpusRoot, p.ID, "contracts", "CON-"+s.ID+".md")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("no contract for %s/%s: %v", p.ID, s.ID, err)
				continue
			}
			f := markerFields(t, string(b))
			lo, hi := int64(0), int64(0)
			billing := "monthly"
			if s.IsAnnual() {
				billing, lo, hi = "annual", s.AmountINR.Min, s.AmountINR.Max
			} else {
				lo, hi = s.MonthlyINR.Min, s.MonthlyINR.Max
			}
			rate := 0
			if s.GSTRate != nil {
				rate = *s.GSTRate
			}
			start, end := s.StartMonth, s.EndMonth
			if start == "" {
				start = "none"
			}
			if end == "" {
				end = "none"
			}
			want := map[string]string{
				"supplier": s.ID, "company": p.ID, "billing": billing,
				"amount_min_inr": strconv.FormatInt(lo, 10), "amount_max_inr": strconv.FormatInt(hi, 10),
				"gst_rate": strconv.Itoa(rate), "registered": strconv.FormatBool(s.IsRegistered()),
				"service_start": start, "service_end": end, "renew_month": strconv.Itoa(s.RenewMonth),
			}
			for k, v := range want {
				if f[k] != v {
					t.Errorf("%s: %s = %q, profile says %q", path, k, f[k], v)
				}
			}
			text := string(b)
			if !strings.Contains(text, inr(lo)) || !strings.Contains(text, inr(hi)) {
				t.Errorf("%s does not show amount %s-%s in text", path, inr(lo), inr(hi))
			}
			if !strings.Contains(text, s.Name) {
				t.Errorf("%s does not name the supplier", path)
			}
			if s.IsRegistered() {
				g, err := p.SupplierGSTIN(s)
				if err != nil || !strings.Contains(text, g) {
					t.Errorf("%s does not show the supplier GSTIN %q (%v)", path, g, err)
				}
			}
		}
	}
	if total < 30 {
		t.Errorf("profiles hold %d suppliers, want at least 30", total)
	}
	n := 0
	_ = filepath.WalkDir(corpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "CON-") && strings.HasSuffix(d.Name(), ".md") {
			n++
		}
		return nil
	})
	if n != total {
		t.Errorf("%d contracts on disk, %d suppliers in the profiles", n, total)
	}
}

func TestCorpusNotes(t *testing.T) {
	ps, _ := corpusInputs(t)
	forbidden := regexp.MustCompile(`2026-0[789]|ground.truth|planted`)
	n := 0
	for _, p := range ps {
		for _, m := range CorpusHistoryMonths {
			n++
			path := filepath.Join(corpusRoot, p.ID, "notes", "NOTE-"+p.ID+"-"+m+".md")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("missing note: %v", err)
				continue
			}
			if forbidden.Match(b) {
				t.Errorf("%s mentions a forbidden term", path)
			}
		}
	}
	if n < 6 {
		t.Errorf("%d notes, want at least 6", n)
	}
	_ = filepath.WalkDir(corpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			if b, _ := os.ReadFile(path); forbidden.Match(b) {
				t.Errorf("%s mentions a forbidden term", path)
			}
		}
		return nil
	})
}

func TestCorpusPDFsExist(t *testing.T) {
	policyPages := 0
	_ = filepath.WalkDir(corpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") || strings.Contains(path, "/render/") {
			return nil
		}
		pdf, err := os.ReadFile(strings.TrimSuffix(path, ".md") + ".pdf")
		if err != nil {
			t.Errorf("%s has no PDF: %v", path, err)
			return nil
		}
		if !bytes.HasPrefix(pdf, []byte("%PDF")) || len(pdf) <= 1024 {
			t.Errorf("%s: PDF is not a PDF or is under 1 KB (%d bytes)", path, len(pdf))
		}
		if strings.Contains(path, "POL-001") {
			policyPages = render.Pages(pdf)
		}
		return nil
	})
	if policyPages < 10 || policyPages > 15 {
		t.Errorf("policy PDF has %d pages, want 10 to 15", policyPages)
	}
}

func readAll(t *testing.T, root string) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "render" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".pdf") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			m[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCorpusRegenerationIsByteStable(t *testing.T) {
	ps, rules := corpusInputs(t)
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "shared", "POL-001-accounting-policy.md"), []byte(readPolicy(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateCorpus(ps, rules, out); err != nil {
		t.Fatal(err)
	}
	if err := render.Tree(out); err != nil {
		t.Fatal(err)
	}
	want, have := readAll(t, out), readAll(t, corpusRoot)
	for name, w := range want {
		h, ok := have[name]
		switch {
		case !ok:
			t.Errorf("committed corpus lacks %s", name)
		case !bytes.Equal(h, w):
			t.Errorf("committed %s differs from regenerated output (run go run corpus/gen.go)", name)
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			t.Errorf("committed %s is not generated", name)
		}
	}
	// A second run gives the same bytes.
	again := t.TempDir()
	if err := GenerateCorpus(ps, rules, again); err != nil {
		t.Fatal(err)
	}
	for name, b := range readAll(t, again) {
		if w, ok := want[name]; ok && !bytes.Equal(w, b) {
			t.Errorf("%s differs between two runs", name)
		}
	}
}

func TestCorpusMoney(t *testing.T) {
	for n, want := range map[int64]string{0: "₹0", 999: "₹999", 1000: "₹1,000", 150000: "₹1,50,000", 1234567: "₹12,34,567"} {
		if got := inr(n); got != want {
			t.Errorf("inr(%d) = %q, want %q", n, got, want)
		}
	}
}
