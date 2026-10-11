package evidence_test

import (
	"path/filepath"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/seed"
)

var normCases = []struct{ in, want string }{
	{"abc/101", "ABC101"},
	{"ABC-0101", "ABC101"},
	{"ABC 101", "ABC101"},
	{"INV/2026/007", "INV20267"},
	{"000", "0"},
	{"xyz_001.002", "XYZ12"},
	{"CLD/2026/0912", "CLD2026912"},
	{"", ""},
	{"\tabc 007 \t", "ABC7"},
	{"A0B00C", "A0B0C"},
}

func TestNormInvoiceNo(t *testing.T) {
	for _, tc := range normCases {
		t.Run(tc.in, func(t *testing.T) {
			if got := evidence.NormInvoiceNo(tc.in); got != tc.want {
				t.Errorf("NormInvoiceNo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormInvoiceNoParityWithSeed(t *testing.T) {
	inputs := make([]string, 0, len(normCases))
	for _, tc := range normCases {
		inputs = append(inputs, tc.in)
	}
	g := seedGSTR2B(t)
	for _, sup := range g.Data.DocData.B2B {
		for _, inv := range sup.Inv {
			inputs = append(inputs, inv.Inum)
		}
	}
	for _, in := range inputs {
		if got, want := evidence.NormInvoiceNo(in), seed.NormInvoiceNo(in); got != want {
			t.Errorf("NormInvoiceNo(%q) = %q, seed gives %q", in, got, want)
		}
	}
}

// seedGSTR2B builds a small generated sharma return for 2026-09.
func seedGSTR2B(t *testing.T) seed.GSTR2B {
	t.Helper()
	p, err := seed.LoadProfile(filepath.Join("..", "..", "config", "companies", "sharma.yaml"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	aug, err := seed.Generate(p, "2026-08", seed.Options{Small: true})
	if err != nil {
		t.Fatalf("generate 2026-08: %v", err)
	}
	sep, err := seed.Generate(p, "2026-09", seed.Options{Small: true})
	if err != nil {
		t.Fatalf("generate 2026-09: %v", err)
	}
	g, err := seed.BuildGSTR2B(p, "2026-09", []seed.World{aug, sep}, seed.DefaultGSTR2BOptions())
	if err != nil {
		t.Fatalf("BuildGSTR2B: %v", err)
	}
	return g
}
