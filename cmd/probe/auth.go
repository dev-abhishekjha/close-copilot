package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// botUser is the ERPNext user deploy/erpnext/api-users.sh creates for the
// Books MCP server.
const botUser = "copilot-bot@example.com"

// loggedUser asks Frappe whose key c holds:
// GET /api/method/frappe.auth.get_logged_user returns {"message": "<user>"}.
func loggedUser(ctx context.Context, c *client) (string, error) {
	var resp struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, http.MethodGet, methodPath("frappe.auth.get_logged_user"), nil, nil, &resp); err != nil {
		return "", err
	}
	return resp.Message, nil
}

// runAuth prints the bot key's user and fails unless it is botUser.
func runAuth(ctx context.Context, bot *client, w io.Writer) error {
	user, err := loggedUser(ctx, bot)
	if err != nil {
		return fmt.Errorf("probe auth: %w", err)
	}
	if user != botUser {
		_, _ = fmt.Fprintf(w, "FAIL auth: the bot key logs in as %q, want %q\n", user, botUser)
		return fmt.Errorf("probe auth: the bot key logs in as %q, want %q", user, botUser)
	}
	_, err = fmt.Fprintf(w, "PASS auth: the bot key logs in as %s\n", user)
	return err
}
