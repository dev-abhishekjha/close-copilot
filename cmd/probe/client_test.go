package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestRequestHeaders(t *testing.T) {
	f := newFake(t)
	srv := f.start()
	c, err := newClient(testConfig(srv), config.NewSecret(testBotKey), config.NewSecret(testBotSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.do(context.Background(), http.MethodPut, resourcePath("System Settings", "System Settings"), nil, map[string]any{}, nil); !isRefused(err) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, err := loggedUser(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	for _, r := range reqs {
		if want := "token " + testBotKey + ":" + testBotSecret; r.Auth != want {
			t.Errorf("%s %s: Authorization header has the wrong format", r.Method, r.Path)
		}
		if r.Accept != "application/json" {
			t.Errorf("%s %s: Accept = %q", r.Method, r.Path, r.Accept)
		}
		if r.Host != testSite {
			t.Errorf("%s %s: Host = %q, want %q", r.Method, r.Path, r.Host, testSite)
		}
	}
	if reqs[0].ContentType != "application/json" {
		t.Errorf("PUT Content-Type = %q", reqs[0].ContentType)
	}
	if reqs[0].Path != "/api/resource/System Settings/System Settings" {
		t.Errorf("path = %q", reqs[0].Path)
	}
}

func TestNewClientNeedsCredentials(t *testing.T) {
	srv := newFake(t).start()
	if _, err := newClient(testConfig(srv), config.NewSecret(testBotKey), config.Secret{}); err == nil {
		t.Error("want an error for an empty secret")
	}
}

func TestIsRefused(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"403", &apiError{Status: 403}, true},
		{"PermissionError on 417", &apiError{Status: 417, ExcType: "PermissionError"}, true},
		{"wrapped 403", fmt.Errorf("probe: %w", &apiError{Status: 403, ExcType: "PermissionError"}), true},
		{"401 is not a refusal", &apiError{Status: 401, ExcType: "AuthenticationError"}, false},
		{"404", &apiError{Status: 404, ExcType: "DoesNotExistError"}, false},
		{"500", &apiError{Status: 500}, false},
		{"network error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRefused(tt.err); got != tt.want {
				t.Errorf("isRefused(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestParseError(t *testing.T) {
	c := &client{secret: config.NewSecret(testBotSecret)}
	tests := []struct {
		name, body, wantExc, wantMsg string
	}{
		{
			"server messages",
			`{"exc_type":"PermissionError","exception":"frappe.exceptions.PermissionError","_server_messages":"[\"{\\\"message\\\": \\\"Insufficient Permission for System Settings\\\"}\"]","exc":"[\"Traceback...\"]"}`,
			"PermissionError", "Insufficient Permission for System Settings",
		},
		{"exception only", `{"exc_type":"PermissionError","exception":"frappe.exceptions.PermissionError"}`, "PermissionError", "frappe.exceptions.PermissionError"},
		{"message string", `{"message":"Not found"}`, "", "Not found"},
		{"not JSON", `<html>502 Bad Gateway</html>`, "", "<html>502 Bad Gateway</html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.parseError("GET", "/api/x", 403, []byte(tt.body))
			var ae *apiError
			if !errors.As(err, &ae) {
				t.Fatalf("want *apiError, got %T", err)
			}
			if ae.ExcType != tt.wantExc || ae.Message != tt.wantMsg {
				t.Errorf("got exc %q msg %q, want %q %q", ae.ExcType, ae.Message, tt.wantExc, tt.wantMsg)
			}
			if strings.Contains(err.Error(), "Traceback") {
				t.Errorf("error keeps the traceback: %v", err)
			}
		})
	}
}

func TestErrorsRedactSecret(t *testing.T) {
	// A server that echoes the Authorization header in every kind of error
	// body: JSON, plain text, and a body long enough to be truncated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/method/json":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"exc_type":"AuthenticationError","exception":"bad token %s"}`, auth)
		case "/api/method/text":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, "upstream rejected %s", auth)
		case "/api/method/long":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, "%s%s", strings.Repeat("x", 290), auth)
		default:
			_, _ = fmt.Fprintf(w, `{"message": %q`, auth) // truncated JSON: decode error
		}
	}))
	defer srv.Close()

	c, err := newClient(testConfig(srv), config.NewSecret(testBotKey), config.NewSecret(testBotSecret))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/method/json", "/api/method/text", "/api/method/long", "/api/method/decode"} {
		var out struct{ Message string }
		err := c.do(context.Background(), http.MethodGet, path, nil, nil, &out)
		if err == nil {
			t.Fatalf("%s: want an error", path)
		}
		assertNoSecrets(t, err.Error())
		if strings.Contains(err.Error(), testBotSecret[:8]) {
			t.Errorf("%s: error keeps part of the secret: %v", path, err)
		}
	}
	assertNoSecrets(t, c.String())
	assertNoSecrets(t, fmt.Sprintf("%v %+v", c, c))
	assertNoSecrets(t, c.LogValue().String())
}

func TestClientFormattingHidesCredentials(t *testing.T) {
	c, err := newClient(testConfig(newFake(t).start()), config.NewSecret(testBotKey), config.NewSecret(testBotSecret))
	if err != nil {
		t.Fatal(err)
	}
	// holder reaches the client and its fields through unexported fields,
	// where fmt prints by reflection and never calls Format or String.
	type holder struct {
		c   *client
		v   client
		key config.Secret
	}
	h := holder{c: c, v: *c, key: c.secret}
	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%p"} {
		outputs = append(outputs,
			fmt.Sprintf(verb, c), fmt.Sprintf(verb, *c), fmt.Sprintf(verb, h), fmt.Sprintf(verb, &h))
	}
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "c", c, "v", *c, "h", h)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "c", c, "h", h)
	outputs = append(outputs, buf.String())
	for _, out := range outputs {
		assertNoSecrets(t, out)
		if strings.Contains(out, testBotKey) || strings.Contains(out, testBotSecret[:8]) {
			t.Errorf("output shows a credential: %s", out)
		}
	}
	if got := fmt.Sprintf("%#v", c); got != c.String() {
		t.Errorf("%%#v = %q, want %q", got, c.String())
	}
}

func TestDispatchUsage(t *testing.T) {
	srv := newFake(t).start()
	cfg := testConfig(srv)
	for _, args := range [][]string{nil, {"nope"}, {"auth", "extra"}, {"perms", "extra"}} {
		if r := runProbe(t, cfg, args...); r.err == nil {
			t.Errorf("args %q: want an error", args)
		}
	}
}
