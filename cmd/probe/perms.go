package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Endpoints the perms checks use (Frappe v15, REST API v1):
//
//	(a) GET /api/resource/<DocType>?fields=["name"]&limit_page_length=1
//	    a list call; 200 with a (possibly empty) data array means read access.
//	(b) GET /api/method/frappe.client.has_permission?doctype=Journal Entry&docname=&perm_type=<create|submit>
//	    frappe.has_permission at DocType level (empty docname); nothing is created.
//	(c) GET /api/resource/System Settings/System Settings
//	    a read of a System Manager-only single DocType; must be refused.
//	(d) PUT /api/resource/System Settings/System Settings with body {}
//	    a write that changes no field; must be refused.
//
// Refused means HTTP 403 with exc_type PermissionError. Any other error,
// an authentication failure or a bare 403 included, fails the check.
//
// Check (d) runs only when check (c) was refused: a bot that can read
// System Settings, or whose read failed for some other reason, must not be
// sent a write to it. (d) is then reported as SKIP, and (c) has already
// failed the run.

// readDocTypes are the DocTypes the bot must be able to list (check a).
var readDocTypes = []string{"GL Entry", "Purchase Invoice", "Payment Entry"}

// journalPerms are the Journal Entry rights the bot must hold (check b).
var journalPerms = []string{"create", "submit"}

// adminOnlyDocType is a DocType only System Manager may read or write
// (checks c and d). It is a single DocType, so its one document has its name.
const adminOnlyDocType = "System Settings"

type permCheck struct {
	id, name string
	// after names a check that must have passed (every check with that id)
	// before this one runs; empty means always run.
	after string
	run   func(ctx context.Context, bot *client) (detail string, err error)
}

func permChecks() []permCheck {
	var checks []permCheck
	for _, dt := range readDocTypes {
		checks = append(checks, permCheck{id: "a", name: "read " + dt, run: func(ctx context.Context, bot *client) (string, error) {
			return checkRead(ctx, bot, dt)
		}})
	}
	for _, p := range journalPerms {
		checks = append(checks, permCheck{id: "b", name: p + " Journal Entry", run: func(ctx context.Context, bot *client) (string, error) {
			return checkPermission(ctx, bot, "Journal Entry", p)
		}})
	}
	checks = append(checks,
		permCheck{id: "c", name: "refused read of " + adminOnlyDocType, run: func(ctx context.Context, bot *client) (string, error) {
			path := resourcePath(adminOnlyDocType, adminOnlyDocType)
			return expectRefused(bot.do(ctx, http.MethodGet, path, nil, nil, nil), "GET "+path)
		}},
		permCheck{id: "d", name: "refused write to " + adminOnlyDocType, after: "c", run: func(ctx context.Context, bot *client) (string, error) {
			path := resourcePath(adminOnlyDocType, adminOnlyDocType)
			return expectRefused(bot.do(ctx, http.MethodPut, path, nil, map[string]any{}, nil), "PUT "+path)
		}},
	)
	return checks
}

// runPerms confirms the key is the bot's, then runs every check, printing
// one line each, and fails if any failed. The bot check comes first so the
// System Settings write is never tried with another user's key.
func runPerms(ctx context.Context, bot *client, w io.Writer) error {
	user, err := loggedUser(ctx, bot)
	if err != nil {
		return fmt.Errorf("probe perms: %w", err)
	}
	if user != botUser {
		return fmt.Errorf("probe perms: the bot key logs in as %q, want %q; not running the checks", user, botUser)
	}
	if _, err := fmt.Fprintf(w, "PASS auth: logged in as %s\n", user); err != nil {
		return err
	}

	failed := 0
	passed := map[string]bool{} // id -> every check with that id passed
	for _, c := range permChecks() {
		if c.after != "" && !passed[c.after] {
			if _, werr := fmt.Fprintf(w, "SKIP (%s) %s: not run because check (%s) did not pass\n", c.id, c.name, c.after); werr != nil {
				return werr
			}
			continue
		}
		detail, err := c.run(ctx, bot)
		status := "PASS"
		if err != nil {
			status = "FAIL"
			detail = err.Error()
			failed++
		}
		prev, seen := passed[c.id]
		passed[c.id] = err == nil && (prev || !seen)
		if _, werr := fmt.Fprintf(w, "%s (%s) %s: %s\n", status, c.id, c.name, detail); werr != nil {
			return werr
		}
	}
	if failed > 0 {
		return fmt.Errorf("probe perms: %d check(s) failed", failed)
	}
	return nil
}

func checkRead(ctx context.Context, bot *client, doctype string) (string, error) {
	q := url.Values{}
	q.Set("fields", `["name"]`)
	q.Set("limit_page_length", "1")
	var resp struct {
		Data *[]json.RawMessage `json:"data"`
	}
	path := resourcePath(doctype)
	if err := bot.do(ctx, http.MethodGet, path, q, nil, &resp); err != nil {
		return "", err
	}
	if resp.Data == nil {
		return "", fmt.Errorf("GET %s: response has no data array", path)
	}
	return fmt.Sprintf("GET %s?limit_page_length=1 returned %d row(s)", path, len(*resp.Data)), nil
}

func checkPermission(ctx context.Context, bot *client, doctype, ptype string) (string, error) {
	q := url.Values{}
	q.Set("doctype", doctype)
	q.Set("docname", "")
	q.Set("perm_type", ptype)
	var resp struct {
		Message struct {
			HasPermission *bool `json:"has_permission"`
		} `json:"message"`
	}
	path := methodPath("frappe.client.has_permission")
	if err := bot.do(ctx, http.MethodGet, path, q, nil, &resp); err != nil {
		return "", err
	}
	switch {
	case resp.Message.HasPermission == nil:
		return "", fmt.Errorf("GET %s: response has no has_permission", path)
	case !*resp.Message.HasPermission:
		return "", fmt.Errorf("GET %s (perm_type=%s): has_permission is false", path, ptype)
	}
	return fmt.Sprintf("GET %s (perm_type=%s): has_permission is true", path, ptype), nil
}

// expectRefused turns the result of a request that must be refused into a
// check result.
func expectRefused(err error, what string) (string, error) {
	switch {
	case err == nil:
		return "", fmt.Errorf("%s succeeded; the bot must be refused", what)
	case isRefused(err):
		var ae *apiError
		errors.As(err, &ae)
		return fmt.Sprintf("%s refused (HTTP %d %s)", what, ae.Status, ae.ExcType), nil
	default:
		return "", fmt.Errorf("%s failed but was not a permission refusal: %w", what, err)
	}
}
