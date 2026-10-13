package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxBodyBytes caps every request body.
const maxBodyBytes = 1 << 20

// contentSecurityPolicy allows only this origin. htmx is vendored under
// /static/ (and loaded with Subresource Integrity), so no third-party script
// origin is needed. No inline scripts or styles, no eval.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

type authMethod string

const (
	authNone   authMethod = "none"
	authCookie authMethod = "cookie"
	authBearer authMethod = "bearer"
)

// principal is the authenticated caller of one request. csrf is the
// session's CSRF token (empty for bearer).
type principal struct {
	user   User
	method authMethod
	csrf   string
}

type ctxKey int

const (
	principalKey ctxKey = iota
	requestInfoKey
)

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey).(*principal)
	return p
}

// requestInfo collects what the request log line reports. Handlers deeper in
// the chain fill it in; it holds no cookies, tokens or form values.
type requestInfo struct {
	user  string
	role  Role
	auth  authMethod
	event string
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	ri, _ := ctx.Value(requestInfoKey).(*requestInfo)
	return ri
}

func isAPI(r *http.Request) bool {
	return r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// statusRecorder remembers the response status for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// baseMiddleware wraps every request: security headers, body limit, panic
// recovery and one request log line.
func (s *Server) baseMiddleware(next http.Handler) http.Handler {
	return s.logRequests(s.securityHeaders(s.recoverPanics(limitBody(next))))
}

// securityHeaders sets the headers every response carries.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a handler panic into a generic 500. The panic value is
// not logged (it could carry request data); only its type is.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.log.ErrorContext(r.Context(), "web: handler panic", "path", r.URL.Path, "panic_type", fmt.Sprintf("%T", v))
			if isAPI(r) {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
				return
			}
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// logRequests writes one JSON log line per request with method, path,
// status, duration, user, role and auth method. It never logs cookies,
// headers, query strings, tokens or form bodies.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{auth: authNone}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestInfoKey, info)))
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"user", info.user,
			"role", string(info.role),
			"auth", string(info.auth),
		}
		if info.event != "" {
			attrs = append(attrs, "event", info.event)
		}
		s.log.InfoContext(r.Context(), "web: request", attrs...)
	})
}

// authenticate resolves the caller and refuses anonymous requests.
//
// On /api/* an Authorization header, when present, decides alone: a known
// bearer token authenticates, anything else is 401 with no fallback to the
// cookie. Elsewhere the Authorization header is ignored and only the session
// cookie counts. Anonymous HTML requests are redirected (303) to the login
// page; anonymous API requests get 401 JSON.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p *principal
		if _, hasAuth := r.Header["Authorization"]; hasAuth && isAPI(r) {
			bp, ok := s.bearerPrincipal(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w, r)
				return
			}
			p = bp
		} else {
			p = s.sessionPrincipal(r)
		}
		if p == nil {
			unauthorized(w, r)
			return
		}
		if info := requestInfoFrom(r.Context()); info != nil {
			info.user, info.role, info.auth = p.user.Name, p.user.Role, p.method
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// csrfProtect requires the session's CSRF token on every unsafe request
// authenticated by the cookie, whatever its Content-Type, and refuses
// cross-site requests. Bearer-authenticated requests skip it: a browser
// never attaches a bearer token on its own.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			unauthorized(w, r)
			return
		}
		if isSafeMethod(r.Method) || p.method == authBearer {
			next.ServeHTTP(w, r)
			return
		}
		if crossSite(r) || !validCSRF(presentedCSRF(r), p.csrf) {
			forbidden(w, r, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireRole lets the request through only when the authenticated user's
// role is one of roles. Roles are an explicit allowed set, not a ladder.
// Unauthenticated requests get 401 (or the login redirect); authenticated
// ones without the role get 403.
func requireRole(roles ...Role) func(http.Handler) http.Handler {
	allowed := slices.Clone(roles)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := principalFrom(r.Context())
			if p == nil {
				unauthorized(w, r)
				return
			}
			if !slices.Contains(allowed, p.user.Role) {
				forbidden(w, r, "your role may not do this")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// unauthorized answers an anonymous (or bad-bearer) request: 401 JSON on
// /api/*, otherwise a 303 to the login page carrying a safe next path.
func unauthorized(w http.ResponseWriter, r *http.Request) {
	if isAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="close-copilot"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	target := "/login"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if next := safeNext(r.URL.RequestURI()); next != "/" {
			target += "?next=" + url.QueryEscape(next)
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// forbidden answers 403: JSON on /api/*, plain text elsewhere.
func forbidden(w http.ResponseWriter, r *http.Request, reason string) {
	if isAPI(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "reason": reason})
		return
	}
	http.Error(w, "forbidden: "+reason, http.StatusForbidden)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status is already sent; a write error means the client left
}
