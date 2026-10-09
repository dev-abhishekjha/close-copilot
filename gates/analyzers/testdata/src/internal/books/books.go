package books

import "internal/frappe"

var _ = frappe.Name

const rate = 1.5 // want `untyped float constant \(defaults to float64\) in internal/books`

// An untyped float constant converted to an integer type is exact: fine.
const lakh int64 = 1e5

type Paise int64

var fee Paise = 1.5e2

func scaled(x int64) int64 { return x * lakh }
