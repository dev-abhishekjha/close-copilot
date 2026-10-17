// Command e2eserver serves the in-memory fixture for the browser suite. It
// binds to a loopback address only and uses non-Secure cookies, so it must
// never be exposed.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/abhishekjha/close-copilot/e2e/fixture"
)

const defaultAddr = "127.0.0.1:8099"

func main() {
	if err := run(); err != nil {
		slog.Error("e2eserver", "err", err)
		os.Exit(1)
	}
}

// checkLoopback refuses any address that is not a loopback IP literal or
// "localhost".
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to bind to %q: the fixture serves plain-http cookies and must stay on loopback", addr)
}

func run() error {
	addr := os.Getenv("E2E_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if err := checkLoopback(addr); err != nil {
		return err
	}
	h, err := fixture.New(false)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("e2e fixture listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
