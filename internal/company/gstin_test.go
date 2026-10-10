package company

import (
	"fmt"
	"strings"
	"testing"
)

// stateCodes are every valid GST state code, for generating test GSTINs.
func stateCodes() []string {
	var out []string
	for n := 1; n <= 38; n++ {
		out = append(out, fmt.Sprintf("%02d", n))
	}
	return append(out, "97")
}

func TestCheckDigit(t *testing.T) {
	// Expected digits come from an independent implementation of the
	// standard GSTIN checksum (factors 1, 2, 1, ... from the left).
	tests := []struct {
		prefix string
		want   byte
	}{
		{"29KLMCS0008P1Z", '0'}, // check digit 0
		{"27PQRCM0099T1Z", 'Z'}, // check digit is a letter, the last one
		{"24DEFCB0002H1Z", '7'},
		{"27ZZZCX0001Q1Z", 'K'},
		{"29QWECS4821K1Z", 'D'},
		{"07MNOCM5555B1Z", 'B'},
		{"33ABCCD1234E1Z", 'C'},
		{"97XYZCT9999Z1Z", 'M'},
		{"01AAACA0001A1Z", 'B'},
	}
	for _, tt := range tests {
		got, err := CheckDigit(tt.prefix)
		if err != nil {
			t.Errorf("CheckDigit(%q): %v", tt.prefix, err)
			continue
		}
		if got != tt.want {
			t.Errorf("CheckDigit(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}

	for _, bad := range []string{"", "29KLMCS0008P1", "29KLMCS0008P1ZZ", "29klmcs0008p1z", "29KLMCS0008P1-"} {
		if _, err := CheckDigit(bad); err == nil {
			t.Errorf("CheckDigit(%q): want an error", bad)
		}
	}
}

func TestCheckDigitRoundTrip(t *testing.T) {
	codes := stateCodes()
	for i := range 1000 {
		pan := PAN(int64(i), fmt.Sprintf("key-%d", i), EntityCompany)
		g, err := GSTIN(codes[i%len(codes)], pan)
		if err != nil {
			t.Fatalf("GSTIN(%q, %q): %v", codes[i%len(codes)], pan, err)
		}
		if err := ValidGSTIN(g); err != nil {
			t.Fatalf("ValidGSTIN(%q): %v", g, err)
		}
		b := []byte(g)
		for pos := range b {
			orig := b[pos]
			for c := range []byte(gstChars) {
				if gstChars[c] == orig {
					continue
				}
				b[pos] = gstChars[c]
				if ValidGSTIN(string(b)) == nil {
					t.Fatalf("ValidGSTIN(%q) passed: changed character %d of %q", b, pos+1, g)
				}
			}
			b[pos] = orig
		}
	}
}

func TestGSTINStable(t *testing.T) {
	// Pinned: math/rand/v2's PCG is a specified algorithm, so these must
	// never change; if they do, every seeded supplier GSTIN changes too.
	if got, want := PAN(42, "cloudly", EntityCompany), "ZIPCC8524K"; got != want {
		t.Errorf("PAN(42, cloudly) = %q, want %q", got, want)
	}

	a1, a2 := PAN(42, "cloudly", EntityCompany), PAN(42, "cloudly", EntityCompany)
	if a1 != a2 {
		t.Fatalf("same seed and key gave %q and %q", a1, a2)
	}
	g1, err := GSTIN("27", a1)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := GSTIN("27", a2)
	if err != nil {
		t.Fatal(err)
	}
	if g1 != g2 {
		t.Fatalf("same seed and key gave GSTINs %q and %q", g1, g2)
	}
	if b := PAN(42, "techkart", EntityCompany); b == a1 {
		t.Errorf("different keys gave the same PAN %q", b)
	}
	if c := PAN(43, "cloudly", EntityCompany); c == a1 {
		t.Errorf("different seeds gave the same PAN %q", c)
	}

	seen := map[string]string{}
	for i := range 500 {
		key := fmt.Sprintf("supplier-%d", i)
		pan := PAN(7, key, EntityCompany)
		if err := ValidPAN(pan); err != nil {
			t.Fatalf("PAN(7, %q): %v", key, err)
		}
		if pan[3] != 'C' || pan[4] != 'S' {
			t.Errorf("PAN(7, %q) = %q: want 4th letter C and 5th S", key, pan)
		}
		if prev, dup := seen[pan]; dup {
			t.Errorf("keys %q and %q both gave PAN %q", prev, key, pan)
		}
		seen[pan] = key
		g, err := GSTIN("29", pan)
		if err != nil {
			t.Fatal(err)
		}
		if len(g) != 15 || g[12:14] != "1Z" || g[2:12] != pan || g[:2] != "29" {
			t.Errorf("GSTIN(29, %q) = %q: want 15 characters, 29 + PAN + 1Z + check digit", pan, g)
		}
	}

	// A key that doesn't start with a letter still gives a valid PAN.
	if err := ValidPAN(PAN(1, "9lives", EntityCompany)); err != nil {
		t.Error(err)
	}
	if err := ValidPAN(PAN(1, "", EntityCompany)); err != nil {
		t.Error(err)
	}
}

func TestValidGSTIN(t *testing.T) {
	good, err := GSTIN("29", "KLMCS0008P")
	if err != nil {
		t.Fatal(err)
	}
	if good != "29KLMCS0008P1Z0" {
		t.Fatalf("GSTIN = %q, want 29KLMCS0008P1Z0", good)
	}
	if err := ValidGSTIN(good); err != nil {
		t.Fatalf("ValidGSTIN(%q): %v", good, err)
	}

	tests := []struct {
		name, gstin, want string
	}{
		{"too short", good[:14], "want 15 characters"},
		{"too long", good + "0", "want 15 characters"},
		{"lower-case letter", "29KLMCs0008P1Z0", "upper-case letter"},
		{"lower-case z", "29KLMCS0008P1z0", "must be Z"},
		{"state code 00", "00KLMCS0008P1Z0", "not a GST state code"},
		{"state code 39", "39KLMCS0008P1Z0", "not a GST state code"},
		{"state code 99", "99KLMCS0008P1Z0", "not a GST state code"},
		{"letter in state code", "2AKLMCS0008P1Z0", "not a GST state code"},
		{"digit in PAN letters", "29KLM1S0008P1Z0", "upper-case letter"},
		{"letter in PAN digits", "29KLMCS00O8P1Z0", "must be a digit"},
		{"entity number 0", "29KLMCS0008P0Z0", "character 13"},
		{"bad check digit", "29KLMCS0008P1Z1", "check digit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidGSTIN(tt.gstin)
			if err == nil {
				t.Fatalf("ValidGSTIN(%q) passed", tt.gstin)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidGSTIN(%q) = %v, want it to mention %q", tt.gstin, err, tt.want)
			}
		})
	}

	for _, code := range stateCodes() {
		if !ValidStateCode(code) {
			t.Errorf("ValidStateCode(%q) = false", code)
		}
	}
	for _, code := range []string{"", "0", "00", "39", "96", "98", "99", "1", "001", "AB"} {
		if ValidStateCode(code) {
			t.Errorf("ValidStateCode(%q) = true", code)
		}
	}
}

func TestGSTINRejectsBadInput(t *testing.T) {
	if _, err := GSTIN("40", "KLMCS0008P"); err == nil {
		t.Error("GSTIN with state code 40: want an error")
	}
	if _, err := GSTIN("29", "KLMCS008P"); err == nil {
		t.Error("GSTIN with a 9-character PAN: want an error")
	}
	if _, err := GSTIN("29", "klmcs0008p"); err == nil {
		t.Error("GSTIN with a lower-case PAN: want an error")
	}
}
