// Package agent sits in a directory named internal/agent_test. noerpimport
// treats a _test suffix as the package it tests, which only widens its
// scope, so the import is still reported.
package agent

import (
	"internal/frappe" // want `internal/agent must not import internal/frappe`
)

var _ = frappe.Name
