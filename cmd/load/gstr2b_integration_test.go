//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validGSTR2B = `{"data":{"gstin":"29AAACS1234A1ZX","rtnprd":"092026","docdata":{"b2b":[
{"ctin":"27AABCC5678D1ZX","trdnm":"Cloudly SaaS","inv":[
{"inum":"CLD/2026/0912","dt":"12-09-2026","val":141600.00,"txval":120000.00,"igst":21600.00,"cgst":0,"sgst":0,"itcavl":"Y"}]}]}}}`

func TestLoadGSTR2BCLI(t *testing.T) {
	dbURL := setupTestStore(t)
	cfg := testConfig(dbURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	path := filepath.Join(t.TempDir(), "gstr2b.json")
	if err := os.WriteFile(path, []byte(validGSTR2B), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	run := newRun(&out)
	args := []string{"gstr2b", "--company", "sharma", "--month", "2026-09", "--file", path}

	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if s := out.String(); !strings.Contains(s, `"inserted": 1`) || !strings.Contains(s, `"total": 1`) {
		t.Errorf("summary = %s", s)
	}
	out.Reset()
	if err := run(context.Background(), cfg, log, args); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if s := out.String(); !strings.Contains(s, `"inserted": 0`) || !strings.Contains(s, `"updated": 1`) {
		t.Errorf("second summary = %s", s)
	}
}
