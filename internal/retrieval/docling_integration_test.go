//go:build integration

package retrieval

import (
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestIntegrationConvertPDF(t *testing.T) {
	cfg := itConfig(t, config.EnvDoclingURL)
	d, err := NewDocling(cfg)
	if err != nil {
		t.Fatal(err)
	}
	md, err := d.ConvertPDF(t.Context(), "testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "Bank Charges expense account") {
		t.Fatalf("markdown lacks the known phrase: %q", md)
	}
}
