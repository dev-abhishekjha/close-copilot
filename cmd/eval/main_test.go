package main

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/mcpkit"
)

type ctxKey struct{}

func TestPrimeScope(t *testing.T) {
	const key = "test-scope-key"
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	withScope := func(ctx context.Context, h string) context.Context { return context.WithValue(ctx, ctxKey{}, h) }
	run := func(key, company, month string, calleeErr error) (hdr string, scoped bool, err error) {
		f := primeScope(key, func() time.Time { return now }, withScope, func(ctx context.Context, _, _ string) error {
			hdr, scoped = ctx.Value(ctxKey{}).(string)
			return calleeErr
		})
		err = f(t.Context(), company, month)
		return
	}

	t.Run("scope matches the month", func(t *testing.T) {
		hdr, scoped, err := run(key, "sharma", "2026-09", nil)
		if err != nil || !scoped {
			t.Fatalf("err=%v scoped=%v", err, scoped)
		}
		sc, err := mcpkit.Verify([]byte(key), hdr, now)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := mcpkit.Window("sharma", sc.RunID, "2026-09", time.Unix(sc.Exp, 0))
		if sc != want {
			t.Errorf("scope = %+v, want %+v", sc, want)
		}
		if !regexp.MustCompile(`^eval-prime-[A-Za-z0-9._:-]{1,50}$`).MatchString(sc.RunID) {
			t.Errorf("run ID %q", sc.RunID)
		}
		if d := time.Unix(sc.Exp, 0).Sub(now); d <= 0 || d > 10*time.Minute {
			t.Errorf("expiry %v from now", d)
		}
		if sc.Company != "sharma" || sc.From != "2025-09-01" || sc.To != "2026-10-31" {
			t.Errorf("scope = %+v", sc)
		}
	})
	t.Run("fresh run ID each call", func(t *testing.T) {
		a, _, _ := run(key, "sharma", "2026-09", nil)
		b, _, _ := run(key, "sharma", "2026-09", nil)
		if a == b {
			t.Error("two calls shared a scope header")
		}
	})
	t.Run("empty key passes ctx through", func(t *testing.T) {
		_, scoped, err := run("", "sharma", "2026-09", nil)
		if err != nil || scoped {
			t.Errorf("err=%v scoped=%v, want an unchanged ctx", err, scoped)
		}
	})
	t.Run("bad month errors without a call", func(t *testing.T) {
		called := false
		f := primeScope(key, func() time.Time { return now }, withScope, func(context.Context, string, string) error { called = true; return nil })
		if err := f(t.Context(), "sharma", "2026-13"); err == nil || called {
			t.Errorf("err=%v called=%v", err, called)
		}
	})
	t.Run("bad company errors", func(t *testing.T) {
		if _, _, err := run(key, "", "2026-09", nil); err == nil {
			t.Error("empty company accepted")
		}
	})
	t.Run("callee error is returned", func(t *testing.T) {
		boom := errors.New("books tool: call outside the run scope refused")
		if _, _, err := run(key, "sharma", "2026-09", boom); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
}
