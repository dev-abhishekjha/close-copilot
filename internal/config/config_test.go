package config

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
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

func TestSecret(t *testing.T) {
	const raw = "sk-test-123"
	set, empty := NewSecret(raw), Secret{}

	if set.Reveal() != raw || set.IsZero() {
		t.Errorf("set secret: Reveal %q IsZero %v", set.Reveal(), set.IsZero())
	}
	if empty.Reveal() != "" || !empty.IsZero() || !NewSecret("").IsZero() {
		t.Error("empty secret: want Reveal \"\" and IsZero")
	}

	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10s", "%-10v", "%T"}
	for _, tc := range []struct {
		name string
		s    Secret
		want string
	}{
		{"set", set, redacted},
		{"empty", empty, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputs := map[string]string{
				"String":   tc.s.String(),
				"GoString": tc.s.GoString(),
				"Sprint":   fmt.Sprint(tc.s),
				"Sprint&":  fmt.Sprint(&tc.s),
			}
			for _, v := range verbs {
				outputs[v] = fmt.Sprintf(v, tc.s)
				outputs[v+"&"] = fmt.Sprintf(v, &tc.s)
			}
			for _, k := range []string{"String", "GoString", "Sprint", "Sprint&", "%v", "%+v", "%#v", "%s", "%#v&"} {
				if outputs[k] != tc.want {
					t.Errorf("%s = %q, want %q", k, outputs[k], tc.want)
				}
			}
			if got := outputs["%q"]; got != strconv.Quote(tc.want) {
				t.Errorf("%%q = %q, want %q", got, strconv.Quote(tc.want))
			}

			if got := tc.s.LogValue(); got.Kind() != slog.KindString || got.String() != tc.want {
				t.Errorf("LogValue = %v, want %q", got, tc.want)
			}
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "s", tc.s, "p", &tc.s)
			outputs["slog"] = buf.String()
			if !strings.Contains(buf.String(), `"s":"`+tc.want+`"`) || !strings.Contains(buf.String(), `"p":"`+tc.want+`"`) {
				t.Errorf("slog = %s, want %q for s and p", buf.String(), tc.want)
			}

			j, err := json.Marshal(tc.s)
			if err != nil || string(j) != strconv.Quote(tc.want) {
				t.Errorf("MarshalJSON = %s, %v; want %q", j, err, strconv.Quote(tc.want))
			}
			outputs["json"] = string(j)
			jm, err := json.Marshal(map[string]Secret{"k": tc.s})
			if err != nil || string(jm) != `{"k":`+strconv.Quote(tc.want)+`}` {
				t.Errorf("json map = %s, %v", jm, err)
			}
			outputs["jsonmap"] = string(jm)
			txt, err := tc.s.MarshalText()
			if err != nil || string(txt) != tc.want {
				t.Errorf("MarshalText = %q, %v; want %q", txt, err, tc.want)
			}

			for k, out := range outputs {
				if strings.Contains(out, raw) {
					t.Errorf("%s leaks the secret: %q", k, out)
				}
			}
		})
	}

	// Secrets come only from the environment: no decoding.
	if _, ok := any(&set).(json.Unmarshaler); ok {
		t.Error("*Secret implements json.Unmarshaler")
	}
	if _, ok := any(&set).(encoding.TextUnmarshaler); ok {
		t.Error("*Secret implements encoding.TextUnmarshaler")
	}
}

// secretMarkers maps every secret variable to a distinct marker value.
var secretMarkers = map[string]string{
	EnvERPAPIKey:        "marker-erp-key",
	EnvERPAPISecret:     "marker-erp-secret",
	EnvERPSeedAPIKey:    "marker-seed-key",
	EnvERPSeedAPISecret: "marker-seed-secret",
	EnvDatabaseURL:      "postgres://cc:marker-db-password@localhost/cc",
	EnvMCPTokenAgent:    "marker-mcp-agent",
	EnvMCPTokenAdmin:    "marker-mcp-admin",
	EnvMCPScopeKey:      "marker-mcp-scope",
	EnvAnthropicAPIKey:  "marker-anthropic",
	EnvPseudonymKey:     "marker-pseudonym",
	EnvOTLPHeaders:      "authorization=Bearer marker-otlp",
	EnvAppSessionKey:    "marker-session",
}

// plainValues maps non-secret variables to values that must stay visible.
var plainValues = map[string]string{
	EnvERPBaseURL:     "http://erp.visible.test:8080",
	EnvERPSite:        "visible-site.localhost",
	EnvBooksMCPURL:    "http://books.visible.test/mcp",
	EnvEvidenceMCPURL: "http://evidence.visible.test/mcp",
	EnvLLMProvider:    ProviderAnthropic,
	EnvClaudeCLIPath:  "/opt/visible/claude",
	EnvLLMModelFast:   "visible-fast-model",
	EnvLLMModelStrong: "visible-strong-model",
	EnvLLMDailyBudget: "7.25",
	EnvTEIEmbedURL:    "http://tei-embed.visible.test",
	EnvTEIRerankURL:   "http://tei-rerank.visible.test",
	EnvDoclingURL:     "http://docling.visible.test",
	EnvOTLPEndpoint:   "http://otlp.visible.test:4318",
	EnvAppAddr:        ":9777",
	EnvDataDir:        "/visible/data",
}

func allValues() map[string]string {
	all := map[string]string{}
	for k, v := range secretMarkers {
		all[k] = v
	}
	for k, v := range plainValues {
		all[k] = v
	}
	return all
}

// secretFields reads every secret field so a test can't miss one.
func secretFields(c Config) map[string]Secret {
	return map[string]Secret{
		EnvERPAPIKey:        c.ERPAPIKey,
		EnvERPAPISecret:     c.ERPAPISecret,
		EnvERPSeedAPIKey:    c.ERPSeedAPIKey,
		EnvERPSeedAPISecret: c.ERPSeedAPISecret,
		EnvDatabaseURL:      c.DatabaseURL,
		EnvMCPTokenAgent:    c.MCPTokenAgent,
		EnvMCPTokenAdmin:    c.MCPTokenAdmin,
		EnvMCPScopeKey:      c.MCPScopeKey,
		EnvAnthropicAPIKey:  c.AnthropicAPIKey,
		EnvPseudonymKey:     c.PseudonymKey,
		EnvOTLPHeaders:      c.OTLPHeaders,
		EnvAppSessionKey:    c.AppSessionKey,
	}
}

func TestConfigRedacted(t *testing.T) {
	cfg, err := Load(env(allValues()))
	if err != nil {
		t.Fatal(err)
	}
	if len(secretMarkers) != len(secretFields(cfg)) {
		t.Fatalf("markers cover %d secrets, Config has %d", len(secretMarkers), len(secretFields(cfg)))
	}
	// Every string field holds a plain value, so none can hold a credential.
	visible := map[string]bool{}
	for _, v := range plainValues {
		visible[v] = true
	}
	rv := reflect.ValueOf(cfg)
	for i := range rv.NumField() {
		if f := rv.Field(i); f.Kind() == reflect.String && !visible[f.String()] {
			t.Errorf("string field %s = %q is not a plain value", rv.Type().Field(i).Name, f.String())
		}
	}

	outputs := map[string]string{
		"%v":           fmt.Sprintf("%v", cfg),
		"%+v":          fmt.Sprintf("%+v", cfg),
		"%#v":          fmt.Sprintf("%#v", cfg),
		"%v&":          fmt.Sprintf("%v", &cfg),
		"%+v&":         fmt.Sprintf("%+v", &cfg),
		"%#v&":         fmt.Sprintf("%#v", &cfg),
		"Sprint":       fmt.Sprint(cfg),
		"Sprint&":      fmt.Sprint(&cfg),
		"Sprintln":     fmt.Sprintln(cfg),
		"slogJSON":     slogJSON(t, slog.Any("cfg", cfg)),
		"slogJSON&":    slogJSON(t, slog.Any("cfg", &cfg)),
		"slogText":     slogText(slog.Any("cfg", cfg)),
		"slogText&":    slogText(slog.Any("cfg", &cfg)),
		"slogKV":       slogTextKV("cfg", cfg),
		"json.Marshal": mustJSON(t, cfg),
		"json&":        mustJSON(t, &cfg),
	}

	for name, out := range outputs {
		for key, marker := range secretMarkers {
			if strings.Contains(out, marker) || strings.Contains(out, "marker-") {
				t.Errorf("%s leaks %s: %s", name, key, out)
				break
			}
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s does not show %q for set secrets: %s", name, redacted, out)
		}
		for key, v := range plainValues {
			if !strings.Contains(out, v) {
				t.Errorf("%s hides non-secret %s=%q: %s", name, key, v, out)
			}
		}
	}

	// LogValue marks each secret as set or unset, by field name.
	var unset Config
	unset.ERPAPIKey = NewSecret("marker-only-one")
	group := map[string]string{}
	for _, a := range unset.LogValue().Group() {
		group[a.Key] = a.Value.Resolve().String()
	}
	if group["ERPAPIKey"] != redacted || group["MCPTokenAdmin"] != "" || group["DatabaseURL"] != "" {
		t.Errorf("LogValue set/unset: ERPAPIKey %q MCPTokenAdmin %q DatabaseURL %q",
			group["ERPAPIKey"], group["MCPTokenAdmin"], group["DatabaseURL"])
	}
	if len(group) != reflect.TypeOf(Config{}).NumField() {
		t.Errorf("LogValue lists %d settings, Config has %d fields", len(group), reflect.TypeOf(Config{}).NumField())
	}
}

func TestLoadReadsEverySecret(t *testing.T) {
	cfg, err := Load(env(allValues()), keys(secretMarkers)...)
	if err != nil {
		t.Fatal(err)
	}
	for key, s := range secretFields(cfg) {
		if s.Reveal() != secretMarkers[key] {
			t.Errorf("%s: Reveal = %q, want %q", key, s.Reveal(), secretMarkers[key])
		}
	}
}

func TestLoadRequiresSecrets(t *testing.T) {
	_, err := Load(env(nil), keys(secretMarkers)...)
	if err == nil {
		t.Fatal("want an error for missing secrets")
	}
	for key := range secretMarkers {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name %s", err, key)
		}
	}
}

func TestFromEnvLoadsEverySecret(t *testing.T) {
	for k, v := range allValues() {
		t.Setenv(k, v)
	}
	cfg, err := FromEnv(keys(secretMarkers)...)
	if err != nil {
		t.Fatal(err)
	}
	for key, s := range secretFields(cfg) {
		if s.Reveal() != secretMarkers[key] {
			t.Errorf("%s: Reveal = %q, want %q", key, s.Reveal(), secretMarkers[key])
		}
	}
	if cfg.ERPBaseURL != plainValues[EnvERPBaseURL] {
		t.Errorf("ERPBaseURL = %q", cfg.ERPBaseURL)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func slogJSON(t *testing.T, a slog.Attr) string {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).LogAttrs(context.Background(), slog.LevelInfo, "cfg", a)
	if !json.Valid(buf.Bytes()) {
		t.Errorf("slog JSON is not valid: %s", buf.String())
	}
	return buf.String()
}

func slogText(a slog.Attr) string {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).LogAttrs(context.Background(), slog.LevelInfo, "cfg", a)
	return buf.String()
}

func slogTextKV(k string, v any) string {
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", k, v)
	return buf.String()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
