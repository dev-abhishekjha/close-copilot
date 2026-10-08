// Command mcp-books_test is not cmd/mcp-books: the name exception does not
// reach it, even inside a []string literal.
package main

import "internal/config"

var required = []string{
	config.EnvMCPTokenAdmin, // want `reference to config.EnvMCPTokenAdmin in cmd/mcp-books_test`
}

func main() { _ = required }
