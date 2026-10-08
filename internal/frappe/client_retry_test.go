package frappe

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	// failFirst answers status (with header) n times, then 200.
	failFirst := func(n int32, status int, header map[string]string) (http.Handler, *atomic.Int32) {
		var calls atomic.Int32
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= n {
				for k, v := range header {
					w.Header().Set(k, v)
				}
				writeRaw(w, status, `{"exc_type":"SessionStopped"}`)
				return
			}
			writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
		}), &calls
	}

	t.Run("503 retried then OK", func(t *testing.T) {
		h, calls := failFirst(2, http.StatusServiceUnavailable, nil)
		c, _ := newTestClient(t, h)
		var w waits
		c.sleep = w.sleep
		doc, err := Get[map[string]any](t.Context(), c, "Account", "ok")
		if err != nil {
			t.Fatal(err)
		}
		if doc["name"] != "ok" || calls.Load() != 3 {
			t.Errorf("doc = %v after %d calls, want ok after 3", doc, calls.Load())
		}
		got := w.get()
		if len(got) != 2 {
			t.Fatalf("waits = %v, want 2", got)
		}
		if got[0] < backoffBase/2 || got[0] > backoffBase {
			t.Errorf("first wait %v outside [%v, %v]", got[0], backoffBase/2, backoffBase)
		}
		if got[1] < backoffBase || got[1] > 2*backoffBase {
			t.Errorf("second wait %v outside [%v, %v]", got[1], backoffBase, 2*backoffBase)
		}
	})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status)+" retried", func(t *testing.T) {
			h, calls := failFirst(1, status, nil)
			c, _ := newTestClient(t, h)
			if _, err := Get[map[string]any](t.Context(), c, "Account", "ok"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Errorf("calls = %d, want 2", calls.Load())
			}
		})
	}

	t.Run("500 not retried", func(t *testing.T) {
		h, calls := failFirst(1, http.StatusInternalServerError, nil)
		c, _ := newTestClient(t, h)
		_, err := Get[map[string]any](t.Context(), c, "Account", "ok")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusInternalServerError {
			t.Fatalf("err = %v, want an APIError 500", err)
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})

	retryAfter := []struct {
		name, value string
		lo, hi      time.Duration
	}{
		{"Retry-After seconds", "2", 2 * time.Second, 2 * time.Second},
		{"Retry-After capped", "3600", retryAfterCap, retryAfterCap},
		{"Retry-After date", time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat), 8 * time.Second, 10 * time.Second},
		{"Retry-After garbage falls back to backoff", "soon", backoffBase / 2, backoffBase},
	}
	for _, tt := range retryAfter {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := failFirst(1, http.StatusTooManyRequests, map[string]string{"Retry-After": tt.value})
			c, _ := newTestClient(t, h)
			var w waits
			c.sleep = w.sleep
			if _, err := Get[map[string]any](t.Context(), c, "Account", "ok"); err != nil {
				t.Fatal(err)
			}
			got := w.get()
			if len(got) != 1 || got[0] < tt.lo || got[0] > tt.hi {
				t.Errorf("waits = %v, want one in [%v, %v]", got, tt.lo, tt.hi)
			}
		})
	}

	t.Run("gives up after 4 attempts", func(t *testing.T) {
		h, calls := failFirst(100, http.StatusServiceUnavailable, nil)
		c, _ := newTestClient(t, h)
		var w waits
		c.sleep = w.sleep
		_, err := Get[map[string]any](t.Context(), c, "Account", "ok")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want an APIError 503", err)
		}
		if calls.Load() != MaxAttempts || MaxAttempts != 4 {
			t.Errorf("calls = %d, want %d (MaxAttempts = %d, want 4)", calls.Load(), 4, MaxAttempts)
		}
		if len(w.get()) != 3 {
			t.Errorf("waits = %v, want 3", w.get())
		}
	})

	t.Run("cancelled context stops retries", func(t *testing.T) {
		h, calls := failFirst(100, http.StatusServiceUnavailable, nil)
		c, _ := newTestClient(t, h)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c.sleep = func(ctx context.Context, d time.Duration) error {
			cancel() // the caller gives up while the client waits
			return sleepCtx(ctx, d)
		}
		_, err := Get[map[string]any](ctx, c, "Account", "ok")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})

	t.Run("transport error before a response is retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close() // drop the connection without a response
				}
				return
			}
			writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
		}))
		if _, err := Get[map[string]any](t.Context(), c, "Account", "ok"); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})
}

func TestBackoff(t *testing.T) {
	c := &Client{backoffBase: backoffBase, backoffMax: backoffMax, afterCap: retryAfterCap}
	for attempt := 1; attempt <= 12; attempt++ {
		full := min(backoffBase<<(attempt-1), backoffMax)
		for range 50 {
			d := c.backoff(attempt, nil)
			if d < full/2 || d > full {
				t.Fatalf("attempt %d: wait %v outside [%v, %v]", attempt, d, full/2, full)
			}
		}
	}
}

func TestNoRetryOnWrite(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeRaw(w, http.StatusServiceUnavailable, `{}`)
	}))
	doc := map[string]any{"voucher_type": "Journal Entry"}
	writes := []struct {
		name string
		call func() error
	}{
		{"POST insert", func() error { _, err := Insert(t.Context(), c, "Journal Entry", doc); return err }},
		{"PUT update", func() error { _, err := Update(t.Context(), c, "Journal Entry", "JV-1", doc); return err }},
		{"DELETE", func() error { return Delete(t.Context(), c, "Journal Entry", "JV-1") }},
		{"POST cancel", func() error { _, err := Cancel(t.Context(), c, "Journal Entry", "JV-1"); return err }},
		{"POST call", func() error {
			_, err := Call[any](t.Context(), c, http.MethodPost, "frappe.client.set_value", nil)
			return err
		}},
	}
	for _, tt := range writes {
		t.Run(tt.name, func(t *testing.T) {
			calls.Store(0)
			err := tt.call()
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
				t.Fatalf("err = %v, want an APIError 503", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1: writes are never retried", calls.Load())
			}
		})
	}
}
