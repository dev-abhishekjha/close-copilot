// Package approvals uses the admin token: allowed.
package approvals

import "internal/config"

// AdminEnv re-exports the variable's name: allowed here, but every use of
// it outside the allowed places is still reported.
const AdminEnv = config.EnvMCPTokenAdmin

func bearer(cfg config.Config) string { return "Bearer " + cfg.MCPTokenAdmin }
