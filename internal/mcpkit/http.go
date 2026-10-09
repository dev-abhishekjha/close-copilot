package mcpkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// BuildHandler constructs an http.Handler with health check, routes, and middleware chain.
func BuildHandler(routes map[string]Route, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
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
		streamHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return server
		}, nil)

		// Authenticate with Bearer token
		authed := RequireBearer(r.Token, streamHandler)
		mux.Handle(path, authed)
	}

	// Wrap mux with global middleware: Request ID, Slog Access Log, Recovery
	return withRecovery(withLogging(withRequestID(mux), log), log)
}

// ServeHTTP starts an HTTP server serving the given routes and shuts down
// gracefully with a 10-second timeout when ctx is cancelled.
func ServeHTTP(ctx context.Context, addr string, routes map[string]Route, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	handler := BuildHandler(routes, log)
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
