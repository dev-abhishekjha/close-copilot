package admin

import (
	"os"

	"internal/config"
)

func token() string {
	return os.Getenv("MCP_TOKEN_ADMIN") // want `string "MCP_TOKEN_ADMIN" in internal/books/admin`
}

func name() string {
	return config.EnvMCPTokenAdmin // want `reference to config.EnvMCPTokenAdmin in internal/books/admin`
}

const sneaky = "MCP_TOKEN_" + "ADMIN" // want `string "MCP_TOKEN_ADMIN" in internal/books/admin`

func agentToken() string {
	return os.Getenv(config.EnvMCPTokenAgent) // the agent token is fine
}
