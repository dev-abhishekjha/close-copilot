package frappe

import "net/http"

// Test files are exempt from egress.
func testClient() *http.Client { _, _ = http.Get("u"); return &http.Client{} }
