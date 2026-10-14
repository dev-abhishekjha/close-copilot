package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

const (
	// Cookie names. By default (Secure cookies) they carry the browser-enforced
	// prefixes: __Host- pins the session cookie to this host with Secure,
	// Path=/ and no Domain; __Secure- requires Secure on the login CSRF cookie.
	// The plain names are used only with Options.InsecureCookies (tests),
	// because browsers reject prefixed cookies without Secure.
	sessionCookieSecure     = "__Host-cc_session"
	sessionCookieInsecure   = "cc_session"
	loginCSRFCookieSecure   = "__Secure-cc_login_csrf"
	loginCSRFCookieInsecure = "cc_login_csrf"

	csrfHeaderName = "X-CSRF-Token"
	csrfFieldName  = "csrf_token"

	// sessionTTL is the server-side session lifetime, checked from the
	// signed payload on every request.
	sessionTTL = 8 * time.Hour
	// loginCSRFTTL bounds how long a login form stays usable.
	loginCSRFTTL = time.Hour
	// minSessionKeyLen is the shortest APP_SESSION_KEY NewServer accepts.
	minSessionKeyLen = 32
	// maxCookieLen caps a session cookie before any decoding.
	maxCookieLen = 2048
	// maxPasswordLen is bcrypt's input limit.
	maxPasswordLen = 72

	sessionVersion = 1
	invalidLogin   = "invalid username or password"
)

// keys are independent HMAC keys derived from APP_SESSION_KEY, one per use,
// so a value minted for one purpose never verifies for another.
type keys struct {
	session   []byte
	csrf      []byte
	fp        []byte
	loginCSRF []byte
}

// deriveKeys checks the session key's length and derives the per-purpose
// keys. This is the only place the raw key is revealed; it is never logged
// and never appears in an error.
func deriveKeys(secret config.Secret) (keys, error) {
	raw := []byte(secret.Reveal())
	if len(raw) < minSessionKeyLen {
		return keys{}, fmt.Errorf("web: %s must be at least %d bytes (got %d)", config.EnvAppSessionKey, minSessionKeyLen, len(raw))
	}
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, raw)
		m.Write([]byte("close-copilot/web/" + label))
		return m.Sum(nil)
	}
	return keys{
		session:   derive("session/v1"),
		csrf:      derive("csrf/v1"),
		fp:        derive("fingerprint/v1"),
		loginCSRF: derive("login-csrf/v1"),
	}, nil
}

func mac(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		m.Write(n[:])
		m.Write(p)
	}
	return m.Sum(nil)
}

var b64 = base64.RawURLEncoding

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return b64.EncodeToString(b)
}

// sessionClaims is the signed session payload. The role is deliberately
// absent: it is re-read from the users file on every request.
type sessionClaims struct {
	V   int    `json:"v"`
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	FP  string `json:"fp"`
	N   string `json:"n"`
}

// fingerprint is a short keyed digest of the user's credentials. Changing
// the password hash or the bearer token hash changes it and so ends every
// session issued before the change. Keyed, so the cookie reveals nothing
// about the hash.
func (k keys) fingerprint(u User) string {
	sum := mac(k.fp, []byte(u.Name), []byte(u.PasswordHash), []byte(u.TokenSHA256))
	return b64.EncodeToString(sum[:12])
}

// newSession mints a session cookie for u and returns it with the session's
// CSRF token.
func (s *Server) newSession(u User) (*http.Cookie, string) {
	now := s.now()
	c := sessionClaims{
		V:   sessionVersion,
		Sub: u.Name,
		Iat: now.Unix(),
		Exp: now.Add(sessionTTL).Unix(),
		FP:  s.keys.fingerprint(u),
		N:   randomToken()[:16],
	}
	payload, _ := json.Marshal(c) // a struct of strings and ints always marshals
	p := b64.EncodeToString(payload)
	sig := mac(s.keys.session, []byte(p))
	cookie := s.cookie(s.sessionCookieName(), p+"."+b64.EncodeToString(sig), "/", int(sessionTTL/time.Second), now.Add(sessionTTL))
	return cookie, s.csrfToken(sig)
}

// csrfToken is the per-session CSRF token: a keyed digest of the session's
// signature, so it changes with every login and needs no server state.
func (s *Server) csrfToken(sig []byte) string {
	return b64.EncodeToString(mac(s.keys.csrf, sig))
}

// verifySession checks a session cookie's signature (in constant time),
// version and expiry, and returns its claims and signature.
func (s *Server) verifySession(value string) (sessionClaims, []byte, bool) {
	if value == "" || len(value) > maxCookieLen {
		return sessionClaims{}, nil, false
	}
	p, sigText, ok := strings.Cut(value, ".")
	if !ok || strings.Contains(sigText, ".") {
		return sessionClaims{}, nil, false
	}
	sig, err := b64.DecodeString(sigText)
	if err != nil || !hmac.Equal(sig, mac(s.keys.session, []byte(p))) {
		return sessionClaims{}, nil, false
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return sessionClaims{}, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var c sessionClaims
	if err := dec.Decode(&c); err != nil || c.V != sessionVersion || c.Sub == "" {
		return sessionClaims{}, nil, false
	}
	now := s.now().Unix()
	ttl := int64(sessionTTL / time.Second)
	if now >= c.Exp || c.Exp-c.Iat > ttl || now-c.Iat >= ttl || c.Iat > now+60 {
		return sessionClaims{}, nil, false
	}
	return c, sig, true
}

// sessionPrincipal resolves the request's session cookie to a principal. A
// removed user or a changed fingerprint yields nil (anonymous).
func (s *Server) sessionPrincipal(r *http.Request) *principal {
	ck, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return nil
	}
	c, sig, ok := s.verifySession(ck.Value)
	if !ok {
		return nil
	}
	u, ok := s.users.Load().Lookup(c.Sub)
	if !ok {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(c.FP), []byte(s.keys.fingerprint(u))) != 1 {
		return nil
	}
	return &principal{user: u, method: authCookie, csrf: s.csrfToken(sig)}
}

// bearerPrincipal resolves an Authorization header on /api/*. It returns
// ok=false for anything but a known "Bearer <token>": the caller answers 401
// and does not fall back to the cookie.
func (s *Server) bearerPrincipal(header string) (*principal, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	presented := []byte(hex.EncodeToString(sum[:]))
	var match *User
	for _, u := range s.users.Load().All() {
		if u.TokenSHA256 == "" {
			continue // bearer is off for this user
		}
		if subtle.ConstantTimeCompare(presented, []byte(u.TokenSHA256)) == 1 {
			match = &u
		}
	}
	if match == nil {
		return nil, false
	}
	return &principal{user: *match, method: authBearer}, true
}

// crossSite reports whether the request's Fetch metadata or Origin header
// shows it came from another origin. Absent headers are not evidence either
// way; the CSRF token still has to match.
func crossSite(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" {
		return v != "same-origin"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return true // includes "null"
		}
		return !strings.EqualFold(u.Host, r.Host)
	}
	return false
}

// validCSRF compares a presented CSRF token with the expected one in
// constant time.
func validCSRF(presented, want string) bool {
	if presented == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// presentedCSRF reads the CSRF token from the X-CSRF-Token header or, failing
// that, the csrf_token form field of a form-encoded body. The query string is
// never consulted.
func presentedCSRF(r *http.Request) string {
	if v := r.Header.Get(csrfHeaderName); v != "" {
		return v
	}
	return r.PostFormValue(csrfFieldName)
}

// loginCSRFToken is the form token paired with a login-CSRF cookie nonce
// (signed double submit).
func (s *Server) loginCSRFToken(nonce string) string {
	return b64.EncodeToString(mac(s.keys.loginCSRF, []byte(nonce)))
}

// dummyHashes holds one bcrypt hash per cost for unknown-username logins.
var dummyHashes sync.Map // int -> []byte

func dummyHash(cost int) ([]byte, error) {
	if h, ok := dummyHashes.Load(cost); ok {
		return h.([]byte), nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(randomToken()), cost)
	if err != nil {
		return nil, fmt.Errorf("web: dummy bcrypt hash: %w", err)
	}
	actual, _ := dummyHashes.LoadOrStore(cost, h)
	return actual.([]byte), nil
}

// handleLoginForm serves GET /login and issues the pre-session CSRF cookie.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	nonce := s.loginNonce(w, r)
	s.renderLogin(w, r, http.StatusOK, nonce, r.URL.Query().Get("next"), "")
}

// loginNonce returns the request's login-CSRF cookie nonce, issuing a new
// one when it is missing or malformed. Reusing a good one keeps two open
// login tabs working.
func (s *Server) loginNonce(w http.ResponseWriter, r *http.Request) string {
	if ck, err := r.Cookie(s.loginCSRFCookieName()); err == nil {
		if raw, err := b64.DecodeString(ck.Value); err == nil && len(raw) == 32 {
			return ck.Value
		}
	}
	nonce := randomToken()
	http.SetCookie(w, s.cookie(s.loginCSRFCookieName(), nonce, "/login", int(loginCSRFTTL/time.Second), time.Time{}))
	return nonce
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, nonce, next, errMsg string) {
	s.render(w, r, status, templates.Login(templates.LoginPage{
		CSRFToken: s.loginCSRFToken(nonce),
		Next:      safeNext(next),
		Error:     errMsg,
	}))
}

// handleLogin serves POST /login. It needs the pre-session CSRF token and a
// same-origin request, checks the password with bcrypt (running a compare
// against a dummy hash for unknown users so timing doesn't reveal which
// usernames exist), and on success sets the session cookie and redirects to
// a safe next path.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		forbidden(w, r, "cross-site login refused")
		return
	}
	ck, err := r.Cookie(s.loginCSRFCookieName())
	if err != nil || !validCSRF(r.PostFormValue(csrfFieldName), s.loginCSRFToken(ck.Value)) {
		forbidden(w, r, "missing or invalid login form token; reload the login page")
		return
	}
	name := r.PostFormValue("username")
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")

	// Refuse a locked IP or username before the bcrypt compare, with the
	// same answer whether or not the user exists.
	// begin reserves the attempt under one lock, so concurrent guesses
	// cannot all pass before the first failure is counted.
	allowed, wait, done := s.logins.begin(clientIP(r), name, s.now())
	if !allowed {
		s.refuse(w, r, wait, "login_locked")
		return
	}
	success := false
	defer func() { done(success) }()

	users := s.users.Load()
	u, known := users.Lookup(name)
	hash := []byte(u.PasswordHash)
	if !known {
		if hash, err = dummyHash(users.maxCost()); err != nil {
			s.log.ErrorContext(r.Context(), "web: login", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	if s.bcryptHook != nil {
		s.bcryptHook()
	}
	pwOK := len(password) <= maxPasswordLen && bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	if !known || !pwOK {
		if info := requestInfoFrom(r.Context()); info != nil {
			info.event = "login_failed"
		}
		s.renderLogin(w, r, http.StatusUnauthorized, ck.Value, next, invalidLogin)
		return
	}

	success = true
	cookie, _ := s.newSession(u)
	http.SetCookie(w, cookie)
	http.SetCookie(w, s.expired(s.loginCSRFCookieName(), "/login"))
	if info := requestInfoFrom(r.Context()); info != nil {
		info.user, info.role, info.event = u.Name, u.Role, "login"
	}
	// G710 false positive: safeNext allows only same-site relative paths (TestOpenRedirect).
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther) //nolint:gosec // G710: sanitised by safeNext
}

// handleLogout serves POST /logout (authenticated and CSRF-checked by the
// middleware). Sessions are stateless, so logout only clears the browser's
// copy of the cookie: a copied cookie stays valid until it expires or the
// user's password hash or token hash changes.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.expired(s.sessionCookieName(), "/"))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// sessionCookieName is the session cookie's name for this server's cookie mode.
func (s *Server) sessionCookieName() string {
	if s.secureCookies {
		return sessionCookieSecure
	}
	return sessionCookieInsecure
}

// loginCSRFCookieName is the login CSRF cookie's name for this server's
// cookie mode.
func (s *Server) loginCSRFCookieName() string {
	if s.secureCookies {
		return loginCSRFCookieSecure
	}
	return loginCSRFCookieInsecure
}

// expired is a cookie that deletes name at path.
func (s *Server) expired(name, path string) *http.Cookie {
	return s.cookie(name, "", path, -1, time.Unix(0, 0))
}

// cookie builds a cookie with the flags every cookie here carries: HttpOnly,
// SameSite=Lax and Secure. Secure is dropped only when the server was built
// with Options.InsecureCookies (tests); it is never inferred from the request.
func (s *Server) cookie(name, value, path string, maxAge int, expires time.Time) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // G124: Secure defaults to true; see Options.InsecureCookies
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	if !s.secureCookies {
		c.Secure = false
	}
	return c
}

// safeNext returns next when it is a same-site relative path and "/"
// otherwise. It rejects protocol-relative paths ("//host"), schemes,
// backslashes, control characters and encoded forms of those.
func safeNext(next string) string {
	if next == "" || len(next) > 2048 || next[0] != '/' {
		return "/"
	}
	decoded, err := url.PathUnescape(next)
	if err != nil {
		return "/"
	}
	for _, v := range []string{next, decoded} {
		if strings.HasPrefix(v, "//") || strings.ContainsRune(v, '\\') {
			return "/"
		}
		for _, c := range v {
			if c < 0x20 || c == 0x7f {
				return "/"
			}
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	return next
}
