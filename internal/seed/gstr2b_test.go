package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/money"
)

const goldenGSTR2B = "testdata/evidence/gstr2b-sharma-2026-09-small.json"

// gstr2bFile mirrors the shared-spec example, with amounts kept as the
// literal numbers in the file.
type gstr2bFile struct {
	Data struct {
		GSTIN   string `json:"gstin"`
		RtnPrd  string `json:"rtnprd"`
		DocData struct {
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

func mustGSTR2B(t *testing.T, p Profile, period string, worlds []World, opt GSTR2BOptions) GSTR2B {
	t.Helper()
	g, err := BuildGSTR2B(p, period, worlds, opt)
	if err != nil {
		t.Fatalf("BuildGSTR2B(%s, %s): %v", p.ID, period, err)
	}
	return g
}

// sharmaSep builds sharma's Small 2026-09 return with 2026-08.
func sharmaSep(t *testing.T, opt GSTR2BOptions) (Profile, GSTR2B) {
	t.Helper()
	p := loadCompany(t, "sharma")
	aug := mustGenerate(t, p, "2026-08", Options{Small: true})
	sep := mustGenerate(t, p, "2026-09", Options{Small: true})
	return p, mustGSTR2B(t, p, "2026-09", []World{aug, sep}, opt)
}

func gstr2bBytes(t *testing.T, g GSTR2B) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x", GSTR2BFile)
	if err := WriteGSTR2B(path, g); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGSTR2BCoverage(t *testing.T) {
	p := loadCompany(t, "sharma")
	registered := map[string]bool{}
	for _, s := range p.Suppliers {
		registered[s.ID] = s.IsRegistered()
	}
	for _, small := range []bool{true, false} {
		for _, bp := range []int{DefaultLateShareBP, 3000} {
			t.Run(fmt.Sprintf("small=%v/late=%d", small, bp), func(t *testing.T) {
				opt := GSTR2BOptions{LateShareBP: bp}
				worlds := make([]World, len(testMonths))
				events := map[string]Event{}
				for i, m := range testMonths {
					worlds[i] = mustGenerate(t, p, m, Options{Small: small})
					for _, ev := range worlds[i].Events {
						events[ev.ExtID] = ev
					}
				}
				seen := map[string]int{}
				lateTotal := 0
				for i, m := range testMonths {
					ws := worlds[max(0, i-1) : i+1]
					g := mustGSTR2B(t, p, m, ws, opt)
					lateTotal += len(g.Late)
					for _, id := range g.Included {
						seen[id]++
						ev, ok := events[id]
						if !ok {
							t.Errorf("%s: unknown ExtID %s", m, id)
							continue
						}
						if ev.Kind != EventPurchase {
							t.Errorf("%s: %s is a %s", m, id, ev.Kind)
						}
						if !registered[ev.Meta.SupplierID] || ev.Meta.SupplierGSTIN == "" {
							t.Errorf("%s: %s is from unregistered supplier %s", m, id, ev.Meta.SupplierID)
						}
					}
					for _, id := range g.Late {
						if !slices.Contains(g.Included, id) || !strings.HasPrefix(events[id].Date, testMonths[max(0, i-1)]) || i == 0 {
							t.Errorf("%s: late %s (dated %s) is not a previous-month inclusion", m, id, events[id].Date)
						}
					}
					for _, id := range g.Deferred {
						if slices.Contains(g.Included, id) || !strings.HasPrefix(events[id].Date, m) {
							t.Errorf("%s: deferred %s (dated %s)", m, id, events[id].Date)
						}
					}
				}
				last := testMonths[len(testMonths)-1]
				for _, w := range worlds {
					for _, ev := range w.Events {
						isReg := ev.Kind == EventPurchase && registered[ev.Meta.SupplierID]
						want := 0
						if isReg && (w.Month != last || !opt.IsLate(ev.ExtID)) {
							want = 1
						}
						if seen[ev.ExtID] != want {
							t.Errorf("%s (%s, %s, %s) appears in %d returns, want %d",
								ev.ExtID, ev.Kind, ev.Party, ev.Date, seen[ev.ExtID], want)
						}
					}
				}
				if bp == 3000 && lateTotal == 0 {
					t.Error("no late invoices at 30%")
				}
			})
		}
	}
}

func TestGSTR2BGolden(t *testing.T) {
	_, g := sharmaSep(t, DefaultGSTR2BOptions())
	got := gstr2bBytes(t, g)
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenGSTR2B), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenGSTR2B, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenGSTR2B)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("gstr2b.json differs from %s; run go test ./internal/seed -run TestGSTR2BGolden -update and review the diff", goldenGSTR2B)
	}
}

var (
	twoDecimals = regexp.MustCompile(`^\d+\.\d{2}$`)
	ddmmyyyyRE  = regexp.MustCompile(`^\d{2}-\d{2}-\d{4}$`)
)

func TestGSTR2BShape(t *testing.T) {
	p, g := sharmaSep(t, GSTR2BOptions{LateShareBP: 3000})
	raw := gstr2bBytes(t, g)
	if !bytes.HasSuffix(raw, []byte("}\n")) || !bytes.Contains(raw, []byte("\n  \"data\": {")) {
		t.Error("not 2-space indented with a trailing newline")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var f gstr2bFile
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gstin, err := p.GSTIN()
	if err != nil {
		t.Fatal(err)
	}
	if f.Data.GSTIN != gstin || f.Data.RtnPrd != "092026" {
		t.Errorf("gstin %s rtnprd %s", f.Data.GSTIN, f.Data.RtnPrd)
	}
	state := map[string]string{}
	for _, s := range p.Suppliers {
		state[s.Name] = s.StateCode
	}
	if len(f.Data.DocData.B2B) == 0 {
		t.Fatal("no b2b suppliers")
	}
	n := 0
	prevCTIN := ""
	for _, s := range f.Data.DocData.B2B {
		if err := ValidGSTIN(s.CTIN); err != nil {
			t.Errorf("ctin %s: %v", s.CTIN, err)
		}
		if want, ok := state[s.TrdNm]; !ok || s.CTIN[:2] != want {
			t.Errorf("ctin %s for %q, whose state is %q", s.CTIN, s.TrdNm, want)
		}
		if s.CTIN <= prevCTIN {
			t.Errorf("ctin %s after %s", s.CTIN, prevCTIN)
		}
		prevCTIN = s.CTIN
		prevKey := ""
		for _, inv := range s.Inv {
			n++
			var amt [5]money.Paise
			for i, num := range []json.Number{inv.Val, inv.Txval, inv.IGST, inv.CGST, inv.SGST} {
				if !twoDecimals.MatchString(num.String()) {
					t.Errorf("%s: amount %s doesn't have exactly two decimals", inv.Inum, num)
				}
				v, err := money.FromJSONNumber(num)
				if err != nil {
					t.Fatalf("%s: %v", inv.Inum, err)
				}
				amt[i] = v
			}
			if amt[0] != amt[1]+amt[2]+amt[3]+amt[4] {
				t.Errorf("%s: val %s != txval + taxes", inv.Inum, inv.Val)
			}
			if (amt[2] != 0) == (amt[3]+amt[4] != 0) {
				t.Errorf("%s: igst %s with cgst %s and sgst %s", inv.Inum, inv.IGST, inv.CGST, inv.SGST)
			}
			if !ddmmyyyyRE.MatchString(inv.Dt) {
				t.Errorf("%s: dt %q", inv.Inum, inv.Dt)
			}
			if inv.ItcAvl != "Y" || inv.Inum == "" {
				t.Errorf("invoice %+v", inv)
			}
			key := inv.Dt[6:] + inv.Dt[3:5] + inv.Dt[:2] + "|" + inv.Inum
			if key < prevKey {
				t.Errorf("%s: invoices not sorted by (dt, inum)", inv.Inum)
			}
			prevKey = key
		}
	}
	if n != len(g.Included) {
		t.Errorf("%d invoices in the file, %d included", n, len(g.Included))
	}
	if len(g.Late) == 0 {
		t.Error("expected August late filers at 30%")
	}
}

func TestGSTR2BLateDeterministic(t *testing.T) {
	ids := []string{"EVT-sharma-2026-09-0001", "EVT-sharma-2026-09-0042", "EVT-mehta-2026-10-0100", "x"}
	for i := range 2000 {
		ids = append(ids, fmt.Sprintf("EVT-sharma-2026-%02d-%04d", 1+i%12, i))
	}
	none, all := GSTR2BOptions{LateShareBP: 0}, GSTR2BOptions{LateShareBP: 10000}
	def := DefaultGSTR2BOptions()
	late := 0
	for _, id := range ids {
		if none.IsLate(id) {
			t.Errorf("%s late at 0 bp", id)
		}
		if !all.IsLate(id) {
			t.Errorf("%s on time at 10000 bp", id)
		}
		first := def.IsLate(id)
		for range 3 {
			if def.IsLate(id) != first {
				t.Fatalf("%s changes its mind", id)
			}
		}
		if first {
			late++
		}
	}
	// 5% of 2004 is about 100.
	if late < 50 || late > 160 {
		t.Errorf("%d of %d late at the default share", late, len(ids))
	}

	// End to end: 0 bp defers nothing, 10000 bp defers every invoice.
	_, g0 := sharmaSep(t, none)
	if len(g0.Late) != 0 || len(g0.Deferred) != 0 {
		t.Errorf("0 bp: late %v, deferred %v", g0.Late, g0.Deferred)
	}
	_, g1 := sharmaSep(t, all)
	if len(g1.Included) != len(g1.Late) || len(g1.Deferred) == 0 {
		t.Errorf("10000 bp: %d included, %d late, %d deferred", len(g1.Included), len(g1.Late), len(g1.Deferred))
	}
	for _, id := range g1.Included {
		if !strings.Contains(id, "-2026-08-") {
			t.Errorf("10000 bp: September return holds %s", id)
		}
	}
}

func TestGSTR2BErrors(t *testing.T) {
	p := loadCompany(t, "sharma")
	sep := mustGenerate(t, p, "2026-09", Options{Small: true})
	other := sep
	other.Company = "mehta"
	cases := []struct {
		name   string
		period string
		worlds []World
		opt    GSTR2BOptions
	}{
		{"bad period", "2026-13", []World{sep}, DefaultGSTR2BOptions()},
		{"no period world", "2026-10", []World{sep}, DefaultGSTR2BOptions()},
		{"two worlds for a month", "2026-09", []World{sep, sep}, DefaultGSTR2BOptions()},
		{"other company", "2026-09", []World{other}, DefaultGSTR2BOptions()},
		{"negative share", "2026-09", []World{sep}, GSTR2BOptions{LateShareBP: -1}},
		{"share over 100%", "2026-09", []World{sep}, GSTR2BOptions{LateShareBP: 10001}},
	}
	for _, tc := range cases {
		if _, err := BuildGSTR2B(p, tc.period, tc.worlds, tc.opt); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}

	// An empty return still writes "b2b": [].
	g := mustGSTR2B(t, p, "2026-09", []World{{Company: "sharma", Month: "2026-09"}}, DefaultGSTR2BOptions())
	if !bytes.Contains(gstr2bBytes(t, g), []byte(`"b2b": []`)) {
		t.Error("empty return doesn't write b2b as []")
	}
}
