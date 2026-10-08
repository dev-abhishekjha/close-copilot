// Command mcp-books hosts /mcp-admin: it may list the admin token's
// variable name to require it at startup, but never read the token's value.
package main

import (
	"os"
	"syscall"

	"internal/config"
)

// required lists the variables the server needs at startup: the name is
// allowed as an element of this []string literal.
var required = []string{
	config.EnvMCPTokenAgent,
	config.EnvMCPTokenAdmin,
}

// The name is allowed in an array of strings too, even as a literal.
var requiredArr = [2]string{"MCP_TOKEN_AGENT", "MCP_TOKEN_ADMIN"}

func cliMain(name string, vars []string) {}

func check() bool {
	return os.Getenv("MCP_TOKEN_ADMIN") != "" // want `os.Getenv in cmd/mcp-books` `string "MCP_TOKEN_ADMIN" in cmd/mcp-books`
}

func read() string {
	return os.Getenv(config.EnvMCPTokenAdmin) // want `os.Getenv in cmd/mcp-books` `reference to config.EnvMCPTokenAdmin in cmd/mcp-books`
}

func lookup() bool {
	_, ok := os.LookupEnv("MCP_TOKEN_" + "ADMIN") // want `os.LookupEnv in cmd/mcp-books` `string "MCP_TOKEN_ADMIN" in cmd/mcp-books`
	return ok
}

func everything() []string {
	_ = syscall.Environ()          // want `syscall.Environ in cmd/mcp-books`
	v, _ := syscall.Getenv("HOME") // want `syscall.Getenv in cmd/mcp-books`
	_ = v
	return os.Environ() // want `os.Environ in cmd/mcp-books`
}

// Holding a reader in a variable is reported too.
var getenv = os.Getenv // want `os.Getenv in cmd/mcp-books`

const joined = "MCP_TOKEN_" + "ADMIN" // want `string "MCP_TOKEN_ADMIN" in cmd/mcp-books`

var alias = config.EnvMCPTokenAdmin // want `reference to config.EnvMCPTokenAdmin in cmd/mcp-books`

func indirect() string {
	return getenv(joined) // want `reference to main.joined in cmd/mcp-books`
}

// os.ExpandEnv reads the environment, and a template holding the name is
// not the name, even inside a []string literal.
func expanded() string {
	return os.ExpandEnv("$MCP_TOKEN_ADMIN") // want `os.ExpandEnv in cmd/mcp-books` `string "\$MCP_TOKEN_ADMIN" in cmd/mcp-books contains the admin token's variable name`
}

func braced() string {
	return os.ExpandEnv("${MCP_TOKEN_ADMIN}") // want `os.ExpandEnv in cmd/mcp-books` `string "\$\{MCP_TOKEN_ADMIN\}" in cmd/mcp-books contains the admin token's variable name`
}

var templates = []string{
	"${MCP_TOKEN_ADMIN}",                // want `string "\$\{MCP_TOKEN_ADMIN\}" in cmd/mcp-books contains`
	"$" + config.EnvMCPTokenAdmin,       // want `string "\$MCP_TOKEN_ADMIN" in cmd/mcp-books contains`
	config.EnvMCPTokenAdmin + "_SUFFIX", // want `string "MCP_TOKEN_ADMIN_SUFFIX" in cmd/mcp-books contains`
}

// A map value or a struct field is not a list element.
var byRole = map[string]string{"admin": config.EnvMCPTokenAdmin} // want `reference to config.EnvMCPTokenAdmin in cmd/mcp-books`

// A list element that reads the value is still the value.
var leaked = []string{config.Load().MCPTokenAdmin} // want `reference to config.Config.MCPTokenAdmin in cmd/mcp-books`

func value(cfg config.Config) string {
	return cfg.MCPTokenAdmin // want `reference to config.Config.MCPTokenAdmin in cmd/mcp-books`
}

func main() {
	cliMain("mcp-books", []string{config.EnvERPBaseURL, config.EnvMCPTokenAdmin})
	_, _, _, _ = required, requiredArr, alias, byRole
	_, _, _ = check(), read(), lookup()
	_, _, _ = everything(), indirect(), leaked
	_ = value(config.Load())
}
