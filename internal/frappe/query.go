package frappe

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsafeQuery is wrapped by the error List and Call return, before any
// request is sent, when a Query or a method call is outside what the client
// allows: a field, order_by clause, filter or operator that isn't a plain
// identifier form, or a GET on a frappe.client method that writes.
//
// Frappe has had SQL injection bugs through fields and order_by, so the
// client never forwards anything it can't parse as a plain column
// reference. CC-502 validates the same inputs at the MCP tool boundary; this
// is the second line of defence.
var ErrUnsafeQuery = errors.New("frappe: unsafe query")

// fieldPattern is a column reference: an identifier, optionally qualified
// with a backquoted table name (`tab<DocType>`.field). No current caller
// needs an aggregate, so none is allowed.
const fieldPattern = "(?:`tab[A-Za-z0-9 ]+`\\.)?[A-Za-z_][A-Za-z0-9_]*"

var (
	fieldRE   = regexp.MustCompile(`^` + fieldPattern + `$`)
	orderByRE = regexp.MustCompile(`^ *` + fieldPattern + ` +(?i:asc|desc) *(?:, *` + fieldPattern + ` +(?i:asc|desc) *)*$`)
	doctypeRE = regexp.MustCompile(`^[A-Za-z0-9 ]+$`)
)

// filterOperators are the Frappe filter operators the client forwards, in
// lower case (frappe v15, frappe/model/db_query.py and frappe/database/query.py).
// An operator is folded to lower case over ASCII only; one with any
// non-ASCII byte is refused, so no Unicode case mapping can turn into an
// operator.
var filterOperators = map[string]bool{
	"=": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true,
	"like": true, "not like": true, "in": true, "not in": true,
	"between": true, "is": true, "timespan": true,
	"descendants of": true, "not descendants of": true,
	"ancestors of": true, "not ancestors of": true,
	"descendants of (inclusive)": true,
}

// validateQuery checks q's Fields, OrderBy and Filters. Every violation
// wraps ErrUnsafeQuery.
func validateQuery(q Query) error {
	for i, f := range q.Fields {
		if !fieldRE.MatchString(f) {
			return fmt.Errorf("%w: field %d %q is not a column name", ErrUnsafeQuery, i, f)
		}
	}
	if q.OrderBy != "" && !orderByRE.MatchString(q.OrderBy) {
		return fmt.Errorf("%w: order_by %q is not a list of \"<field> asc|desc\"", ErrUnsafeQuery, q.OrderBy)
	}
	for i, f := range q.Filters {
		if err := validateFilter(f); err != nil {
			return fmt.Errorf("%w: filter %d: %w", ErrUnsafeQuery, i, err)
		}
	}
	return nil
}

// validateFilter checks one filter in Frappe's list form: [field, operator,
// value] or [doctype, field, operator, value]. The value is sent as JSON
// data and Frappe escapes it as a value (it is never read as a column or
// SQL), so only its position is checked.
func validateFilter(f []any) error {
	var field, op any
	switch len(f) {
	case 3:
		field, op = f[0], f[1]
	case 4:
		dt, ok := f[0].(string)
		if !ok || !doctypeRE.MatchString(dt) {
			return fmt.Errorf("DocType %v is not a DocType name", f[0])
		}
		field, op = f[1], f[2]
	default:
		return fmt.Errorf("has %d elements, want 3 or 4", len(f))
	}
	name, ok := field.(string)
	if !ok || !fieldRE.MatchString(name) {
		return fmt.Errorf("field %q is not a column name", fmt.Sprint(field))
	}
	if !validOperator(op) {
		return fmt.Errorf("operator %q is not allowed", fmt.Sprint(op))
	}
	return nil
}

// validOperator reports whether op is a string naming one of
// filterOperators, in any ASCII case.
func validOperator(op any) bool {
	o, ok := op.(string)
	if !ok {
		return false
	}
	l, ok := asciiLower(o)
	return ok && filterOperators[l]
}

// asciiLower lower-cases the ASCII letters of s. It reports false, and
// returns "", when s has any non-ASCII byte.
func asciiLower(s string) (string, bool) {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 0x80:
			return "", false
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

// clientReadMethods are the frappe.client methods Call may send as GET. The
// rest of frappe.client (insert, save, set_value, submit, cancel, delete,
// rename_doc, ...) writes, and Frappe accepts GET for whitelisted methods, so
// a GET to them would be a retried write.
var clientReadMethods = map[string]bool{
	"get":       true,
	"get_list":  true,
	"get_count": true,
	"get_value": true,
}

// clientMethod returns the method name after "frappe.client." in lower
// case, or "" when dottedPath isn't a frappe.client method. The comparison
// ignores ASCII case so a spelling trick can't slip past the checks.
func clientMethod(dottedPath string) string {
	l, ok := asciiLower(dottedPath)
	if !ok {
		return ""
	}
	rest, _ := strings.CutPrefix(l, "frappe.client.")
	if rest == l {
		return ""
	}
	return rest
}

// isClientRead reports whether dottedPath is one of clientReadMethods, the
// only calls Call retries.
func isClientRead(dottedPath string) bool {
	return clientReadMethods[clientMethod(dottedPath)]
}

// validateCall refuses a GET on a frappe.client method outside
// clientReadMethods.
func validateCall(httpMethod, dottedPath string) error {
	if httpMethod != "GET" {
		return nil
	}
	if m := clientMethod(dottedPath); m != "" && !clientReadMethods[m] {
		return fmt.Errorf("%w: GET %s; only get, get_list, get_count and get_value of frappe.client may be sent as GET", ErrUnsafeQuery, dottedPath)
	}
	return nil
}

// clientQueryParams are the parameters Call accepts for the frappe.client
// methods that build a query from their arguments (frappe v15,
// frappe/client.py). Anything else, group_by and debug included, is
// refused.
var clientQueryParams = map[string]map[string]bool{
	"get_list": {
		"doctype": true, "fields": true, "filters": true, "or_filters": true, "order_by": true,
		"limit_start": true, "limit_page_length": true, "parent": true, "as_dict": true,
	},
	"get_count": {"doctype": true, "filters": true},
	"get_value": {"doctype": true, "fieldname": true, "filters": true, "as_dict": true, "parent": true},
}

// validateCallParams checks the parameters of frappe.client.get_list,
// get_count and get_value with the rules List applies to a Query: fields
// and fieldname are column names, order_by is "<field> asc|desc" items,
// and filters and or_filters are lists of list-form filters or a map of
// field to value or [operator, value]. A value may be a Go value or the
// JSON string Frappe would parse. Every violation wraps ErrUnsafeQuery.
func validateCallParams(dottedPath string, params map[string]any) error {
	allowed, ok := clientQueryParams[clientMethod(dottedPath)]
	if !ok {
		return nil
	}
	for k, v := range params {
		if !allowed[k] {
			return fmt.Errorf("%w: parameter %q is not accepted", ErrUnsafeQuery, k)
		}
		var err error
		switch k {
		case "doctype":
			if _, ok := v.(string); !ok {
				err = errors.New("doctype is not a string")
			}
		case "fields", "fieldname":
			err = validateFieldList(normParam(v))
		case "order_by":
			err = validateOrderBy(normParam(v))
		case "filters", "or_filters":
			err = validateFilterParam(normParam(v))
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnsafeQuery, k, err)
		}
	}
	return nil
}

// normParam turns a parameter into the generic JSON form Frappe sees: a
// string that holds a JSON array or object is decoded (as frappe.parse_json
// would), any other string stays as it is, and other Go values go through
// encoding/json.
func normParam(v any) any {
	if s, ok := v.(string); ok {
		t := strings.TrimSpace(s)
		if strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
			var out any
			if err := decodeJSON([]byte(t), &out); err == nil {
				return out
			}
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v // not JSON-encodable: the checks below refuse it
	}
	var out any
	if err := decodeJSON(b, &out); err != nil {
		return v
	}
	return out
}

// validateFieldList accepts nil, one column name, or a list of them.
func validateFieldList(v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if !fieldRE.MatchString(x) {
			return fmt.Errorf("%q is not a column name", x)
		}
		return nil
	case []any:
		for i, f := range x {
			s, ok := f.(string)
			if !ok || !fieldRE.MatchString(s) {
				return fmt.Errorf("item %d %q is not a column name", i, fmt.Sprint(f))
			}
		}
		return nil
	}
	return fmt.Errorf("%T is not a column name or a list of them", v)
}

// validateOrderBy accepts nil, "" or an OrderBy that List would accept.
func validateOrderBy(v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x != "" && !orderByRE.MatchString(x) {
			return fmt.Errorf("%q is not a list of \"<field> asc|desc\"", x)
		}
		return nil
	}
	return fmt.Errorf("%T is not a string", v)
}

// validateFilterParam accepts nil, a list of list-form filters, or a map
// from column name to a value or [operator, value]. A plain string (Frappe
// would take it as a document name) is refused; use frappe.client.get or
// {"name": ...} instead.
func validateFilterParam(v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		for i, f := range x {
			l, ok := f.([]any)
			if !ok {
				return fmt.Errorf("filter %d is not a list", i)
			}
			if err := validateFilter(l); err != nil {
				return fmt.Errorf("filter %d: %w", i, err)
			}
		}
		return nil
	case map[string]any:
		for k, val := range x {
			if !fieldRE.MatchString(k) {
				return fmt.Errorf("field %q is not a column name", k)
			}
			if l, ok := val.([]any); ok && (len(l) == 0 || !validOperator(l[0])) {
				return fmt.Errorf("field %q: %q is not an allowed operator", k, fmt.Sprint(l))
			}
		}
		return nil
	}
	return fmt.Errorf("%T is not a list or a map of filters", v)
}
