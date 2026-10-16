package web

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limits. Each is overridable through Options (zero means the default), so
// tests need no sleeping.
const (
	defaultAPIRate  = 5.0 // requests a second per user on /api/*
	defaultAPIBurst = 10
	defaultAskRate  = 1.0 // POST /api/ask costs a model call: tighter
	defaultAskBurst = 3

	defaultLoginIPFailures   = 10 // failed logins per IP per window
	defaultLoginIPWindow     = time.Minute
	defaultLoginUserFailures = 5 // failed logins per username per window, then lockout
	defaultLoginUserWindow   = 15 * time.Minute

	// maxLimiterKeys caps every limiter map. At the cap, idle entries are
	// swept; if none is idle a new key is refused rather than an active one
	// dropped, so forcing eviction never resets an attacker's own bucket.
	maxLimiterKeys = 10000

	maxUsernameKey = 64       // bytes of the username used as a limiter key
	maxCompanyLen  = 64       // bytes of a company id
	maxAPIBody     = 16 << 10 // bytes of an /api JSON request body

	maxRunningCloses = 2  // closes running at once, all companies
	runCapRetryAfter = 30 // seconds
)

// rateLimitMsg is the only detail a refusal gives.
const rateLimitMsg = "rate limit exceeded"

// retryAfterSeconds renders a wait as whole seconds, at least 1.
func retryAfterSeconds(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// sweepDue reports whether a sweep is due and records it. Sweeping walks
// the whole map, so an attacker filling it must not trigger one per request.
func sweepDue(last *time.Time, now time.Time) bool {
	if now.Before(last.Add(time.Second)) && !now.Before(*last) {
		return false
	}
	*last = now
	return true
}

// bucketSet is a bounded set of token buckets keyed by string.
type bucketSet struct {
	mu    sync.Mutex
	m     map[string]*rate.Limiter
	rps   rate.Limit
	burst int
	max   int
	swept time.Time // last full sweep; a full map is swept at most once a second
}

func newBucketSet(rps float64, burst int) *bucketSet {
	return &bucketSet{m: map[string]*rate.Limiter{}, rps: rate.Limit(rps), burst: burst, max: maxLimiterKeys}
}

// allow takes one token for key at now. When refused, wait is the time until
// one is available.
func (b *bucketSet) allow(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lim, ok := b.m[key]
	if !ok {
		if len(b.m) >= b.max && sweepDue(&b.swept, now) {
			for k, l := range b.m {
				if l.TokensAt(now) >= float64(b.burst) {
					delete(b.m, k)
				}
			}
		}
		if len(b.m) >= b.max {
			return false, time.Second
		}
		lim = rate.NewLimiter(b.rps, b.burst)
		b.m[key] = lim
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

func (b *bucketSet) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// apiLimits holds the per-user buckets of the JSON API. The state lives on
// the Server, not in Users, so a users hot-reload does not reset it.
type apiLimits struct {
	general *bucketSet
	ask     *bucketSet
}

func newAPILimits(o Options) *apiLimits {
	pick := func(rps float64, burst int, dr float64, db int) *bucketSet {
		if rps <= 0 {
			rps = dr
		}
		if burst <= 0 {
			burst = db
		}
		return newBucketSet(rps, burst)
	}
	return &apiLimits{
		general: pick(o.APIRate, o.APIBurst, defaultAPIRate, defaultAPIBurst),
		ask:     pick(o.AskRate, o.AskBurst, defaultAskRate, defaultAskBurst),
	}
}

// refuse answers 429 with Retry-After, JSON on /api/*, plain text elsewhere.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, wait time.Duration, event string) {
	if info := requestInfoFrom(r.Context()); info != nil {
		info.event = event
	}
	if p := principalFrom(r.Context()); p != nil && askRateLimited(r) {
		s.refuseAskPage(w, r, p, wait)
		return
	}
	w.Header().Set("Retry-After", retryAfterSeconds(wait))
	if isAPI(r) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": rateLimitMsg})
		return
	}
	http.Error(w, rateLimitMsg, http.StatusTooManyRequests)
}

// rateLimit throttles /api/* per authenticated user name, the same key for
// cookie and bearer callers. It runs after authenticate; other paths (pages,
// htmx polling) pass untouched. POST /api/ask has its own tighter bucket, and
// POST /ask (the Ask page's form) draws from that same bucket under the same
// key, so the form and the API share one allowance.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		form := askRateLimited(r)
		if p == nil || (!isAPI(r) && !form) {
			next.ServeHTTP(w, r)
			return
		}
		set := s.limits.general
		if form || (r.Method == http.MethodPost && r.URL.Path == "/api/ask") {
			set = s.limits.ask
		}
		if ok, wait := set.allow(p.user.Name, s.now()); !ok {
			s.refuse(w, r, wait, "rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// capAPIBody refuses an /api JSON body over maxAPIBody with 413. It reads at
// most maxAPIBody+1 bytes, so a chunked body is caught too, and hands the
// handler the buffered bytes.
func capAPIBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPI(r) || r.Body == nil || r.Body == http.NoBody || isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
		var tooBig *http.MaxBytesError
		switch {
		case len(b) > maxAPIBody || errors.As(err, &tooBig):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		case err != nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		next.ServeHTTP(w, r)
	})
}

// --- login failure limiter ---

// failEntry counts failures inside a fixed window. At the limit the key is
// locked for the rest of the window.
type failEntry struct {
	count int
	start time.Time
}

type failSet struct {
	mu     sync.Mutex
	m      map[string]*failEntry
	limit  int
	window time.Duration
	max    int
	swept  time.Time
}

func newFailSet(limit int, window time.Duration) *failSet {
	return &failSet{m: map[string]*failEntry{}, limit: limit, window: window, max: maxLimiterKeys}
}

// live returns the entry for key if its window is still open.
func (f *failSet) live(key string, now time.Time) *failEntry {
	e := f.m[key]
	if e != nil && !now.Before(e.start.Add(f.window)) {
		delete(f.m, key)
		return nil
	}
	return e
}

// reserve counts one attempt for key before the password is compared, so
// concurrent attempts cannot all pass a check taken before any failure is
// recorded. It refuses when the key is locked (count at the limit). At the
// key cap it sweeps expired entries, then evicts the oldest entry still
// below its limit; a new key is refused only when every entry is locked, and
// a locked entry is never evicted.
func (f *failSet) reserve(key string, now time.Time) (*failEntry, bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.live(key, now); e != nil {
		if e.count >= f.limit {
			return nil, false, e.start.Add(f.window).Sub(now)
		}
		e.count++
		return e, true, 0
	}
	if len(f.m) >= f.max {
		f.sweep(now)
	}
	if len(f.m) >= f.max && !f.evictOne() {
		return nil, false, f.window
	}
	e := &failEntry{count: 1, start: now}
	f.m[key] = e
	return e, true, 0
}

// release gives one reserved attempt back (a successful login).
func (f *failSet) release(key string, e *failEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[key] != e || e.count == 0 {
		return
	}
	if e.count--; e.count == 0 {
		delete(f.m, key)
	}
}

// evictOne drops the oldest entry that is not locked. It reports whether it
// found one.
func (f *failSet) evictOne() bool {
	var victim string
	var oldest time.Time
	found := false
	for k, e := range f.m {
		if e.count >= f.limit {
			continue
		}
		if !found || e.start.Before(oldest) {
			victim, oldest, found = k, e.start, true
		}
	}
	if found {
		delete(f.m, victim)
	}
	return found
}

func (f *failSet) sweep(now time.Time) {
	if !sweepDue(&f.swept, now) {
		return
	}
	for k, e := range f.m {
		if !now.Before(e.start.Add(f.window)) {
			delete(f.m, k)
		}
	}
}

func (f *failSet) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

// loginLimiter counts failed logins per client IP and per username. Only
// failures count; a success consumes nothing and clears nothing.
type loginLimiter struct {
	ip   *failSet
	user *failSet
}

func newLoginLimiter(o Options) *loginLimiter {
	n := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	return &loginLimiter{
		ip:   newFailSet(n(o.LoginIPFailures, defaultLoginIPFailures), defaultLoginIPWindow),
		user: newFailSet(n(o.LoginUserFailures, defaultLoginUserFailures), defaultLoginUserWindow),
	}
}

// clientIP is the host part of r.RemoteAddr and nothing else. X-Forwarded-For
// and Forwarded are never read: a header is attacker-chosen, so trusting one
// would let a client pick its own bucket. A configured trusted-proxy setting
// belongs to the public deploy (CC-1201), not here.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// userKey normalises a username for limiting: trimmed, lowercase, capped.
func userKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) > maxUsernameKey {
		name = name[:maxUsernameKey]
	}
	return name
}

// begin reserves one login attempt for ip and user, under the set locks, before
// any password is compared. When ok is false the attempt is refused (locked)
// and wait is the time left. Otherwise the caller must call done after the
// compare: a failure keeps the reservation counted, a success gives it back.
func (l *loginLimiter) begin(ip, user string, now time.Time) (ok bool, wait time.Duration, done func(success bool)) {
	uk := userKey(user)
	ie, ok1, w1 := l.ip.reserve(ip, now)
	if !ok1 {
		return false, w1, nil
	}
	ue, ok2, w2 := l.user.reserve(uk, now)
	if !ok2 {
		l.ip.release(ip, ie)
		return false, w2, nil
	}
	return true, 0, func(success bool) {
		if success {
			l.ip.release(ip, ie)
			l.user.release(uk, ue)
		}
	}
}
