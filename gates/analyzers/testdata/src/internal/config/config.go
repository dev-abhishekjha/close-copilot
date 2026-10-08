// Package config loads the admin token: allowed.
package config

import "os"

const (
	EnvMCPTokenAgent = "MCP_TOKEN_AGENT"
	EnvMCPTokenAdmin = "MCP_TOKEN_ADMIN"
)

type Config struct {
	MCPTokenAgent string
	MCPTokenAdmin string
}

func Load() Config {
	return Config{
		MCPTokenAgent: os.Getenv(EnvMCPTokenAgent),
		MCPTokenAdmin: os.Getenv(EnvMCPTokenAdmin),
	}
}
