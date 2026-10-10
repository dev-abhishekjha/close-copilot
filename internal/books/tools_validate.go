package books

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidInput is wrapped by every tool-input validation error. The
// message names the field and the rule so the model can correct its call.
var ErrInvalidInput = errors.New("invalid input")

// inputError is a tool-input validation failure. It is the one kind of
// error whose own text reaches the model (see toModelError).
type inputError struct {
	field, rule string
}

func (e *inputError) Error() string {
	return fmt.Sprintf("%s: %s %s", ErrInvalidInput, e.field, e.rule)
}

func (e *inputError) Is(target error) bool { return target == ErrInvalidInput }

// invalid returns an *inputError, which wraps ErrInvalidInput: "invalid
// input: <field> <rule>".
func invalid(field, rule string) error {
	return &inputError{field: field, rule: rule}
}

// checkText requires printable text of 1 to MaxFilterLen characters:
// valid UTF-8 with no control or other non-printing characters (plain
// spaces are allowed). It guards the values that reach Frappe as filter
// values and the company ID.
func checkText(field, s string) error {
	if s == "" {
		return invalid(field, "must not be empty")
	}
	if !utf8.ValidString(s) {
		return invalid(field, "must be valid UTF-8 text")
	}
	if n := utf8.RuneCountInString(s); n > MaxFilterLen {
		return invalid(field, fmt.Sprintf("must be at most %d characters, got %d", MaxFilterLen, n))
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return invalid(field, fmt.Sprintf("must be printable text without control characters, found %U", r))
		}
	}
	return nil
}

// checkOptionalText is checkText for an optional filter: empty means "no
// filter".
func checkOptionalText(field, s string) error {
	if s == "" {
		return nil
	}
	return checkText(field, s)
}

// parseDate parses a YYYY-MM-DD date to UTC midnight.
func parseDate(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, invalid(field, "is required, a date in YYYY-MM-DD form")
	}
	d, err := time.Parse(dateLayout, s)
	if err != nil || d.Format(dateLayout) != s {
		return time.Time{}, invalid(field, fmt.Sprintf("must be a date in YYYY-MM-DD form, got %q", clip(s)))
	}
	return d, nil
}

// dateRange parses from_date and to_date and requires from <= to and a
// range of at most MaxRangeDays days, both ends included.
func dateRange(fromS, toS string) (from, to time.Time, err error) {
	if from, err = parseDate("from_date", fromS); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to, err = parseDate("to_date", toS); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, invalid("from_date", fmt.Sprintf("%s must not be after to_date %s", fromS, toS))
	}
	if days := int(to.Sub(from)/(24*time.Hour)) + 1; days > MaxRangeDays {
		return time.Time{}, time.Time{}, invalid("to_date", fmt.Sprintf("range %s..%s covers %d days; the most is %d", fromS, toS, days, MaxRangeDays))
	}
	return from, to, nil
}

// parseMonth parses a YYYY-MM month to UTC midnight on its first day.
func parseMonth(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, invalid(field, "is required, a month in YYYY-MM form")
	}
	m, err := time.Parse(monthLayout, s)
	if err != nil || m.Format(monthLayout) != s {
		return time.Time{}, invalid(field, fmt.Sprintf("must be a month in YYYY-MM form, got %q", clip(s)))
	}
	return m, nil
}

// checkCount requires 1 <= n <= MaxToolMonths.
func checkCount(field string, n int) error {
	if n < 1 || n > MaxToolMonths {
		return invalid(field, fmt.Sprintf("must be 1 to %d, got %d", MaxToolMonths, n))
	}
	return nil
}

// cursorEncoding is unpadded URL-safe base64, decoded strictly.
var cursorEncoding = base64.RawURLEncoding.Strict()

// encodeCursor is the list_gl_entries cursor for a page that ends at the
// GL Entry named last: base64 of the name. The next page asks for names
// after it (keyset paging), so rows inserted between calls can't shift
// the pages.
func encodeCursor(last string) string {
	return cursorEncoding.EncodeToString([]byte(last))
}

// decodeCursor returns the GL Entry name a cursor holds, or "" for no
// cursor. A cursor that isn't valid base64 of a printable name is refused.
func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	const rule = "is not a next_cursor returned by list_gl_entries; pass it back unchanged, or omit it for the first page"
	if len(cursor) > 4*MaxFilterLen {
		return "", invalid("cursor", rule)
	}
	b, err := cursorEncoding.DecodeString(cursor)
	if err != nil || len(b) == 0 {
		return "", invalid("cursor", rule)
	}
	name := string(b)
	if checkText("cursor", name) != nil {
		return "", invalid("cursor", rule)
	}
	return name, nil
}

// clip shortens s for an error message.
func clip(s string) string {
	const keep = 40
	if utf8.RuneCountInString(s) <= keep {
		return s
	}
	r := []rune(s)
	return string(r[:keep]) + "..."
}
