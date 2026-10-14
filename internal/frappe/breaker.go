package frappe

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/abhishekjha/close-copilot/internal/httpx"
)

// ErrUnavailable is returned, without sending a request, while the circuit
// breaker is open: ERPNext failed too many times in a row. Its text reads
// "ERPNext unavailable".
var ErrUnavailable = errors.New("ERPNext unavailable")

// Breaker defaults.
const (
	// DefaultBreakerThreshold is the consecutive failures that open the breaker.
	DefaultBreakerThreshold = 5
	// DefaultBreakerCooldown is how long the breaker stays open before it
	// lets one probe request through.
	DefaultBreakerCooldown = 30 * time.Second
)

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// breaker is a small circuit breaker: closed until threshold consecutive
// failures, then open (every call fails fast) for cooldown, then half-open
// (one probe at a time) until a probe succeeds (closed) or fails (open again).
// It is safe for concurrent use.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
	probing  bool
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *breaker {
	if threshold <= 0 {
		threshold = DefaultBreakerThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}
	if now == nil {
		now = time.Now
	}
	return &breaker{threshold: threshold, cooldown: cooldown, now: now}
}

// allow reports whether a request may be sent. probe is true when the
// request is the half-open probe; the caller must then call done.
func (b *breaker) allow() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return false, nil
	case stateOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false, ErrUnavailable
		}
		b.state = stateHalfOpen
		b.probing = true
		return true, nil
	default: // half-open
		if b.probing {
			return false, ErrUnavailable
		}
		b.probing = true
		return true, nil
	}
}

// done records the outcome of an allowed request. ok means the server was
// reached (success or a 4xx); failed means a transport error or a 5xx; neither
// (the caller's own cancellation) says nothing about the server.
func (b *breaker) done(probe, ok, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	switch {
	case ok:
		if probe || b.state == stateClosed {
			b.state = stateClosed
			b.failures = 0
		}
	case failed:
		if probe {
			b.trip()
			return
		}
		if b.state == stateClosed {
			b.failures++
			if b.failures >= b.threshold {
				b.trip()
			}
		}
	}
}

func (b *breaker) trip() {
	b.state = stateOpen
	b.openedAt = b.now()
	b.failures = 0
}

// outcome classifies a request's error for the breaker.
func breakerOutcome(ctx context.Context, err error) (ok, failed bool) {
	if err == nil {
		return true, false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status < 500, ae.Status >= 500
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		// The caller gave up (or its deadline passed): not the server's fault.
		return false, false
	}
	if errors.Is(err, httpx.ErrHostRefused) || errors.Is(err, errRedirectRefused) {
		return false, false
	}
	var oe *opError
	if errors.As(err, &oe) {
		return false, true
	}
	return false, false
}

// guarded runs fn through the breaker.
func (c *Client) guarded(ctx context.Context, fn func() error) error {
	if c.brk == nil {
		return fn()
	}
	probe, err := c.brk.allow()
	if err != nil {
		return err
	}
	err = fn()
	ok, failed := breakerOutcome(ctx, err)
	c.brk.done(probe, ok, failed)
	return err
}

// wrapTransport applies Options.Transport to hc.
func wrapTransport(hc *http.Client, wrap func(http.RoundTripper) http.RoundTripper) {
	if wrap == nil || hc.Transport == nil {
		return
	}
	hc.Transport = wrap(hc.Transport)
}
