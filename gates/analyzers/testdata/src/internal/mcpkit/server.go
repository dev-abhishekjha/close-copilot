package mcpkit

import "internal/config"

func serve(cfg config.Config) string {
	return cfg.MCPTokenAdmin // want `reference to config.Config.MCPTokenAdmin in internal/mcpkit`
}
