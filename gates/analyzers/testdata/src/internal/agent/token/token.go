package token

import "internal/config"

func admin(cfg config.Config) string {
	return cfg.MCPTokenAdmin // want `reference to config.Config.MCPTokenAdmin in internal/agent/token`
}

func build() config.Config {
	return config.Config{MCPTokenAdmin: "x"} // want `reference to config.Config.MCPTokenAdmin in internal/agent/token`
}

// A field with the same name on another struct is not the admin token.
type other struct{ MCPTokenAdmin string }

func notAdmin(o other) string { return o.MCPTokenAdmin }

func agent(cfg config.Config) string { return cfg.MCPTokenAgent }
