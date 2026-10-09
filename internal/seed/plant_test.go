package seed

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"
)

func loadTestProfile(t *testing.T, id string) Profile {
	t.Helper()
	p, err := LoadProfile(filepath.Join("..", "..", "config", "companies", id+".yaml"))
	if err != nil {
		t.Fatalf("LoadProfile(%s): %v", id, err)
	}
	return p
}

func TestPlantBankCharge(t *testing.T) {
	p := loadTestProfile(t, "sharma")
	w, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := DefaultSkeletonConfig()
	pw, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors: %v", err)
	}

	if pw.Clean {
		t.Errorf("sharma 2026-09: want clean == false, got true")
	}

	if len(pw.PlantedErrors) != 3 {
		t.Fatalf("sharma 2026-09: want 3 planted errors, got %d", len(pw.PlantedErrors))
	}

	wantIDs := []string{"E01", "E02", "E03"}
	touchedSet := make(map[string]bool)
	for i, pe := range pw.PlantedErrors {
		if pe.ID != wantIDs[i] {
			t.Errorf("planted error %d: got ID %q, want %q", i, pe.ID, wantIDs[i])
		}
		if pe.Type != ErrorUnrecordedBankCharge {
			t.Errorf("planted error %s: got type %q, want %q", pe.ID, pe.Type, ErrorUnrecordedBankCharge)
		}
		if len(pe.TouchedExtIDs) != 1 {
			t.Errorf("planted error %s: want 1 touched ExtID, got %d", pe.ID, len(pe.TouchedExtIDs))
		}
		if pe.AmountPaise <= 0 {
			t.Errorf("planted error %s: amount_paise must be positive, got %d", pe.ID, pe.AmountPaise)
		}
		ext := pe.TouchedExtIDs[0]
		if touchedSet[ext] {
			t.Errorf("planted error %s: duplicate touched ExtID %s", pe.ID, ext)
		}
		touchedSet[ext] = true
	}

	// In BooksWorld: none of the touched bank charges should exist.
	for _, ev := range pw.BooksWorld.Events {
		if touchedSet[ev.ExtID] {
			t.Errorf("BooksWorld contains touched event %s (should have been dropped)", ev.ExtID)
		}
		if ev.Kind == EventBankCharge {
			t.Errorf("BooksWorld contains bank charge %s (should have 0 bank charges)", ev.ExtID)
		}
	}

	// In BankWorld: all touched bank charges must exist.
	bankEventMap := make(map[string]Event)
	for _, ev := range pw.BankWorld.Events {
		bankEventMap[ev.ExtID] = ev
	}
	for ext := range touchedSet {
		ev, ok := bankEventMap[ext]
		if !ok {
			t.Errorf("BankWorld missing touched bank charge event %s", ext)
			continue
		}
		if ev.Kind != EventBankCharge {
			t.Errorf("BankWorld event %s has kind %q, want %q", ext, ev.Kind, EventBankCharge)
		}
	}

	// BankLines must succeed and match BankWorld.ClosingBank.
	lines, err := BankLines(pw.BankWorld)
	if err != nil {
		t.Fatalf("BankLines(BankWorld): %v", err)
	}

	// Each touched bank charge must appear in the bank lines as a withdrawal.
	foundLines := make(map[string]BankLine)
	for _, l := range lines {
		if touchedSet[l.ExtID] {
			foundLines[l.ExtID] = l
			if l.Withdrawal <= 0 || l.Deposit != 0 {
				t.Errorf("bank line for %s: withdrawal must be positive and deposit 0, got W=%d, D=%d",
					l.ExtID, l.Withdrawal, l.Deposit)
			}
		}
	}
	if len(foundLines) != 3 {
		t.Errorf("want 3 bank lines for touched bank charges, found %d", len(foundLines))
	}
}

func TestCleanControlMonth(t *testing.T) {
	p := loadTestProfile(t, "sharma")
	w, err := Generate(p, "2026-08", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := DefaultSkeletonConfig()
	pw, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors: %v", err)
	}

	if !pw.Clean {
		t.Errorf("clean control month 2026-08: want Clean == true, got false")
	}

	if len(pw.PlantedErrors) != 0 {
		t.Errorf("clean control month 2026-08: want 0 planted errors, got %d", len(pw.PlantedErrors))
	}

	if len(pw.BooksWorld.Events) != len(w.Events) {
		t.Errorf("BooksWorld event count %d != original world count %d", len(pw.BooksWorld.Events), len(w.Events))
	}
	if len(pw.BankWorld.Events) != len(w.Events) {
		t.Errorf("BankWorld event count %d != original world count %d", len(pw.BankWorld.Events), len(w.Events))
	}
	if pw.BooksWorld.ClosingBank != w.ClosingBank {
		t.Errorf("BooksWorld closing bank %d != original %d", pw.BooksWorld.ClosingBank, w.ClosingBank)
	}
	if pw.BankWorld.ClosingBank != w.ClosingBank {
		t.Errorf("BankWorld closing bank %d != original %d", pw.BankWorld.ClosingBank, w.ClosingBank)
	}

	// Ground truth for clean control month must be clean with 0 planted findings.
	gt, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw})
	if err != nil {
		t.Fatalf("BuildGroundTruth: %v", err)
	}
	if !gt.Clean {
		t.Errorf("ground truth clean: want true, got false")
	}
	if len(gt.Planted) != 0 {
		t.Errorf("ground truth planted findings: want 0, got %d", len(gt.Planted))
	}
}

func TestReproducibility(t *testing.T) {
	p := loadTestProfile(t, "sharma")
	w, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := DefaultSkeletonConfig()
	pw1, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors (1): %v", err)
	}
	pw2, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors (2): %v", err)
	}

	if !reflect.DeepEqual(pw1, pw2) {
		t.Fatalf("PlantErrors is not deterministic with identical seed and inputs")
	}

	gt1, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw1})
	if err != nil {
		t.Fatalf("BuildGroundTruth (1): %v", err)
	}
	gt2, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw2})
	if err != nil {
		t.Fatalf("BuildGroundTruth (2): %v", err)
	}

	b1, err := gt1.JSON()
	if err != nil {
		t.Fatalf("gt1.JSON: %v", err)
	}
	b2, err := gt2.JSON()
	if err != nil {
		t.Fatalf("gt2.JSON: %v", err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("Ground truth JSON is not byte-identical:\n%s\nvs\n%s", string(b1), string(b2))
	}
}

func TestNeverTwoErrorsOnSameEvent(t *testing.T) {
	p := loadTestProfile(t, "sharma")
	w, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := DefaultSkeletonConfig()
	pw, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors: %v", err)
	}

	seen := make(map[string]string)
	for _, pe := range pw.PlantedErrors {
		for _, ext := range pe.TouchedExtIDs {
			if other, ok := seen[ext]; ok {
				t.Fatalf("event %s touched by both %s and %s", ext, other, pe.ID)
			}
			seen[ext] = pe.ID
		}
	}
}
