//go:build integration

package retrieval

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// itConfig returns the URLs from the environment and skips the test when one
// is unset or its service does not answer. Plain `go test` runs never get here.
func itConfig(t *testing.T, envs ...string) config.Config {
	t.Helper()
	var cfg config.Config
	for _, env := range envs {
		u := os.Getenv(env)
		if u == "" {
			t.Skipf("%s not set", env)
		}
		switch env {
		case config.EnvTEIEmbedURL:
			cfg.TEIEmbedURL = u
		case config.EnvTEIRerankURL:
			cfg.TEIRerankURL = u
		case config.EnvDoclingURL:
			cfg.DoclingURL = u
		}
		// A plain probe client is fine in a test; the clients under test use httpx.
		if !reachable(u) {
			t.Skipf("%s=%s is not reachable", env, u)
		}
	}
	return cfg
}

func reachable(base string) bool {
	hc := &http.Client{Timeout: 3 * time.Second} //nolint:forbidigo // test-only reachability probe
	resp, err := hc.Get(base + "/health")        //nolint:noctx // test-only probe
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func TestIntegrationEmbed(t *testing.T) {
	cfg := itConfig(t, config.EnvTEIEmbedURL, config.EnvTEIRerankURL)
	tei, err := NewTEI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tei.Embed(t.Context(), []string{"bank charges for March", "office rent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0]) != 384 || len(got[1]) != 384 {
		t.Fatalf("want 2 vectors of 384 dims, got %d", len(got))
	}
}

func TestIntegrationRerank(t *testing.T) {
	cfg := itConfig(t, config.EnvTEIEmbedURL, config.EnvTEIRerankURL)
	tei, err := NewTEI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"The cafeteria menu changes every Tuesday.",
		"Bank charges are debited by the bank for account maintenance and booked to the Bank Charges expense account.",
		"The office plants are watered on Fridays.",
	}
	got, err := tei.Rerank(t.Context(), "How are bank charges booked?", texts)
	if err != nil {
		t.Fatal(err)
	}
	best := got[0]
	for _, s := range got {
		if s.Score > best.Score {
			best = s
		}
	}
	if best.Index != 1 {
		t.Fatalf("most relevant index = %d, want 1 (%+v)", best.Index, got)
	}
}
