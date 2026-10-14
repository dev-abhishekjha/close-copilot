package frappe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/faults"
)

// fakeClock is a settable clock for the breaker.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func newBreakerClient(t *testing.T, h http.Handler) (*Client, *fakeClock) {
	t.Helper()
	c, _ := newTestClient(t, h)
	clk := &fakeClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	c.brk = newBreaker(5, 30*time.Second, clk.now)
	return c, clk
}

func getOK(c *Client) error {
	_, err := Get[map[string]any](context.Background(), c, "Account", "ok")
	return err
}

func TestBreakerOpensAfterFiveFailuresAndProbes(t *testing.T) {
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if s := int(status.Load()); s != http.StatusOK {
			writeRaw(w, s, `{"exc_type":"ServerError"}`)
			return
		}
		writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
	})
	c, clk := newBreakerClient(t, h)

	for i := 0; i < 5; i++ {
		var ae *APIError
		if err := getOK(c); !errors.As(err, &ae) {
			t.Fatalf("call %d: err %v, want an APIError", i, err)
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("server saw %d calls, want 5", calls.Load())
	}

	// Open: fails fast, no request.
	err := getOK(c)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "ERPNext unavailable") {
		t.Fatalf("err %v, want ErrUnavailable", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("open breaker sent a request (%d calls)", calls.Load())
	}
	clk.advance(29 * time.Second)
	if err := getOK(c); !errors.Is(err, ErrUnavailable) || calls.Load() != 5 {
		t.Fatalf("before the cooldown: err %v, calls %d", err, calls.Load())
	}

	// After the cooldown one probe goes out; a failed probe reopens.
	clk.advance(2 * time.Second)
	var ae *APIError
	if err := getOK(c); !errors.As(err, &ae) || calls.Load() != 6 {
		t.Fatalf("probe: err %v, calls %d, want one probe", err, calls.Load())
	}
	if err := getOK(c); !errors.Is(err, ErrUnavailable) || calls.Load() != 6 {
		t.Fatalf("after a failed probe: err %v, calls %d", err, calls.Load())
	}

	// A successful probe closes it.
	clk.advance(31 * time.Second)
	status.Store(http.StatusOK)
	if err := getOK(c); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := getOK(c); err != nil || calls.Load() != 8 {
		t.Fatalf("closed again: err %v, calls %d", err, calls.Load())
	}
}

func TestBreakerCountsOnlyConsecutiveServerFailures(t *testing.T) {
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n.Add(1) % 5 {
		case 0:
			writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
		case 4:
			writeRaw(w, http.StatusNotFound, `{"exc_type":"DoesNotExistError"}`)
		default:
			writeRaw(w, http.StatusInternalServerError, `{}`)
		}
	})
	c, _ := newBreakerClient(t, h)
	// 500 x3, 404 (resets), 200, ... never five 5xx in a row.
	for i := 0; i < 40; i++ {
		if err := getOK(c); errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d: breaker opened on non-consecutive failures", i)
		}
	}
}

func TestBreakerIgnoresCallerCancellation(t *testing.T) {
	c, _ := newBreakerClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 10; i++ {
		_, err := Get[map[string]any](ctx, c, "Account", "ok")
		if errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d: cancelled calls opened the breaker", i)
		}
	}
	if err := getOK(c); err != nil {
		t.Fatalf("after cancelled calls: %v", err)
	}
}

func TestBreakerTransportErrorsCountAndWritesAreGuarded(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // every connection is refused
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	c.brk = newBreaker(5, 30*time.Second, clk.now)
	for i := 0; i < 5; i++ {
		err := c.request(context.Background(), http.MethodPost, "/api/resource/Account", nil, map[string]any{"a": 1}, nil, false)
		if err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d: err %v, want a transport error", i, err)
		}
	}
	err := c.request(context.Background(), http.MethodPost, "/api/resource/Account", nil, map[string]any{"a": 1}, nil, false)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("write after 5 failures: %v, want ErrUnavailable", err)
	}
}

func TestBreakerConcurrentUse(t *testing.T) {
	c, clk := newBreakerClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, http.StatusInternalServerError, `{}`)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = getOK(c)
				clk.advance(10 * time.Second)
			}
		}()
	}
	wg.Wait()
}

func TestBreakerRequestDeadlineEndsInContextError(t *testing.T) {
	release := make(chan struct{})
	c, _ := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	c.deadline = 50 * time.Millisecond
	err := getOK(c)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want a deadline error", err)
	}
}

func TestBreakerWithInjectedFaults(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		writeRaw(w, http.StatusOK, `{"data":{"name":"ok"}}`)
	}))
	t.Cleanup(srv.Close)
	specs, err := faults.Parse("erp:error=503:rate=1")
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	c, err := NewWithOptions(config.Config{ERPBaseURL: srv.URL, ERPSite: testSite}, testKey, testSecret, Options{
		Transport:    faults.NewTransport(nil, specs, 1).Wrap(),
		BreakerClock: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	for i := 0; i < 5; i++ {
		var ae *APIError
		if err := getOK(c); !errors.As(err, &ae) || ae.Status != 503 {
			t.Fatalf("call %d: %v, want an injected 503", i, err)
		}
	}
	if err := getOK(c); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v, want ErrUnavailable", err)
	}
	if reached.Load() != 0 {
		t.Errorf("injected faults let %d requests reach the server", reached.Load())
	}
}
