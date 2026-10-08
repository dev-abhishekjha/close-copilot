// Package approvals sits in a directory named internal/approvals_test. It is
// not internal/approvals, so it does not inherit its allowance.
package approvals

import "internal/config"

func bearer(cfg config.Config) string {
	return "Bearer " + cfg.MCPTokenAdmin // want `reference to config.Config.MCPTokenAdmin in internal/approvals_test`
}
