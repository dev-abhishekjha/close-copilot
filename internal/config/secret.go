package config

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// redacted stands in for a set secret in every printed, logged or encoded form.
const redacted = "[redacted]"

// Secret holds a credential read from the environment. Its fmt, slog, JSON
// and text forms show "[redacted]" when set and "" when empty, so a Config
// can be logged or printed whole. Reveal is the only way to the raw value.
//
// The value sits behind a pointer. fmt does not call Format or String on a
// value it reaches through an unexported struct field, and it prints %p
// operands before any Formatter runs; in both cases reflection walks the
// struct and finds only an address, never the string.
//
// Secret has no UnmarshalJSON or UnmarshalText: secrets come only from the
// environment (see Load).
type Secret struct{ p *string }

// NewSecret wraps s. NewSecret("") is the zero Secret. Tests use it to build
// a Config by hand.
func NewSecret(s string) Secret {
	if s == "" {
		return Secret{}
	}
	return Secret{p: &s}
}

// Reveal returns the raw value. Call it only where the credential is used,
// for example to build an Authorization header or open a connection.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.p == nil }

// masked is the printable form: "[redacted]" when set, "" when empty.
func (s Secret) masked() string {
	if s.IsZero() {
		return ""
	}
	return redacted
}

// String implements fmt.Stringer.
func (s Secret) String() string { return s.masked() }

// GoString implements fmt.GoStringer.
func (s Secret) GoString() string { return s.masked() }

// Format implements fmt.Formatter for every verb, including %#v and %+v,
// so no verb or flag reaches the raw value. %q quotes the masked form.
func (s Secret) Format(f fmt.State, verb rune) {
	out := s.masked()
	if verb == 'q' {
		out = strconv.Quote(out)
	}
	_, _ = io.WriteString(f, out)
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.masked()) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.masked())), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.masked()), nil }
