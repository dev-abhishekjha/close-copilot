package frappe

import (
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// integrationEnv are the variables the integration test needs. CI's
// integration job has no ERPNext, so without them the test skips.
var integrationEnv = []string{
	config.EnvERPBaseURL,
	config.EnvERPSite,
	config.EnvERPAPIKey,
	config.EnvERPAPISecret,
	config.EnvERPSeedAPIKey,
	config.EnvERPSeedAPISecret,
}

// integrationConfig loads the ERPNext settings through lookup, or skips t
// when any of integrationEnv is unset or empty. Only variable names reach
// the skip message, never values.
func integrationConfig(t *testing.T, lookup config.Lookup) config.Config {
	t.Helper()
	var missing []string
	for _, k := range integrationEnv {
		if v, ok := lookup(k); !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("ERPNext integration test skipped: %s not set (source tmp/erp-keys.env)", strings.Join(missing, ", "))
	}
	cfg, err := config.Load(lookup, integrationEnv...)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// TestIntegrationSkips shows the integration test's guard: with any of
// the variables missing, integrationConfig skips instead of failing.
func TestIntegrationSkips(t *testing.T) {
	full := map[string]string{}
	for _, k := range integrationEnv {
		full[k] = "x"
	}
	full[config.EnvERPBaseURL] = "http://localhost:8080"

	cases := map[string]map[string]string{"nothing set": {}}
	for _, k := range integrationEnv {
		env := map[string]string{}
		for kk, v := range full {
			if kk != k {
				env[kk] = v
			}
		}
		cases["without "+k] = env
	}
	cases["empty "+config.EnvERPSeedAPISecret] = func() map[string]string {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env[config.EnvERPSeedAPISecret] = " "
		return env
	}()

	for name, env := range cases {
		var inner *testing.T
		t.Run(name, func(t *testing.T) {
			inner = t
			integrationConfig(t, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			t.Error("integrationConfig returned instead of skipping")
		})
		if inner == nil || !inner.Skipped() {
			t.Errorf("%s: the guard did not skip", name)
		}
	}

	t.Run("all set", func(t *testing.T) {
		cfg := integrationConfig(t, func(k string) (string, bool) { v, ok := full[k]; return v, ok })
		if cfg.ERPBaseURL != "http://localhost:8080" || cfg.ERPSeedAPIKey != "x" {
			t.Errorf("cfg not loaded: base %q", cfg.ERPBaseURL)
		}
	})
}
