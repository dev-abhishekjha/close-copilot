package web

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

// Role is a web user's role. Roles are explicit sets, not a ladder: a maker
// proposes and a checker approves (separation of duties), so a checker is
// not a "bigger" maker. Each route lists the roles it allows.
type Role string

// The three roles of config/users.yaml.
const (
	RoleViewer  Role = "viewer"
	RoleMaker   Role = "maker"
	RoleChecker Role = "checker"
)

// Valid reports whether r is one of the three known roles.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleMaker, RoleChecker:
		return true
	}
	return false
}

// MinBcryptCost is the lowest bcrypt cost a password hash may use.
const MinBcryptCost = 10

// User is one entry of config/users.yaml. PasswordHash is bcrypt; TokenSHA256
// is the lower-case hex SHA-256 of the user's bearer token, or empty when
// bearer auth is off for the user. Neither is ever logged or rendered.
type User struct {
	Name         string
	Role         Role
	PasswordHash string
	TokenSHA256  string
}

// Users is a validated, immutable set of users keyed by username.
type Users struct {
	byName map[string]User
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NewUsers validates list and returns it as a Users set. It rejects a bad or
// duplicate username, an unknown role, an empty or non-bcrypt hash, a hash
// with cost below MinBcryptCost, a token_sha256 that is set but not 64 hex
// characters, and two users sharing a token.
func NewUsers(list ...User) (Users, error) {
	byName := make(map[string]User, len(list))
	tokens := make(map[string]string, len(list))
	for i, u := range list {
		if !usernamePattern.MatchString(u.Name) {
			return Users{}, fmt.Errorf("web: user %d: username %q must match %s", i, u.Name, usernamePattern)
		}
		if _, dup := byName[u.Name]; dup {
			return Users{}, fmt.Errorf("web: user %q is listed twice", u.Name)
		}
		if !u.Role.Valid() {
			return Users{}, fmt.Errorf("web: user %q: unknown role %q (want viewer, maker or checker)", u.Name, u.Role)
		}
		if u.PasswordHash == "" {
			return Users{}, fmt.Errorf("web: user %q: password_hash is empty", u.Name)
		}
		cost, err := bcrypt.Cost([]byte(u.PasswordHash))
		if err != nil {
			return Users{}, fmt.Errorf("web: user %q: password_hash is not a bcrypt hash", u.Name)
		}
		if cost < MinBcryptCost {
			return Users{}, fmt.Errorf("web: user %q: bcrypt cost %d is below %d", u.Name, cost, MinBcryptCost)
		}
		if u.TokenSHA256 != "" {
			raw, err := hex.DecodeString(u.TokenSHA256)
			if err != nil || len(raw) != 32 {
				return Users{}, fmt.Errorf("web: user %q: token_sha256 must be 64 hex characters", u.Name)
			}
			u.TokenSHA256 = strings.ToLower(u.TokenSHA256)
			if other, dup := tokens[u.TokenSHA256]; dup {
				return Users{}, fmt.Errorf("web: users %q and %q share a token_sha256", other, u.Name)
			}
			tokens[u.TokenSHA256] = u.Name
		}
		byName[u.Name] = u
	}
	return Users{byName: byName}, nil
}

// Lookup returns the user called name.
func (us Users) Lookup(name string) (User, bool) {
	u, ok := us.byName[name]
	return u, ok
}

// Len is the number of users.
func (us Users) Len() int { return len(us.byName) }

// All returns the users sorted by name.
func (us Users) All() []User {
	out := make([]User, 0, len(us.byName))
	for _, u := range us.byName {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// maxCost is the highest bcrypt cost among the users' hashes, or
// bcrypt.DefaultCost when there are none. The dummy hash for unknown
// usernames uses it so a failed login takes as long either way.
func (us Users) maxCost() int {
	best := 0
	for _, u := range us.byName {
		if c, err := bcrypt.Cost([]byte(u.PasswordHash)); err == nil && c > best {
			best = c
		}
	}
	if best == 0 {
		return bcrypt.DefaultCost
	}
	return best
}

// usersFile is the YAML shape of config/users.yaml.
type usersFile struct {
	Users []struct {
		Username     string `yaml:"username"`
		PasswordHash string `yaml:"password_hash"`
		Role         string `yaml:"role"`
		TokenSHA256  string `yaml:"token_sha256"`
	} `yaml:"users"`
}

// LoadUsers reads and validates the users file at path (config/users.yaml).
func LoadUsers(path string) (Users, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Users{}, fmt.Errorf("web: read users file: %w", err)
	}
	us, err := parseUsers(data)
	if err != nil {
		return Users{}, fmt.Errorf("web: users file %s: %w", path, err)
	}
	return us, nil
}

// parseUsers decodes and validates a users file. Unknown fields are errors,
// so a typo such as "pasword_hash" can't silently drop a credential.
func parseUsers(data []byte) (Users, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f usersFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return Users{}, errors.New("web: users file is empty")
		}
		return Users{}, fmt.Errorf("web: decode users: %w", err)
	}
	if len(f.Users) == 0 {
		return Users{}, errors.New("web: users file lists no users")
	}
	list := make([]User, 0, len(f.Users))
	for _, u := range f.Users {
		list = append(list, User{
			Name:         u.Username,
			Role:         Role(u.Role),
			PasswordHash: u.PasswordHash,
			TokenSHA256:  u.TokenSHA256,
		})
	}
	return NewUsers(list...)
}
