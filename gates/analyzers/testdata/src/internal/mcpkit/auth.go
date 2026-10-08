package mcpkit

import "internal/config"

// auth*.go in internal/mcpkit verifies the admin token: allowed.
func adminOK(cfg config.Config, got string) bool { return got == cfg.MCPTokenAdmin }
