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

// A string that contains the name is reported too: os.ExpandEnv reads it.
func expanded() string {
	return os.ExpandEnv("$MCP_TOKEN_ADMIN") // want `string "\$MCP_TOKEN_ADMIN" in internal/books/admin contains the admin token's variable name`
}

func braced() string {
	return os.ExpandEnv("${MCP_TOKEN_ADMIN}") // want `string "\$\{MCP_TOKEN_ADMIN\}" in internal/books/admin contains the admin token's variable name`
}

const template = "token=${" + config.EnvMCPTokenAdmin + "}" // want `string "token=\$\{MCP_TOKEN_ADMIN\}" in internal/books/admin contains`

func viaConst() string {
	return os.ExpandEnv(template) // want `reference to admin.template in internal/books/admin: its value contains the admin token's variable name`
}

// Other variables' names are fine, even next to it in the alphabet.
func agentExpanded() string {
	return os.ExpandEnv("$MCP_TOKEN_AGENT")
}

func agentToken() string {
	return os.Getenv(config.EnvMCPTokenAgent) // the agent token is fine
}
