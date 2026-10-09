// Package config loads settings from environment variables into a typed
// Config. Secrets come only from the environment; .env.example lists every
// variable (Implementation Tickets, shared specs).
//
// Each binary names the variables it cannot run without, and Load reports
// every missing one in a single error so a fresh setup fails fast and once.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Environment variable names. Keep in sync with .env.example.
//
//nolint:gosec // G101 false positive: these are variable names, not secrets.
const (
	EnvERPBaseURL       = "ERP_BASE_URL"
	EnvERPSite          = "ERP_SITE"
	EnvERPAPIKey        = "ERP_API_KEY"
	EnvERPAPISecret     = "ERP_API_SECRET"
	EnvERPSeedAPIKey    = "ERP_SEED_API_KEY"
	EnvERPSeedAPISecret = "ERP_SEED_API_SECRET"
	EnvDatabaseURL      = "DATABASE_URL"
	EnvBooksMCPURL      = "BOOKS_MCP_URL"
	EnvEvidenceMCPURL   = "EVIDENCE_MCP_URL"
	EnvMCPTokenAgent    = "MCP_TOKEN_AGENT"
	EnvMCPTokenAdmin    = "MCP_TOKEN_ADMIN"
	EnvMCPScopeKey      = "MCP_SCOPE_KEY"
	EnvLLMProvider      = "LLM_PROVIDER"
	EnvClaudeCLIPath    = "CLAUDE_CLI_PATH"
	EnvAnthropicAPIKey  = "ANTHROPIC_API_KEY"
	EnvLLMModelFast     = "LLM_MODEL_FAST"
	EnvLLMModelStrong   = "LLM_MODEL_STRONG"
	EnvLLMDailyBudget   = "LLM_DAILY_BUDGET_USD"
	EnvPseudonymKey     = "PSEUDONYM_KEY"
	EnvTEIEmbedURL      = "TEI_EMBED_URL"
	EnvTEIRerankURL     = "TEI_RERANK_URL"
	EnvDoclingURL       = "DOCLING_URL"
	EnvOTLPEndpoint     = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPHeaders      = "OTEL_EXPORTER_OTLP_HEADERS"
	EnvAppAddr          = "APP_ADDR"
	EnvAppSessionKey    = "APP_SESSION_KEY"
	EnvDataDir          = "DATA_DIR"
)

// LLM providers (CC-701). claude-cli runs model calls through the local
// `claude -p` for development (ADR 0001); anthropic uses the API key and is
// required for the public demo and final baselines.
const (
	ProviderClaudeCLI = "claude-cli"
	ProviderAnthropic = "anthropic"
)

// Config holds every setting the binaries use. Fields a binary didn't mark
// as required may be empty. Credentials are Secret, so printing, logging or
// JSON-encoding a Config never shows them (CC-104).
type Config struct {
	ERPBaseURL       string
	ERPSite          string
	ERPAPIKey        Secret
	ERPAPISecret     Secret
	ERPSeedAPIKey    Secret
	ERPSeedAPISecret Secret

	DatabaseURL Secret // carries the Postgres password

	BooksMCPURL    string
	EvidenceMCPURL string
	MCPTokenAgent  Secret
	MCPTokenAdmin  Secret
	MCPScopeKey    Secret

	LLMProvider       string
	ClaudeCLIPath     string
	AnthropicAPIKey   Secret
	LLMModelFast      string
	LLMModelStrong    string
	LLMDailyBudgetUSD float64
	PseudonymKey      Secret

	TEIEmbedURL  string
	TEIRerankURL string
	DoclingURL   string

	OTLPEndpoint string
	OTLPHeaders  Secret // carries exporter auth

	AppAddr       string
	AppSessionKey Secret

	DataDir string
}

// LogValue implements slog.LogValuer. It lists the non-secret settings and
// shows each secret only as set ("[redacted]") or unset ("").
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("ERPBaseURL", c.ERPBaseURL),
		slog.String("ERPSite", c.ERPSite),
		slog.String("ERPAPIKey", c.ERPAPIKey.masked()),
		slog.String("ERPAPISecret", c.ERPAPISecret.masked()),
		slog.String("ERPSeedAPIKey", c.ERPSeedAPIKey.masked()),
		slog.String("ERPSeedAPISecret", c.ERPSeedAPISecret.masked()),
		slog.String("DatabaseURL", c.DatabaseURL.masked()),
		slog.String("BooksMCPURL", c.BooksMCPURL),
		slog.String("EvidenceMCPURL", c.EvidenceMCPURL),
		slog.String("MCPTokenAgent", c.MCPTokenAgent.masked()),
		slog.String("MCPTokenAdmin", c.MCPTokenAdmin.masked()),
		slog.String("MCPScopeKey", c.MCPScopeKey.masked()),
		slog.String("LLMProvider", c.LLMProvider),
		slog.String("ClaudeCLIPath", c.ClaudeCLIPath),
		slog.String("AnthropicAPIKey", c.AnthropicAPIKey.masked()),
		slog.String("LLMModelFast", c.LLMModelFast),
		slog.String("LLMModelStrong", c.LLMModelStrong),
		slog.Float64("LLMDailyBudgetUSD", c.LLMDailyBudgetUSD),
		slog.String("PseudonymKey", c.PseudonymKey.masked()),
		slog.String("TEIEmbedURL", c.TEIEmbedURL),
		slog.String("TEIRerankURL", c.TEIRerankURL),
		slog.String("DoclingURL", c.DoclingURL),
		slog.String("OTLPEndpoint", c.OTLPEndpoint),
		slog.String("OTLPHeaders", c.OTLPHeaders.masked()),
		slog.String("AppAddr", c.AppAddr),
		slog.String("AppSessionKey", c.AppSessionKey.masked()),
		slog.String("DataDir", c.DataDir),
	)
}

// defaults apply when a variable is unset or empty.
var defaults = map[string]string{
	EnvLLMProvider:    ProviderClaudeCLI,
	EnvClaudeCLIPath:  "claude",
	EnvAppAddr:        ":8000",
	EnvDataDir:        "./data/external",
	EnvLLMDailyBudget: "2.00",
}

// Lookup reads one variable; os.LookupEnv satisfies it.
type Lookup func(key string) (string, bool)

// FromEnv loads the configuration from the process environment.
func FromEnv(required ...string) (Config, error) {
	return Load(os.LookupEnv, required...)
}

// Load builds a Config from lookup. Every name in required must be set to a
// non-empty value; all missing names are reported together.
func Load(lookup Lookup, required ...string) (Config, error) {
	var c Config
	plain := map[string]*string{
		EnvERPBaseURL:     &c.ERPBaseURL,
		EnvERPSite:        &c.ERPSite,
		EnvBooksMCPURL:    &c.BooksMCPURL,
		EnvEvidenceMCPURL: &c.EvidenceMCPURL,
		EnvLLMProvider:    &c.LLMProvider,
		EnvClaudeCLIPath:  &c.ClaudeCLIPath,
		EnvLLMModelFast:   &c.LLMModelFast,
		EnvLLMModelStrong: &c.LLMModelStrong,
		EnvTEIEmbedURL:    &c.TEIEmbedURL,
		EnvTEIRerankURL:   &c.TEIRerankURL,
		EnvDoclingURL:     &c.DoclingURL,
		EnvOTLPEndpoint:   &c.OTLPEndpoint,
		EnvAppAddr:        &c.AppAddr,
		EnvDataDir:        &c.DataDir,
	}
	secrets := map[string]*Secret{
		EnvERPAPIKey:        &c.ERPAPIKey,
		EnvERPAPISecret:     &c.ERPAPISecret,
		EnvERPSeedAPIKey:    &c.ERPSeedAPIKey,
		EnvERPSeedAPISecret: &c.ERPSeedAPISecret,
		EnvDatabaseURL:      &c.DatabaseURL,
		EnvMCPTokenAgent:    &c.MCPTokenAgent,
		EnvMCPTokenAdmin:    &c.MCPTokenAdmin,
		EnvMCPScopeKey:      &c.MCPScopeKey,
		EnvAnthropicAPIKey:  &c.AnthropicAPIKey,
		EnvPseudonymKey:     &c.PseudonymKey,
		EnvOTLPHeaders:      &c.OTLPHeaders,
		EnvAppSessionKey:    &c.AppSessionKey,
	}

	get := func(key string) string {
		v, ok := lookup(key)
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			return defaults[key]
		}
		return v
	}

	for key, dst := range plain {
		*dst = get(key)
	}
	for key, dst := range secrets {
		*dst = NewSecret(get(key))
	}

	// isEmpty reports whether a known variable loaded empty.
	isEmpty := func(key string) (empty, known bool) {
		if dst, ok := plain[key]; ok {
			return *dst == "", true
		}
		if dst, ok := secrets[key]; ok {
			return dst.IsZero(), true
		}
		return false, false
	}

	var errs []error

	var missing []string
	for _, key := range required {
		empty, known := isEmpty(key)
		if !known && key != EnvLLMDailyBudget {
			errs = append(errs, fmt.Errorf("config: unknown variable %q marked as required", key))
			continue
		}
		if known && empty {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("config: missing required environment variables: %s (see .env.example)", strings.Join(missing, ", ")))
	}

	budget, err := strconv.ParseFloat(get(EnvLLMDailyBudget), 64)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("config: %s must be a number: %w", EnvLLMDailyBudget, err))
	case budget < 0:
		errs = append(errs, fmt.Errorf("config: %s must not be negative", EnvLLMDailyBudget))
	default:
		c.LLMDailyBudgetUSD = budget
	}

	switch c.LLMProvider {
	case ProviderClaudeCLI:
		// The CLI accepts model aliases; pin full IDs in .env when baselines matter.
		if c.LLMModelFast == "" {
			c.LLMModelFast = "haiku"
		}
		if c.LLMModelStrong == "" {
			c.LLMModelStrong = "sonnet"
		}
	case ProviderAnthropic:
	default:
		errs = append(errs, fmt.Errorf("config: %s must be %q or %q, got %q",
			EnvLLMProvider, ProviderClaudeCLI, ProviderAnthropic, c.LLMProvider))
	}

	return c, errors.Join(errs...)
}

// CheckLLM reports what the chosen LLM provider still needs. Binaries that
// call a model run it at startup; the rest never need LLM settings.
func (c Config) CheckLLM() error {
	if c.LLMProvider != ProviderAnthropic {
		return nil
	}
	var missing []string
	for key, empty := range map[string]bool{
		EnvAnthropicAPIKey: c.AnthropicAPIKey.IsZero(),
		EnvLLMModelFast:    c.LLMModelFast == "",
		EnvLLMModelStrong:  c.LLMModelStrong == "",
	} {
		if empty {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("config: %s=%s needs %s (see .env.example)",
		EnvLLMProvider, ProviderAnthropic, strings.Join(missing, ", "))
}
