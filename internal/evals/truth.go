package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The scorer decodes ground truth with its own types, written against
// evals/schema/ground_truth.schema.json, so it never reuses the seeder's
// code (internal/seed) that plants the errors it grades.

// GroundTruth is one company-month's ground truth file,
// evals/scenarios/<suite>/ground_truth/<company>-<month>.json.
type GroundTruth struct {
	Scenario       string               `json:"scenario"`
	Company        string               `json:"company"`
	Month          string               `json:"month"`
	Clean          bool                 `json:"clean"`
	Planted        []TruthPlanted       `json:"planted"`
	Expected       []TruthExpected      `json:"expected"`
	Investigations []TruthInvestigation `json:"investigations"`
}

// TruthPlanted is one planted error the checks should find.
type TruthPlanted struct {
	ID          string            `json:"id"` // E01, E02, ...
	Type        string            `json:"type"`
	Keys        map[string]string `json:"keys"`
	AmountPaise json.Number       `json:"amount_paise"`
}

// TruthExpected is a genuine, unplanted finding the checks should also
// raise (a late-filed invoice, say); a finding that matches one is correct.
type TruthExpected struct {
	Type string            `json:"type"`
	Keys map[string]string `json:"keys"`
}

// TruthInvestigation is a case the investigator should resolve; an
// unmatched_bank_line finding that matches one surfaced it correctly.
type TruthInvestigation struct {
	ID                 string            `json:"id"` // X01, X02, ...
	Keys               map[string]string `json:"keys"`
	ExpectedResolution string            `json:"expected_resolution"`
}

// truthFile mirrors GroundTruth with pointers, so a required field that is
// absent or null is told apart from a zero value.
type truthFile struct {
	Scenario       *string               `json:"scenario"`
	Company        *string               `json:"company"`
	Month          *string               `json:"month"`
	Clean          *bool                 `json:"clean"`
	Planted        *[]truthPlantedFile   `json:"planted"`
	Expected       *[]truthExpectedFile  `json:"expected"`
	Investigations *[]truthInvestigation `json:"investigations"`
}

type truthPlantedFile struct {
	ID   *string            `json:"id"`
	Type *string            `json:"type"`
	Keys *map[string]string `json:"keys"`
	// AmountPaise is kept raw so a quoted number ("100") is rejected.
	AmountPaise *json.RawMessage `json:"amount_paise"`
}

type truthExpectedFile struct {
	Type *string            `json:"type"`
	Keys *map[string]string `json:"keys"`
}

type truthInvestigation struct {
	ID                 *string            `json:"id"`
	Keys               *map[string]string `json:"keys"`
	ExpectedResolution *string            `json:"expected_resolution"`
}

var (
	plantedIDRe       = regexp.MustCompile(`^E[0-9]{2,}$`)
	investigationIDRe = regexp.MustCompile(`^X[0-9]{2,}$`)
	integerRe         = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	monthRe           = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
)

// ErrTruth reports a ground truth file that is missing or malformed. The
// scorer never treats a month without truth as clean.
var ErrTruth = errors.New("evals: ground truth")

// TruthFileName is the ground truth file of a company-month.
func TruthFileName(company, month string) string { return company + "-" + month + ".json" }

// LoadGroundTruth reads and validates one ground truth file. Decoding is
// strict: unknown fields, missing or null required fields, trailing data,
// a non-integer amount_paise, a company or month that differs from the
// file name, a planted ID that isn't E\d{2,} (or is listed twice), an
// investigation ID that isn't X\d{2,} (or is listed twice), and planted
// errors in a clean month are all errors wrapping ErrTruth.
func LoadGroundTruth(path string) (GroundTruth, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GroundTruth{}, fmt.Errorf("%w: no ground truth file %s", ErrTruth, path)
		}
		return GroundTruth{}, fmt.Errorf("%w: read %s: %w", ErrTruth, path, err)
	}
	gt, err := decodeGroundTruth(data)
	if err != nil {
		return GroundTruth{}, fmt.Errorf("%w: %s: %w", ErrTruth, path, err)
	}
	if want := TruthFileName(gt.Company, gt.Month); filepath.Base(path) != want {
		return GroundTruth{}, fmt.Errorf("%w: %s holds %s %s; its name should be %s", ErrTruth, path, gt.Company, gt.Month, want)
	}
	return gt, nil
}

// decodeGroundTruth decodes and validates the bytes of a ground truth file.
func decodeGroundTruth(data []byte) (GroundTruth, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var f truthFile
	if err := dec.Decode(&f); err != nil {
		return GroundTruth{}, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return GroundTruth{}, errors.New("trailing data after the JSON object")
	}

	var errs []error
	missing := func(field string) { errs = append(errs, fmt.Errorf("%s is missing or null", field)) }
	var gt GroundTruth
	if f.Scenario == nil {
		missing("scenario")
	} else {
		gt.Scenario = *f.Scenario
	}
	if f.Company == nil || *f.Company == "" {
		missing("company")
	} else {
		gt.Company = *f.Company
	}
	if f.Month == nil {
		missing("month")
	} else if !monthRe.MatchString(*f.Month) {
		errs = append(errs, fmt.Errorf("month %.20q is not YYYY-MM", *f.Month))
	} else {
		gt.Month = *f.Month
	}
	if f.Clean == nil {
		missing("clean")
	} else {
		gt.Clean = *f.Clean
	}

	if f.Planted == nil {
		missing("planted")
	} else {
		gt.Planted = make([]TruthPlanted, 0, len(*f.Planted))
		seen := map[string]bool{}
		for i, p := range *f.Planted {
			field := fmt.Sprintf("planted[%d]", i)
			switch {
			case p.ID == nil:
				missing(field + ".id")
				continue
			case !plantedIDRe.MatchString(*p.ID):
				errs = append(errs, fmt.Errorf("%s.id %.20q is not E followed by two or more digits", field, *p.ID))
				continue
			case seen[*p.ID]:
				errs = append(errs, fmt.Errorf("%s.id %s is listed twice", field, *p.ID))
				continue
			}
			seen[*p.ID] = true
			if p.Type == nil || *p.Type == "" {
				missing(field + ".type")
				continue
			}
			if p.Keys == nil {
				missing(field + ".keys")
				continue
			}
			if p.AmountPaise == nil || string(*p.AmountPaise) == "null" {
				missing(field + ".amount_paise")
				continue
			}
			amount := string(*p.AmountPaise)
			if !integerRe.MatchString(amount) {
				errs = append(errs, fmt.Errorf("%s.amount_paise %.30q is not an integer", field, amount))
				continue
			}
			if _, err := strconv.ParseInt(amount, 10, 64); err != nil {
				errs = append(errs, fmt.Errorf("%s.amount_paise %.30q is not an integer: %w", field, amount, err))
				continue
			}
			gt.Planted = append(gt.Planted, TruthPlanted{ID: *p.ID, Type: *p.Type, Keys: *p.Keys, AmountPaise: json.Number(amount)})
		}
	}

	if f.Expected == nil {
		missing("expected")
	} else {
		gt.Expected = make([]TruthExpected, 0, len(*f.Expected))
		for i, e := range *f.Expected {
			field := fmt.Sprintf("expected[%d]", i)
			if e.Type == nil || *e.Type == "" {
				missing(field + ".type")
				continue
			}
			if e.Keys == nil {
				missing(field + ".keys")
				continue
			}
			gt.Expected = append(gt.Expected, TruthExpected{Type: *e.Type, Keys: *e.Keys})
		}
	}

	if f.Investigations == nil {
		missing("investigations")
	} else {
		gt.Investigations = make([]TruthInvestigation, 0, len(*f.Investigations))
		seen := map[string]bool{}
		for i, x := range *f.Investigations {
			field := fmt.Sprintf("investigations[%d]", i)
			switch {
			case x.ID == nil:
				missing(field + ".id")
				continue
			case !investigationIDRe.MatchString(*x.ID):
				errs = append(errs, fmt.Errorf("%s.id %.20q is not X followed by two or more digits", field, *x.ID))
				continue
			case seen[*x.ID]:
				errs = append(errs, fmt.Errorf("%s.id %s is listed twice", field, *x.ID))
				continue
			}
			seen[*x.ID] = true
			if x.Keys == nil {
				missing(field + ".keys")
				continue
			}
			if x.ExpectedResolution == nil {
				missing(field + ".expected_resolution")
				continue
			}
			gt.Investigations = append(gt.Investigations, TruthInvestigation{ID: *x.ID, Keys: *x.Keys, ExpectedResolution: *x.ExpectedResolution})
		}
	}

	errs = append(errs, overlappingTruth(gt)...)
	if gt.Clean && f.Planted != nil && len(*f.Planted) > 0 {
		errs = append(errs, fmt.Errorf("clean is true but %d errors are planted", len(*f.Planted)))
	}
	if err := errors.Join(errs...); err != nil {
		return GroundTruth{}, err
	}
	return gt, nil
}

// overlappingTruth rejects two matchable truth items (planted, expected or
// investigation, across the three lists) of the same mapped type whose
// keys are equal or one a subset of the other: then two findings that
// each match both would count as two correct findings instead of one
// correct and one duplicate, inflating precision. Items that can never
// match (no keys, or a key with an empty value) are left to scoring,
// which reports them unmatchable.
func overlappingTruth(gt GroundTruth) []error {
	type item struct {
		name string
		t    target
	}
	var items []item
	for i, p := range gt.Planted {
		items = append(items, item{fmt.Sprintf("planted[%d] (%s)", i, p.ID), target{typ: MapPlantedType(p.Type), keys: p.Keys}})
	}
	for i, e := range gt.Expected {
		items = append(items, item{fmt.Sprintf("expected[%d]", i), target{typ: MapPlantedType(e.Type), keys: e.Keys}})
	}
	for i, x := range gt.Investigations {
		items = append(items, item{fmt.Sprintf("investigations[%d] (%s)", i, x.ID), target{typ: typeUnmatchedBank, keys: x.Keys}})
	}
	var errs []error
	for i := range items {
		a := items[i]
		if !a.t.matchable() {
			continue
		}
		for _, b := range items[i+1:] {
			if !b.t.matchable() || a.t.typ != b.t.typ {
				continue
			}
			if keysWithin(a.t.keys, b.t.keys) || keysWithin(b.t.keys, a.t.keys) {
				errs = append(errs, fmt.Errorf("%s and %s are both %s with overlapping keys %s and %s: one finding could count for both",
					a.name, b.name, a.t.typ, canonicalKeys(a.t.keys), canonicalKeys(b.t.keys)))
			}
		}
	}
	return errs
}

// keysWithin reports whether every key of a is in b with an equal value
// after trimming spaces.
func keysWithin(a, b map[string]string) bool {
	for k, v := range a {
		bv, ok := b[k]
		if !ok || strings.TrimSpace(bv) != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}
