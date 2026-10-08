// Package sub is below cmd/mcp-books: the name exception does not reach it.
package sub

import (
	"os"

	"internal/config"
)

func name() string {
	return config.EnvMCPTokenAdmin // want `reference to config.EnvMCPTokenAdmin in cmd/mcp-books/sub`
}

func token() string {
	return os.Getenv("MCP_TOKEN_ADMIN") // want `string "MCP_TOKEN_ADMIN" in cmd/mcp-books/sub`
}
