package frappe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serverMessagesJSON encodes msgs the way Frappe does: a JSON array, in a
// string, of JSON-encoded {"message": ...} objects.
func serverMessagesJSON(t *testing.T, msgs ...string) string {
	t.Helper()
	items := make([]string, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(map[string]any{"message": m, "title": "Message", "indicator": "red", "raise_exception": 1})
		if err != nil {
			t.Fatal(err)
		}
		items[i] = string(b)
	}
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAPIError(t *testing.T) {
	traceback := "Traceback (most recent call last):\n  File \"apps/frappe/frappe/app.py\", line 114, in application\nfrappe.exceptions.ValidationError: boom"
	excField, _ := json.Marshal([]string{traceback})

	tests := []struct {
		name        string
		status      int
		body        string
		wantExcType string
		wantMessage string // exact Message
		permission  bool
		notFound    bool
		validation  bool
		auth        bool
	}{
		{
			// Built in the v15 shape (exception, exc_type, exc, _server_messages);
			// recording one would need a write to the shared site.
			name:        "validation error",
			status:      http.StatusExpectationFailed,
			body:        readFixture(t, "validation_error.json"),
			wantExcType: "ValidationError",
			wantMessage: "Total Debit must be equal to Total Credit. The difference is 1.0",
			validation:  true,
		},
		{
			// Recorded from erp.localhost (frappe 15.122.0): the bot reading DocType meta.
			name:        "recorded permission error",
			status:      http.StatusForbidden,
			body:        readFixture(t, "permission_error.json"),
			wantExcType: "PermissionError",
			wantMessage: "User <strong>copilot-bot@example.com</strong> does not have doctype access via role permission for document <strong>DocType</strong><br>User <strong>copilot-bot@example.com</strong> does not have access to this document",
			permission:  true,
		},
		{
			// Recorded from erp.localhost: GET of a Journal Entry that doesn't exist.
			name:        "recorded does-not-exist error",
			status:      http.StatusNotFound,
			body:        readFixture(t, "does_not_exist.json"),
			wantExcType: "DoesNotExistError",
			wantMessage: "Journal Entry ACC-JV-2026-99999 not found",
			notFound:    true,
		},
		{
			name:   "server messages joined",
			status: http.StatusExpectationFailed,
			body: mustJSON(t, map[string]any{
				"exc_type":         "MandatoryError",
				"exception":        "frappe.exceptions.MandatoryError: [Journal Entry, new-1]: company",
				"exc":              string(excField),
				"_server_messages": serverMessagesJSON(t, "Value missing for Company", "Value missing for Posting Date"),
			}),
			wantExcType: "MandatoryError",
			wantMessage: "Value missing for Company; Value missing for Posting Date",
			validation:  true,
		},
		{
			name:        "ERPNext subclass with 417 counts as validation",
			status:      http.StatusExpectationFailed,
			body:        `{"exc_type":"InvalidAccountCurrency","exception":"erpnext.accounts.InvalidAccountCurrency: bad currency"}`,
			wantExcType: "InvalidAccountCurrency",
			wantMessage: "erpnext.accounts.InvalidAccountCurrency: bad currency",
			validation:  true,
		},
		{
			name:        "permission error",
			status:      http.StatusForbidden,
			body:        `{"exc_type":"PermissionError","exception":"frappe.exceptions.PermissionError","_server_messages":` + mustJSON(t, serverMessagesJSON(t, "Not permitted")) + `}`,
			wantExcType: "PermissionError",
			wantMessage: "Not permitted",
			permission:  true,
		},
		{
			name:        "bare 403 from a proxy is not a permission error",
			status:      http.StatusForbidden,
			body:        "<html><body>403 Forbidden</body></html>",
			wantMessage: "<html><body>403 Forbidden</body></html>",
		},
		{
			name:        "403 JSON without exc_type is not a permission error",
			status:      http.StatusForbidden,
			body:        `{"message":"blocked by policy"}`,
			wantMessage: "blocked by policy",
		},
		{
			name:        "does not exist",
			status:      http.StatusNotFound,
			body:        `{"exc_type":"DoesNotExistError","exception":"frappe.exceptions.DoesNotExistError: Journal Entry ACC-JV-2026-99999 not found"}`,
			wantExcType: "DoesNotExistError",
			wantMessage: "frappe.exceptions.DoesNotExistError: Journal Entry ACC-JV-2026-99999 not found",
			notFound:    true,
		},
		{
			name:        "bare 404",
			status:      http.StatusNotFound,
			body:        "Not Found",
			wantMessage: "Not Found",
			notFound:    true,
		},
		{
			name:        "authentication error",
			status:      http.StatusUnauthorized,
			body:        `{"exc_type":"AuthenticationError","exception":"frappe.exceptions.AuthenticationError"}`,
			wantExcType: "AuthenticationError",
			wantMessage: "frappe.exceptions.AuthenticationError",
			auth:        true,
		},
		{
			name:        "traceback in a plain-text 500 is dropped",
			status:      http.StatusInternalServerError,
			body:        "Internal error\n" + traceback,
			wantMessage: "Internal error",
		},
		{
			name:        "traceback in exception is dropped",
			status:      http.StatusInternalServerError,
			body:        mustJSON(t, map[string]any{"exc_type": "OperationalError", "exception": "db down\n" + traceback}),
			wantExcType: "OperationalError",
			wantMessage: "db down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			_, err := List[map[string]any](t.Context(), c, "Journal Entry", Query{})
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v, want an *APIError", err)
			}
			if ae.Method != http.MethodGet || ae.Path != "/api/resource/Journal%20Entry" || ae.Status != tt.status {
				t.Errorf("APIError = %s %s %d", ae.Method, ae.Path, ae.Status)
			}
			if ae.ExcType != tt.wantExcType {
				t.Errorf("ExcType = %q, want %q", ae.ExcType, tt.wantExcType)
			}
			if ae.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", ae.Message, tt.wantMessage)
			}
			if strings.Contains(err.Error(), "Traceback") || strings.Contains(err.Error(), "app.py") {
				t.Errorf("error keeps the traceback: %v", err)
			}
			checks := []struct {
				name string
				got  bool
				want bool
			}{
				{"IsPermission", IsPermission(err), tt.permission},
				{"IsNotFound", IsNotFound(err), tt.notFound},
				{"IsValidation", IsValidation(err), tt.validation},
				{"IsAuth", IsAuth(err), tt.auth},
			}
			for _, ch := range checks {
				if ch.got != ch.want {
					t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
				}
			}
		})
	}

	t.Run("predicates on non-API errors", func(t *testing.T) {
		for _, err := range []error{nil, errors.New("x"), fmt.Errorf("wrapped: %w", errors.New("x"))} {
			if IsPermission(err) || IsNotFound(err) || IsValidation(err) || IsAuth(err) {
				t.Errorf("a predicate is true for %v", err)
			}
		}
	})

	t.Run("long messages are truncated on a rune boundary", func(t *testing.T) {
		got := truncate(strings.Repeat("₹", 300), maxMessage)
		if len(got) > maxMessage+3 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "₹") {
			t.Errorf("truncate gave %d bytes: %q", len(got), got[:20])
		}
		if strings.ContainsRune(got, '�') {
			t.Error("truncate split a rune")
		}
	})
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
