package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestUsersFile(t *testing.T) {
	us, err := LoadUsers("../../config/users.yaml")
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	want := map[string]Role{"asha": RoleMaker, "ravi": RoleChecker, "demo": RoleViewer}
	if us.Len() != len(want) {
		t.Errorf("users = %d, want %d", us.Len(), len(want))
	}
	for name, role := range want {
		u, ok := us.Lookup(name)
		if !ok {
			t.Errorf("user %s missing", name)
			continue
		}
		if u.Role != role {
			t.Errorf("%s role = %s, want %s", name, u.Role, role)
		}
		cost, err := bcrypt.Cost([]byte(u.PasswordHash))
		if err != nil || cost < 10 {
			t.Errorf("%s hash cost = %d (%v), want bcrypt >= 10", name, cost, err)
		}
		if u.TokenSHA256 != "" {
			t.Errorf("%s has a token_sha256 in the seed file; bearer must stay off until the owner sets one", name)
		}
	}
}

func TestLoadUsersRejects(t *testing.T) {
	f := testFixture(t)
	good := f.byName["asha"].PasswordHash
	lowCost, err := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(name, role, hash, token string) string {
		return "  - username: " + name + "\n    role: " + role + "\n    password_hash: \"" + hash + "\"\n    token_sha256: \"" + token + "\"\n"
	}
	tok := tokenHash("t")
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"valid", "users:\n" + entry("asha", "maker", good, "") + entry("ravi", "checker", good, tok), ""},
		{"upper-case token accepted", "users:\n" + entry("asha", "maker", good, strings.ToUpper(tok)), ""},
		{"empty file", "", "empty"},
		{"no users", "users: []\n", "no users"},
		{"duplicate username", "users:\n" + entry("asha", "maker", good, "") + entry("asha", "checker", good, ""), "twice"},
		{"unknown role", "users:\n" + entry("asha", "admin", good, ""), "unknown role"},
		{"empty role", "users:\n" + entry("asha", "", good, ""), "unknown role"},
		{"empty hash", "users:\n" + entry("asha", "maker", "", ""), "empty"},
		{"non-bcrypt hash", "users:\n" + entry("asha", "maker", tokenHash("pw"), ""), "not a bcrypt"},
		{"plaintext password as hash", "users:\n" + entry("asha", "maker", "hunter2", ""), "not a bcrypt"},
		{"low bcrypt cost", "users:\n" + entry("asha", "maker", string(lowCost), ""), "below 10"},
		{"short token hash", "users:\n" + entry("asha", "maker", good, "abc123"), "64 hex"},
		{"non-hex token hash", "users:\n" + entry("asha", "maker", good, strings.Repeat("z", 64)), "64 hex"},
		{"shared token hash", "users:\n" + entry("asha", "maker", good, tok) + entry("ravi", "checker", good, tok), "share"},
		{"bad username", "users:\n" + entry("Asha Rao", "maker", good, ""), "username"},
		{"empty username", "users:\n" + entry("", "maker", good, ""), "username"},
		{"unknown field", "users:\n  - username: asha\n    role: maker\n    pasword_hash: \"" + good + "\"\n", "pasword_hash"},
		{"not yaml", "users: [", "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			us, err := LoadUsers(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadUsers: %v", err)
				}
				for _, u := range us.All() {
					if u.TokenSHA256 != strings.ToLower(u.TokenSHA256) {
						t.Errorf("token hash of %s not normalised", u.Name)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadUsers accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), good) {
				t.Error("error message contains a password hash")
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadUsers(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Error("LoadUsers of a missing file succeeded")
		}
	})
}

func TestRoleValid(t *testing.T) {
	for _, tt := range []struct {
		role Role
		ok   bool
	}{{RoleViewer, true}, {RoleMaker, true}, {RoleChecker, true}, {"admin", false}, {"", false}, {"Maker", false}} {
		if got := tt.role.Valid(); got != tt.ok {
			t.Errorf("Role(%q).Valid() = %v", tt.role, got)
		}
	}
}
