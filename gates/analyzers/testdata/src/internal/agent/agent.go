package agent

import (
	"internal/books"  // want `internal/agent imports internal/frappe via internal/books`
	"internal/frappe" // want `internal/agent must not import internal/frappe`
	"internal/money"
)

var _ = frappe.Name
var _ = books.Paise(0)
var _ = money.Paise(0)
