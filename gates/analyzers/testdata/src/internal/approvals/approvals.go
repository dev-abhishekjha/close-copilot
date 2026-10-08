// Package approvals uses the admin token: allowed.
package approvals

import "internal/config"

func bearer(cfg config.Config) string { return "Bearer " + cfg.MCPTokenAdmin }
