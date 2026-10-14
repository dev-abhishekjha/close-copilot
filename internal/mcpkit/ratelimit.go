package mcpkit

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Default per-token limit. An agent run makes many tool calls in a burst
// (every check's evidence lookups), so the default is generous: 50 requests
// a second with a burst of 100 per token. It exists to stop a runaway or
// hostile caller, not to pace a normal close.
const (
	defaultRateRPS   = 50.0
	defaultRateBurst = 100
	// maxLimiterKeys caps the limiter map. Keys are only created after the
	// bearer token verified, so this is a backstop, not the main defence.
	maxLimiterKeys = 10000
)

// WithRateLimit sets the per-token request limit on every route except
// /healthz: rps requests a second with a burst. rps <= 0 or burst <= 0
// disables limiting. Without this option BuildHandler applies the default
// (50 requests a second, burst 100).
func WithRateLimit(rps float64, burst int) Option {
	return func(o *handlerOptions) {
		o.rateSet = true
		o.rateRPS, o.rateBurst = rps, burst
	}
}

// tokenLimiter is a bounded set of token buckets keyed by a token's SHA-256.
type tokenLimiter struct {
	mu    sync.Mutex
	m     map[string]*rate.Limiter
	rps   rate.Limit
	burst int
	max   int
	now   func() time.Time
	swept time.Time // last full sweep; at most one a second
}

func newTokenLimiter(rps float64, burst int) *tokenLimiter {
	return &tokenLimiter{
		m: map[string]*rate.Limiter{}, rps: rate.Limit(rps), burst: burst,
		max: maxLimiterKeys, now: time.Now,
	}
}

// allow takes one token for key. When refused, wait is how long until one
// is available (at least one second, rounded up).
func (l *tokenLimiter) allow(key string) (ok bool, wait time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, found := l.m[key]
	if !found {
		if len(l.m) >= l.max && (now.Before(l.swept) || !now.Before(l.swept.Add(time.Second))) {
			l.swept = now
			l.sweep(now)
		}
		if len(l.m) >= l.max {
			return false, time.Second
		}
		lim = rate.NewLimiter(l.rps, l.burst)
		l.m[key] = lim
	}
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// sweep drops idle entries (bucket full again). An active bucket is never
// dropped, so forcing eviction cannot reset an attacker's own bucket.
func (l *tokenLimiter) sweep(now time.Time) {
	for k, lim := range l.m {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.m, k)
		}
	}
}

func (l *tokenLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// retryAfterSeconds renders a wait as whole seconds, at least 1.
func retryAfterSeconds(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// tokenKey is the limiter key for a request: the hex SHA-256 of the
// presented bearer token. The token itself is never stored or logged.
func tokenKey(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if _, tok, ok := strings.Cut(h, " "); ok {
		h = tok
	}
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:])
}

// rateLimited refuses a request over the token's limit with 429 and
// Retry-After. It runs after RequireBearer, so unauthenticated requests
// never reach it. A nil l passes everything.
func rateLimited(l *tokenLimiter, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(tokenKey(r)); !ok {
			w.Header().Set("Retry-After", retryAfterSeconds(wait))
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
