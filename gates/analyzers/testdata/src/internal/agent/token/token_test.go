package token

import "internal/config"

// Test files are exempt from admintoken.
func fake() config.Config { return config.Config{MCPTokenAdmin: "test"} }
