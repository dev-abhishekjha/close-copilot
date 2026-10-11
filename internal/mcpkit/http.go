package mcpkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/telemetry"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// RequestIDFromContext extracts the request ID from a context, if present.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// Option configures BuildHandler and ServeHTTP.
type Option func(*handlerOptions)

type handlerOptions struct {
	actors map[string]string
}

// WithActors maps a route path to the audit actor of calls on it, for example
// "/mcp" to "agent" and "/mcp-admin" to "admin". The actor is set server-side
// from which route authenticated, never from the request body.
func WithActors(actors map[string]string) Option {
	return func(o *handlerOptions) { o.actors = actors }
}

// BuildHandler constructs an http.Handler with health check, routes, and middleware chain.
func BuildHandler(routes map[string]Route, log *slog.Logger, opts ...Option) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	var o handlerOptions
	for _, opt := range opts {
		opt(&o)
	}

	mux := http.NewServeMux()

	// /healthz returns 200 OK without requiring authentication
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	for path, r := range routes {
		server := r.Server
		traceToolCalls(server)
		streamHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return server
		}, nil)

		// Authenticate with Bearer token
		var inner http.Handler = streamHandler
		if actor, ok := o.actors[path]; ok {
			inner = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				streamHandler.ServeHTTP(w, req.WithContext(ContextWithActor(req.Context(), actor)))
			})
		}
		authed := RequireBearer(r.Token, inner)
		mux.Handle(path, authed)
	}

	// Wrap mux with global middleware: Request ID, Slog Access Log, Recovery
	return telemetry.WrapHandler(withRecovery(withLogging(withRequestID(mux), log), log))
}

// ServeHTTP starts an HTTP server serving the given routes and shuts down
// gracefully with a 10-second timeout when ctx is cancelled.
func ServeHTTP(ctx context.Context, addr string, routes map[string]Route, log *slog.Logger, opts ...Option) error {
	if log == nil {
		log = slog.Default()
	}

	handler := BuildHandler(routes, log, opts...)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("mcp server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("mcpkit: listen and serve: %w", err)
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down mcp server gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("mcpkit: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// withRequestID injects X-Request-ID into context and response headers.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			var b [16]byte
			_, _ = rand.Read(b[:])
			reqID = hex.EncodeToString(b[:])
		}

		w.Header().Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withLogging logs incoming HTTP requests and response latency.
func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w}

		next.ServeHTTP(rw, r)

		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}

		reqID := RequestIDFromContext(r.Context())
		latency := time.Since(start)

		log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"latency_ms", latency.Milliseconds(),
			"request_id", reqID,
			"remote_addr", r.RemoteAddr,
		)
	})
}

// withRecovery catches panics, logs stack traces, and returns HTTP 500.
func withRecovery(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				reqID := RequestIDFromContext(r.Context())
				log.Error("recovered from panic in http handler",
					"error", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
					"request_id", reqID,
					"path", r.URL.Path,
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher to support streaming MCP responses.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traced remembers the servers that already carry the tool-call span
// middleware, so building a handler twice never doubles the spans.
var traced sync.Map

var toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// traceToolCalls makes every tools/call on s record an
// "mcp.tools/call <tool>" span under the caller's trace. The span holds the
// tool name and method only, never the arguments or the result.
func traceToolCalls(s *mcp.Server) {
	if s == nil {
		return
	}
	if _, loaded := traced.LoadOrStore(s, struct{}{}); loaded {
		return
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			tool := "unknown"
			// The name is client-supplied: use it only when it looks like a
			// tool name, so identifiers never reach a span name.
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && toolNameRE.MatchString(call.Params.Name) {
				tool = call.Params.Name
			}
			ctx, span := telemetry.StartTool(ctx, tool)
			defer span.End()
			res, err := next(ctx, method, req)
			if err != nil {
				telemetry.Fail(span, "tool call failed")
			} else if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				telemetry.Fail(span, "tool returned an error result")
			}
			return res, err
		}
	})
}
