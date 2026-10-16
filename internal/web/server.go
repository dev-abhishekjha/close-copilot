package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// HTTP server timeouts.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
	maxHeaderBytes    = 64 << 10
	defaultAddr       = ":8000"
)

//go:embed static
var staticFiles embed.FS

// Options configure a Server.
type Options struct {
	// Addr is the listen address (APP_ADDR). Empty means ":8000".
	Addr string
	// SessionKey is APP_SESSION_KEY: the raw string, at least 32 bytes.
	SessionKey config.Secret
	// InsecureCookies drops the Secure flag from cookies. For tests only:
	// the default is Secure, and it is never inferred from the request.
	InsecureCookies bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Runs backs the close-run pages and API (CC-1002). Nil means not
	// configured: those routes answer 501 after the role check.
	Runs *RunsBackend
	// Ask backs POST /api/ask (CC-1006). Nil means not configured: the route
	// answers 501 after the role check. It also needs Runs, whose store
	// lists the companies a question may name.
	Ask AskBackend
	// Approvals backs the maker-checker routes and the /approvals page
	// (CC-1004). Nil means not configured: they answer 501 after the role
	// check.
	Approvals ApprovalsBackend
	// Audit backs the audit log page and the run metrics page (CC-1005).
	// Nil means not configured: they answer 501 after the role check.
	Audit AuditStore
	// ERPBaseURL is ERP_BASE_URL, the base of the links to ERPNext pages.
	// Empty or not an http(s) URL with a host means no links.
	ERPBaseURL string
	// DevComponents serves /dev/components; never set by production wiring.
	DevComponents bool

	// Rate limits (CC-1102). Zero means the default: API 5 requests a
	// second per user, burst 10; ask 1 a second, burst 3; 10 failed logins
	// per IP a minute; 5 failed logins per username in 15 minutes.
	APIRate           float64
	APIBurst          int
	AskRate           float64
	AskBurst          int
	LoginIPFailures   int
	LoginUserFailures int
}

// OptionsFromConfig takes APP_ADDR and APP_SESSION_KEY from cfg.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{Addr: cfg.AppAddr, SessionKey: cfg.AppSessionKey, ERPBaseURL: cfg.ERPBaseURL}
}

// Server is the web UI and JSON API. It is an http.Handler.
type Server struct {
	addr          string
	keys          keys
	users         atomic.Pointer[Users]
	log           *slog.Logger
	secureCookies bool
	now           func() time.Time
	routes        []route
	handler       http.Handler
	runs          *runsService     // nil when Options.Runs is nil
	ask           AskBackend       // nil when Options.Ask is nil
	approvals     ApprovalsBackend // nil when Options.Approvals is nil
	audit         AuditStore       // nil when Options.Audit is nil
	erpBase       string
	devComponents bool
	limits        *apiLimits
	logins        *loginLimiter
	bcryptHook    func() // tests only: called before each login password compare
}

// NewServer builds the server. It refuses a session key shorter than 32
// bytes and an empty users set.
func NewServer(opts Options, users Users, log *slog.Logger) (*Server, error) {
	k, err := deriveKeys(opts.SessionKey)
	if err != nil {
		return nil, err
	}
	if users.Len() == 0 {
		return nil, errors.New("web: no users")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		addr:          opts.Addr,
		keys:          k,
		log:           log,
		secureCookies: !opts.InsecureCookies,
		now:           opts.Now,
		erpBase:       opts.ERPBaseURL,
		devComponents: opts.DevComponents,
		ask:           opts.Ask,
		approvals:     opts.Approvals,
		audit:         opts.Audit,
		limits:        newAPILimits(opts),
		logins:        newLoginLimiter(opts),
	}
	if s.addr == "" {
		s.addr = defaultAddr
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.users.Store(&users)
	if opts.Runs != nil {
		if opts.Runs.Store == nil || opts.Runs.Execute == nil {
			return nil, errors.New("web: runs backend needs a store and an executor")
		}
		s.runs = newRunsService(*opts.Runs, log, s.now)
	}
	// Warm the dummy-hash cache so the first unknown-user login isn't slower.
	if _, err := dummyHash(users.maxCost()); err != nil {
		return nil, err
	}

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static files: %w", err)
	}
	s.routes = s.routeTable(http.StripPrefix("/static/", noDirListing(http.FileServerFS(static))))
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		mux.Handle(rt.method+" "+rt.path, s.wrap(rt))
	}
	s.handler = s.baseMiddleware(mux)
	return s, nil
}

// SetUsers replaces the users set (for example after the owner edits
// config/users.yaml). Sessions of removed users or users whose hashes changed
// end at their next request.
func (s *Server) SetUsers(users Users) error {
	if users.Len() == 0 {
		return errors.New("web: no users")
	}
	if _, err := dummyHash(users.maxCost()); err != nil {
		return err
	}
	s.users.Store(&users)
	return nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Run listens on the configured address and serves until ctx is cancelled.
// A bind error is returned before "listening" is logged.
func (s *Server) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully,
// letting in-flight requests finish. It closes ln.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.runs != nil {
		// Runs started from here on stop when ctx is cancelled; Serve waits
		// for them (bounded) after the HTTP server has stopped. A run cut
		// short stays "running" and can be resumed.
		runCtx, cancel := context.WithCancel(ctx)
		s.runs.setBase(runCtx)
		defer func() {
			cancel()
			s.runs.drain(shutdownTimeout)
		}()
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	addr := ln.Addr().String()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.InfoContext(ctx, "web: listening", "addr", addr)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("web: serve %s: %w", addr, err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("web: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web: serve %s: %w", addr, err)
	}
	return nil
}

// render writes a templ component as HTML.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.log.ErrorContext(r.Context(), "web: render", "path", r.URL.Path, "err", err)
	}
}

// noDirListing answers 404 for directory paths instead of listing them.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || r.URL.Path[len(r.URL.Path)-1] == '/' {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
