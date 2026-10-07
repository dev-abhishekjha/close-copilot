package config

import (
	"strings"
	"testing"
)

func env(kv map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

func TestLoadReportsEveryMissingVariableOnce(t *testing.T) {
	_, err := Load(env(map[string]string{EnvDatabaseURL: "postgres://x"}),
		EnvDatabaseURL, EnvBooksMCPURL, EnvMCPTokenAgent)
	if err == nil {
		t.Fatal("expected an error for missing variables")
	}
	msg := err.Error()
	for _, want := range []string{EnvBooksMCPURL, EnvMCPTokenAgent} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %s", msg, want)
		}
	}
	if strings.Contains(msg, EnvDatabaseURL) {
		t.Errorf("error %q names %s, which is set", msg, EnvDatabaseURL)
	}
	if n := strings.Count(msg, "missing required"); n != 1 {
		t.Errorf("want one combined error, got %d in %q", n, msg)
	}
}

func TestLoadTreatsBlankAsMissing(t *testing.T) {
	_, err := Load(env(map[string]string{EnvDatabaseURL: "   "}), EnvDatabaseURL)
	if err == nil || !strings.Contains(err.Error(), EnvDatabaseURL) {
		t.Fatalf("want missing %s, got %v", EnvDatabaseURL, err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppAddr != ":8000" || c.DataDir != "./data/external" || c.LLMDailyBudgetUSD != 2 {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestLoadReadsValues(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvERPBaseURL:     "http://localhost:8080",
		EnvERPSite:        "erp.localhost",
		EnvLLMDailyBudget: "5.5",
		EnvAppAddr:        ":9000",
	}), EnvERPBaseURL, EnvERPSite)
	if err != nil {
		t.Fatal(err)
	}
	if c.ERPBaseURL != "http://localhost:8080" || c.ERPSite != "erp.localhost" {
		t.Errorf("ERP settings not read: %+v", c)
	}
	if c.LLMDailyBudgetUSD != 5.5 || c.AppAddr != ":9000" {
		t.Errorf("overrides not read: %+v", c)
	}
}

func TestLoadRejectsBadBudget(t *testing.T) {
	for _, v := range []string{"two", "-1"} {
		if _, err := Load(env(map[string]string{EnvLLMDailyBudget: v})); err == nil {
			t.Errorf("budget %q: want an error", v)
		}
	}
}

func TestLoadRejectsUnknownRequiredName(t *testing.T) {
	if _, err := Load(env(nil), "NOT_A_SETTING"); err == nil {
		t.Fatal("want an error for an unknown variable name")
	}
}

func TestLoadDefaultsToClaudeCLIWithModelAliases(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMProvider != ProviderClaudeCLI || c.ClaudeCLIPath != "claude" {
		t.Errorf("provider defaults: %q %q", c.LLMProvider, c.ClaudeCLIPath)
	}
	if c.LLMModelFast != "haiku" || c.LLMModelStrong != "sonnet" {
		t.Errorf("model aliases: %q %q", c.LLMModelFast, c.LLMModelStrong)
	}
	if err := c.CheckLLM(); err != nil {
		t.Errorf("claude-cli needs no key, got %v", err)
	}
}

func TestLoadRejectsUnknownProvider(t *testing.T) {
	if _, err := Load(env(map[string]string{EnvLLMProvider: "openai"})); err == nil {
		t.Fatal("want an error for an unknown provider")
	}
}

func TestCheckLLMAnthropicNeedsKeyAndModels(t *testing.T) {
	c, err := Load(env(map[string]string{EnvLLMProvider: ProviderAnthropic}))
	if err != nil {
		t.Fatal(err)
	}
	err = c.CheckLLM()
	if err == nil {
		t.Fatal("want an error without a key")
	}
	for _, want := range []string{EnvAnthropicAPIKey, EnvLLMModelFast, EnvLLMModelStrong} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	c, err = Load(env(map[string]string{
		EnvLLMProvider:     ProviderAnthropic,
		EnvAnthropicAPIKey: "sk-ant-test",
		EnvLLMModelFast:    "claude-haiku-4-5-20251001",
		EnvLLMModelStrong:  "claude-sonnet-5-5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckLLM(); err != nil {
		t.Errorf("complete anthropic config: %v", err)
	}
}
