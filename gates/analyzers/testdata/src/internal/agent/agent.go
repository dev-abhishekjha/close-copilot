package agent

import (
	"internal/frappe" // want `internal/agent must not import internal/frappe`
)

var _ = frappe.Name
