package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestLoadGSTR2BCLIArgs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "bank, gstr2b"},
		{"unknown subcommand", []string{"foo"}, "bank, gstr2b"},
		{"missing flags", []string{"gstr2b"}, "--company and --month are required"},
		{"missing month", []string{"gstr2b", "--company", "sharma"}, "--company and --month are required"},
		{"bad month", []string{"gstr2b", "--company", "sharma", "--month", "2026-13"}, "invalid --month"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := newRun(&out)(context.Background(), testConfig(""), log, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
