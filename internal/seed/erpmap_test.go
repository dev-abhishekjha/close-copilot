package seed

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleERPMap() ERPMap {
	return ERPMap{
		"EVT-sharma-2026-09-0010": {DocType: DocPaymentEntry, Name: "ACC-PAY-2026-00001"},
		"EVT-sharma-2026-09-0002": {DocType: DocPurchaseInvoice, Name: "PINV-26-00001"},
		"EVT-sharma-2026-09-0001": {DocType: DocSalesInvoice, Name: "SINV-26-00001"},
	}
}

func TestERPMapRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "suite-skeleton", "sharma-2026-09")
	m := sampleERPMap()
	if err := WriteERPMap(dir, m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ERPMapFile)
	got, err := LoadERPMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(got, m) {
		t.Errorf("loaded %v, wrote %v", got, m)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "EVT-sharma-2026-09-0001": {
    "doctype": "Sales Invoice",
    "name": "SINV-26-00001"
  },
  "EVT-sharma-2026-09-0002": {
    "doctype": "Purchase Invoice",
    "name": "PINV-26-00001"
  },
  "EVT-sharma-2026-09-0010": {
    "doctype": "Payment Entry",
    "name": "ACC-PAY-2026-00001"
  }
}
`
	if string(first) != want {
		t.Errorf("file:\n%s\nwant:\n%s", first, want)
	}
	// Writing what was loaded gives the same bytes, and no temp file stays.
	if err := WriteERPMap(dir, got); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("rewriting the loaded map changed the bytes")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ERPMapFile {
		t.Errorf("dir holds %v", entries)
	}
}

func TestERPMapGoldenLoads(t *testing.T) {
	m, err := LoadERPMap(goldenBooksMap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(goldenBooksMap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Error("the golden map doesn't re-encode to the same bytes")
	}
}

func TestERPMapEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteERPMap(dir, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ERPMapFile))
	if string(b) != "{}\n" {
		t.Errorf("empty map written as %q", b)
	}
	m, err := LoadERPMap(filepath.Join(dir, ERPMapFile))
	if err != nil || len(m) != 0 {
		t.Errorf("load empty: %v, %v", m, err)
	}
}

func TestERPMapRejects(t *testing.T) {
	dir := t.TempDir()
	if err := WriteERPMap(dir, ERPMap{"EVT-1": {DocType: DocSalesInvoice}}); err == nil {
		t.Error("wrote an entry with no name")
	}
	if _, err := os.Stat(filepath.Join(dir, ERPMapFile)); !os.IsNotExist(err) {
		t.Error("a refused map left a file")
	}
	for name, content := range map[string]string{
		"unknown field": `{"EVT-1": {"doctype": "Sales Invoice", "name": "SINV-1", "extra": 1}}`,
		"empty name":    `{"EVT-1": {"doctype": "Sales Invoice", "name": ""}}`,
		"not an object": `null`,
		"trailing":      `{} {}`,
		"bad json":      `{`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadERPMap(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := LoadERPMap(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("loaded a missing file")
	}
}

func TestERPMapDir(t *testing.T) {
	got, err := ERPMapDir("data/out", "suite-skeleton", "sharma", "2026-09")
	if err != nil || got != filepath.Join("data", "out", "suite-skeleton", "sharma-2026-09") {
		t.Errorf("ERPMapDir = %q, %v", got, err)
	}
	for _, parts := range [][3]string{
		{"", "sharma", "2026-09"},
		{"..", "sharma", "2026-09"},
		{"suite/x", "sharma", "2026-09"},
		{"suite-skeleton", `a\b`, "2026-09"},
		{"suite-skeleton", "sharma", ""},
	} {
		if _, err := ERPMapDir("data/out", parts[0], parts[1], parts[2]); err == nil {
			t.Errorf("ERPMapDir accepted %q", parts)
		}
	}
}
