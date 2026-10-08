package money

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParseRupees(t *testing.T) {
	cases := []struct {
		in   string
		want Paise
	}{
		{"0", 0},
		{"-0", 0},
		{"1", 100},
		{"1180", 118000},
		{"1180.5", 118050},
		{"1180.00", 118000},
		{"1,180.00", 118000},
		{"-1,180.00", -118000},
		{"0.05", 5},
		{"-0.05", -5},
		{"007.10", 710},
		// Rounding: half away from zero on the first dropped digit.
		{"0.005", 1},
		{"-0.005", -1},
		{"0.004", 0},
		{"-0.004", 0},
		{"0.0049999", 0},
		{"0.015", 2},
		{"-0.015", -2},
		{"2.675", 268},
		{"0.0000000000000000000000000005", 0},
		{"0.995", 100},
		{"-0.995", -100},
		{"1.23456789", 123},
		// Over one crore, in Indian and western grouping.
		{"12,34,56,789.01", 123456789_01},
		{"123,456,789.01", 123456789_01},
		{"1,23,456.78", 12345678},
		{"1,00,00,000", 1_00_00_000_00},
		{"1,000", 100000},
		{"12,345", 1234500},
		{"10,00,000", 1000000_00},
		// The int64 limits.
		{"92233720368547758.07", math.MaxInt64},
		{"-92233720368547758.08", math.MinInt64},
		{"92,233,720,368,547,758.07", math.MaxInt64},
		{"92,23,37,20,36,85,47,758.07", math.MaxInt64},
		{"92233720368547758.0749", math.MaxInt64},
		{"-92233720368547758.0849", math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRupees(tc.in)
			if err != nil {
				t.Fatalf("ParseRupees(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRupees(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRupeesRejects(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"", ErrSyntax},
		{"-", ErrSyntax},
		{"abc", ErrSyntax},
		{"12a", ErrSyntax},
		{"1e3", ErrSyntax},
		{"₹100", ErrSyntax},
		{" 100", ErrSyntax},
		{"100 ", ErrSyntax},
		{"1.2.3", ErrSyntax},
		{"1..2", ErrSyntax},
		{"1.", ErrSyntax},
		{".5", ErrSyntax},
		{"+1", ErrSyntax},
		{"--1", ErrSyntax},
		{"1-", ErrSyntax},
		{"1.-5", ErrSyntax},
		{"-,1", ErrSyntax},
		{",", ErrSyntax},
		{",100", ErrSyntax},
		{"100,", ErrSyntax},
		{"1,,000", ErrSyntax},
		{"1,00", ErrSyntax},
		{"1,0000", ErrSyntax},
		{"1234,567", ErrSyntax},
		{"1,23,4567", ErrSyntax},
		{"12,345,67,890", ErrSyntax},
		{"1.000,50", ErrSyntax},
		{"1,000.5,0", ErrSyntax},
		{"92233720368547758.08", ErrOverflow},
		{"-92233720368547758.09", ErrOverflow},
		{"92233720368547758.075", ErrOverflow},
		{"-92233720368547758.085", ErrOverflow},
		{"100000000000000000000", ErrOverflow},
		{"-99999999999999999999999999.99", ErrOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRupees(tc.in)
			if err == nil {
				t.Fatalf("ParseRupees(%q) = %d, want an error", tc.in, got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("ParseRupees(%q) error %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestFromJSONNumber(t *testing.T) {
	cases := []struct {
		in   json.Number
		want Paise
	}{
		{"0", 0},
		{"-0", 0},
		{"0.0", 0},
		{"1180", 118000},
		{"1180.5", 118050},
		{"-1180.5", -118050},
		{"123456789.01", 123456789_01},
		{"0.005", 1},
		{"-0.005", -1},
		{"0.004", 0},
		// Exponent forms from Python floats.
		{"1e-05", 0},
		{"1e-2", 1},
		{"5e-3", 1},
		{"-5e-3", -1},
		{"4.9e-3", 0},
		{"1.18E+3", 118000},
		{"1.18e3", 118000},
		{"-2.5e2", -25000},
		{"2.5E-1", 25},
		{"1e0", 100},
		{"1e+00", 100},
		{"0e999999999", 0},
		{"0.000e5", 0},
		{"1e-999999999999999999999", 0},
		{"9.2233720368547758e16", 9223372036854775800},
		{"9.22337203685477580749e16", math.MaxInt64}, {"92233720368547758.07", math.MaxInt64},
		{"-92233720368547758.08", math.MinInt64},
		{"-9.223372036854775808E16", math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			got, err := FromJSONNumber(tc.in)
			if err != nil {
				t.Fatalf("FromJSONNumber(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("FromJSONNumber(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromJSONNumberRejects(t *testing.T) {
	cases := []struct {
		in   json.Number
		want error
	}{
		{"", ErrSyntax},
		{"-", ErrSyntax},
		{"abc", ErrSyntax},
		{"1,180.00", ErrSyntax},
		{"1.", ErrSyntax},
		{".5", ErrSyntax},
		{"1.2.3", ErrSyntax},
		{"+1", ErrSyntax},
		{"--1", ErrSyntax},
		{"1e", ErrSyntax},
		{"1e+", ErrSyntax},
		{"1e-", ErrSyntax},
		{"1e1.5", ErrSyntax},
		{"1ee2", ErrSyntax},
		{"e5", ErrSyntax},
		{"1.e5", ErrSyntax},
		{"NaN", ErrSyntax},
		{"Infinity", ErrSyntax},
		{"9.22337203685477580849e16", ErrOverflow},
		{"1e17", ErrOverflow},
		{"-1e17", ErrOverflow},
		{"1e1001", ErrOverflow},
		{"1e99999999999999999999", ErrOverflow},
		{"92233720368547758.08", ErrOverflow},
		{"-92233720368547758.09", ErrOverflow},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			got, err := FromJSONNumber(tc.in)
			if err == nil {
				t.Fatalf("FromJSONNumber(%q) = %d, want an error", tc.in, got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("FromJSONNumber(%q) error %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestRupees(t *testing.T) {
	cases := []struct {
		in   Paise
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{-5, "-0.05"},
		{-99, "-0.99"},
		{100, "1.00"},
		{118000, "1180.00"},
		{118050, "1180.50"},
		{-118050, "-1180.50"},
		{123456789_01, "123456789.01"},
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
		{math.MinInt64 + 1, "-92233720368547758.07"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.in.Rupees(); got != tc.want {
				t.Errorf("Paise(%d).Rupees() = %q, want %q", int64(tc.in), got, tc.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		in   Paise
		want string
	}{
		{0, "₹0.00"},
		{1, "₹0.01"},
		{-1, "-₹0.01"},
		{100, "₹1.00"},
		{99999, "₹999.99"},
		{100000, "₹1,000.00"},
		{118000, "₹1,180.00"},
		{1234500, "₹12,345.00"},
		{12345678, "₹1,23,456.78"},
		{-12345678, "-₹1,23,456.78"},
		{123456780, "₹12,34,567.80"}, // 1234567.8 rupees
		{1_00_00_000_00, "₹1,00,00,000.00"},
		{123456789_01, "₹12,34,56,789.01"},
		{1234567890_12, "₹1,23,45,67,890.12"},
		{math.MaxInt64, "₹92,23,37,20,36,85,47,758.07"},
		{math.MinInt64, "-₹92,23,37,20,36,85,47,758.08"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.in.Format(); got != tc.want {
				t.Errorf("Paise(%d).Format() = %q, want %q", int64(tc.in), got, tc.want)
			}
		})
	}
}

// TestFormatParses checks that Format's grouping is one ParseRupees reads.
func TestFormatParses(t *testing.T) {
	for _, p := range spread() {
		s := strings.Replace(p.Format(), "₹", "", 1)
		got, err := ParseRupees(s)
		if err != nil {
			t.Errorf("ParseRupees(%q) from Format: %v", s, err)
			continue
		}
		if got != p {
			t.Errorf("ParseRupees(%q) = %d, want %d", s, got, p)
		}
	}
}

func TestRupeesRoundTrip(t *testing.T) {
	for _, p := range spread() {
		s := p.Rupees()
		got, err := ParseRupees(s)
		if err != nil {
			t.Errorf("ParseRupees(%q): %v", s, err)
			continue
		}
		if got != p {
			t.Errorf("ParseRupees(Paise(%d).Rupees() = %q) = %d", int64(p), s, got)
		}
		gotJSON, err := FromJSONNumber(json.Number(s))
		if err != nil || gotJSON != p {
			t.Errorf("FromJSONNumber(%q) = %d, %v; want %d", s, gotJSON, err, p)
		}
	}
}

// spread is a set of amounts covering both signs, every digit count and
// the int64 limits.
func spread() []Paise {
	out := []Paise{0, 1, -1, 5, -5, 99, -99, 100, -100, 101, 118050, -118050,
		math.MaxInt64, math.MinInt64, math.MinInt64 + 1, math.MaxInt64 - 1}
	v := int64(7)
	for range 18 {
		out = append(out, Paise(v), Paise(-v))
		v = v*10 + 3
	}
	return out
}
