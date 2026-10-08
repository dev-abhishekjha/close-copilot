package frappe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// APIError is a non-2xx response from Frappe. ExcType is Frappe's exc_type
// (the Python exception class name, such as "ValidationError"); it is empty
// when the body wasn't a Frappe error, for example a proxy's HTML page.
// Message comes from _server_messages, message or exception, in that order.
// Tracebacks are dropped, the secret is redacted and the text is truncated.
type APIError struct {
	Method  string
	Path    string
	Status  int
	ExcType string
	Message string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.ExcType != "" {
		msg += " " + e.ExcType
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// IsPermission reports whether Frappe refused the request for lack of
// permission. Only exc_type PermissionError counts: a bare 403 from a proxy
// or WAF says nothing about the user's roles.
func IsPermission(err error) bool {
	ae, ok := asAPIError(err)
	return ok && ae.ExcType == "PermissionError"
}

// IsNotFound reports a DoesNotExistError (or PageDoesNotExistError) or any
// HTTP 404.
func IsNotFound(err error) bool {
	ae, ok := asAPIError(err)
	if !ok {
		return false
	}
	return ae.Status == http.StatusNotFound || ae.ExcType == "DoesNotExistError" || ae.ExcType == "PageDoesNotExistError"
}

// IsAuth reports an authentication failure: HTTP 401 or exc_type
// AuthenticationError. The key pair itself is wrong or revoked.
func IsAuth(err error) bool {
	ae, ok := asAPIError(err)
	return ok && (ae.Status == http.StatusUnauthorized || ae.ExcType == "AuthenticationError")
}

// validationExcTypes are frappe.exceptions.ValidationError and the
// subclasses that describe bad input (frappe v15, frappe/exceptions.py).
// DoesNotExistError is a subclass too but is reported by IsNotFound, and the
// operational ones (RateLimitExceededError, InReadOnlyMode, QueueOverloaded,
// SessionBootFailed) are not validation failures.
var validationExcTypes = map[string]bool{
	"ValidationError":              true,
	"DataError":                    true,
	"MappingMismatchError":         true,
	"InvalidStatusError":           true,
	"MandatoryError":               true,
	"NonNegativeError":             true,
	"CannotChangeConstantError":    true,
	"CharacterLengthExceededError": true,
	"UpdateAfterSubmitError":       true,
	"LinkValidationError":          true,
	"CancelledLinkError":           true,
	"DocstatusTransitionError":     true,
	"TimestampMismatchError":       true,
	"EmptyTableError":              true,
	"LinkExistsError":              true,
	"InvalidEmailAddressError":     true,
	"InvalidNameError":             true,
	"InvalidPhoneNumberError":      true,
	"UniqueValidationError":        true,
	"DocumentLockedError":          true,
	"CircularLinkingError":         true,
	"InvalidColumnName":            true,
	"InvalidDates":                 true,
	"DataTooLongException":         true,
}

// IsValidation reports that Frappe rejected the document or arguments:
// ValidationError or one of its input-related subclasses. ERPNext and India
// Compliance define more subclasses (frappe.throw raises ValidationError by
// default); those inherit HTTP 417, so any 417 that carries an exc_type
// also counts.
func IsValidation(err error) bool {
	ae, ok := asAPIError(err)
	if !ok {
		return false
	}
	return validationExcTypes[ae.ExcType] || (ae.Status == http.StatusExpectationFailed && ae.ExcType != "")
}

func asAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// frappeErrorBody is the part of a Frappe error response the client reads.
// "exc" (the traceback, a JSON list in a string) is deliberately absent.
type frappeErrorBody struct {
	ExcType        string          `json:"exc_type"`
	Exception      string          `json:"exception"`
	Message        json.RawMessage `json:"message"`
	ServerMessages string          `json:"_server_messages"`
}

const maxMessage = 500 // bytes of message text kept in an APIError

// parseError builds an *APIError from a non-2xx response.
func (c *Client) parseError(method, path string, status int, body []byte) error {
	e := &APIError{Method: method, Path: path, Status: status}
	var fe frappeErrorBody
	if err := json.Unmarshal(body, &fe); err != nil {
		e.Message = c.clean(string(body))
		return e
	}
	e.ExcType = c.clean(fe.ExcType)
	msg := serverMessages(fe.ServerMessages)
	if msg == "" {
		msg = rawMessage(fe.Message)
	}
	if msg == "" {
		msg = fe.Exception
	}
	e.Message = c.clean(msg)
	return e
}

// clean redacts the secret, drops any traceback and truncates, in that
// order, so a secret cut in half by truncation can't survive.
func (c *Client) clean(s string) string {
	s = c.redact(s)
	if i := strings.Index(s, "Traceback (most recent call last)"); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), maxMessage)
}

// serverMessages joins the messages in Frappe's _server_messages: a JSON
// array, in a string, of JSON-encoded {"message": ...} objects (or plain
// strings).
func serverMessages(raw string) string {
	if raw == "" {
		return ""
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return ""
	}
	msgs := make([]string, 0, len(list))
	for _, item := range list {
		var m struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(item), &m); err == nil {
			item = m.Message
		}
		if item = strings.TrimSpace(item); item != "" {
			msgs = append(msgs, item)
		}
	}
	return strings.Join(msgs, "; ")
}

// rawMessage returns "message" when it is a string; other shapes are not
// worth reporting.
func rawMessage(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// truncate cuts s to at most limit bytes on a rune boundary.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
