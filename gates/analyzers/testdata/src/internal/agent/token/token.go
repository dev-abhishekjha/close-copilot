package token

import (
	"os"

	"internal/approvals"
	"internal/config"
)

func admin(cfg config.Config) string {
	return cfg.MCPTokenAdmin // want `reference to config.Config.MCPTokenAdmin in internal/agent/token`
}

func build() config.Config {
	return config.Config{MCPTokenAdmin: "x"} // want `reference to config.Config.MCPTokenAdmin in internal/agent/token`
}

// A constant re-exported by an allowed package is still the name.
func reexported() string {
	return os.Getenv(approvals.AdminEnv) // want `reference to approvals.AdminEnv in internal/agent/token`
}

// So is a local copy, at its declaration and at every use.
const local = approvals.AdminEnv // want `reference to approvals.AdminEnv in internal/agent/token`

func viaLocal() string {
	return os.Getenv(local) // want `reference to token.local in internal/agent/token`
}

// A field with the same name on another struct is not the admin token.
type other struct{ MCPTokenAdmin string }

func notAdmin(o other) string { return o.MCPTokenAdmin }

func agent(cfg config.Config) string { return cfg.MCPTokenAgent }
