package agent

import (
	frp "internal/frappe" // want `internal/agent must not import internal/frappe`
)

var _ = frp.Name
