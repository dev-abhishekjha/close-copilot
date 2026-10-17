package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// The System page and the top bar's health dot (UI-12). It is read-only and
// shows fixed words and numbers: no error text, URL with credentials, key or
// DSN reaches the page or the log. internal/web imports neither the ERPNext
// client nor the store's pool: the checks come through SystemBackend.

// Check timing.
const (
	systemCheckTimeout = 2 * time.Second  // bound on one check
	systemFreshFor     = 10 * time.Second // a result this young is reused
	systemForceAfter   = 2 * time.Second  // ?refresh=1 is ignored on a younger result
	systemWarmEvery    = 30 * time.Second // background refresh by Serve
)

// ErrSystemNotConfigured is what a SystemBackend check returns when the
// service has no configuration (for example no ERP_BASE_URL). The page shows
// "Not configured", not "down".
var ErrSystemNotConfigured = errors.New("web: service not configured")

// SpendToday is the model cost of the UTC day. USD is a decimal string; it is
// shown with two decimals. Fraction is the share of the daily budget used
// (zero when there is no budget); it only decides the tone.
type SpendToday struct {
	USD      string
	Fraction float64
}

// SystemBackend is what the status page reads. Every method must return
// within the context deadline.
type SystemBackend interface {
	// Postgres pings the database.
	Postgres(ctx context.Context) error
	// ERPNext checks that ERP_BASE_URL answers, without credentials.
	ERPNext(ctx context.Context) error
	// SpendToday is the model cost for the UTC day.
	SpendToday(ctx context.Context) (SpendToday, error)
}

// SystemInfo is static facts set at wiring time. It never carries a secret:
// KeyPresent says whether a key is set, ERPHost is the host only.
type SystemInfo struct {
	Provider   string  // "claude-cli" or "anthropic"
	KeyPresent bool    // anthropic only: ANTHROPIC_API_KEY is set
	BudgetUSD  float64 // LLM_DAILY_BUDGET_USD
	ERPHost    string  // host of ERP_BASE_URL; "" if unset
}

// SystemOptions is Options.System.
type SystemOptions struct {
	Backend SystemBackend
	Info    SystemInfo
}

// Status words.
const (
	statusOK      = "ok"
	statusDown    = "down"
	statusUnknown = "unknown"
)

// checkResult is the outcome of one check: a status and the fixed text.
type checkResult struct {
	status, text string
}

// systemResult is one round of checks.
type systemResult struct {
	checked  time.Time
	postgres checkResult
	erp      checkResult
	spend    SpendToday
	spendRes checkResult
}

// systemCache holds the last result. The mutex guards the fields only; it is
// never held while a backend is called.
type systemCache struct {
	mu       sync.Mutex
	res      *systemResult
	inflight chan struct{} // closed when the running refresh ends; nil when none
	// timeout bounds one check; zero means systemCheckTimeout. Tests shorten it.
	timeout time.Duration
}

func (c *systemCache) checkTimeout() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return systemCheckTimeout
}

// systemRoute answers 501 with a friendly page until Options.System is set;
// the role check has run by then.
func (s *Server) systemRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.system == nil {
			p := principalFrom(r.Context())
			if p == nil {
				unauthorized(w, r)
				return
			}
			s.render(w, r, http.StatusNotImplemented, templates.System(templates.SystemPage{
				User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf, NotConfigured: true,
			}))
			return
		}
		h(w, r)
	})
}

// systemStatus returns the current result, refreshing it when it is older
// than systemFreshFor (or, with force, than systemForceAfter, or when the
// clock went backwards). Only one refresh runs at a time: other callers wait
// for it, bounded by their own context, and use its result.
func (s *Server) systemStatus(ctx context.Context, force bool) *systemResult {
	c := &s.sysCache
	for {
		c.mu.Lock()
		now := s.now()
		if res := c.res; res != nil {
			age := now.Sub(res.checked)
			limit := systemFreshFor
			if force {
				limit = systemForceAfter
			}
			if age >= 0 && age < limit {
				c.mu.Unlock()
				return res
			}
		}
		if wait := c.inflight; wait != nil {
			res := c.res
			c.mu.Unlock()
			select {
			case <-wait:
				c.mu.Lock()
				res = c.res
				c.mu.Unlock()
				if res != nil {
					return res
				}
				continue
			case <-ctx.Done():
				return res
			}
		}
		done := make(chan struct{})
		c.inflight = done
		prev := c.res
		c.mu.Unlock()

		res := s.runSystemChecks(ctx, prev)
		c.mu.Lock()
		c.res = res
		c.inflight = nil
		c.mu.Unlock()
		close(done)
		return res
	}
}

// runSystemChecks runs the three checks in parallel, each bounded, and
// returns the result stamped with the clock after they end.
func (s *Server) runSystemChecks(ctx context.Context, prev *systemResult) *systemResult {
	res := &systemResult{}
	base := context.WithoutCancel(ctx)
	timeout := s.sysCache.checkTimeout()
	be := s.system.Backend
	spend := make(chan SpendToday, 1)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		res.postgres = s.boundedCheck(base, timeout, "postgres", be.Postgres)
	}()
	go func() {
		defer wg.Done()
		res.erp = s.boundedCheck(base, timeout, "erpnext", be.ERPNext)
	}()
	go func() {
		defer wg.Done()
		res.spendRes = s.boundedCheck(base, timeout, "spend", func(ctx context.Context) error {
			sp, err := be.SpendToday(ctx)
			spend <- sp // buffered: a check that ends late never blocks or races
			return err
		})
	}()
	wg.Wait()
	res.checked = s.now()
	if res.spendRes.status == statusOK {
		res.spend = <-spend
		if _, ok := usd(res.spend.USD); !ok {
			res.spendRes = checkResult{statusDown, "Check failed"}
		}
	}
	s.logTransitions(base, prev, res)
	return res
}

// boundedCheck runs fn with a deadline and returns without waiting for a
// check that ignores it. The text is fixed: "Not reachable" for an error,
// "Check failed" for a timeout or a panic. The error itself is dropped.
func (s *Server) boundedCheck(ctx context.Context, timeout time.Duration, name string, fn func(context.Context) error) checkResult {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				out <- errCheckPanic
			}
		}()
		out <- fn(cctx)
	}()
	select {
	case err := <-out:
		switch {
		case err == nil:
			return checkResult{statusOK, ""}
		case errors.Is(err, ErrSystemNotConfigured):
			return checkResult{statusUnknown, "Not configured"}
		case errors.Is(err, errCheckPanic):
			return checkResult{statusDown, "Check failed"}
		}
		return checkResult{statusDown, "Not reachable"}
	case <-cctx.Done():
		return checkResult{statusDown, "Check failed"}
	}
}

var errCheckPanic = errors.New("web: system check panicked")

// logTransitions logs, once per episode and with a fixed message and no error
// text, that a check went down.
func (s *Server) logTransitions(ctx context.Context, prev, res *systemResult) {
	for _, c := range []struct {
		name string
		now  func(*systemResult) checkResult
	}{
		{"postgres", func(r *systemResult) checkResult { return r.postgres }},
		{"erpnext", func(r *systemResult) checkResult { return r.erp }},
		{"spend", func(r *systemResult) checkResult { return r.spendRes }},
	} {
		if c.now(res).status != statusDown {
			continue
		}
		if prev != nil && c.now(prev).status == statusDown {
			continue
		}
		s.log.WarnContext(ctx, "web: system check is down", "check", c.name, "result", c.now(res).text)
	}
}

// modelRow is the model provider's status from static facts only: the
// provider is never called.
func modelRow(info SystemInfo) (status, text string) {
	switch info.Provider {
	case "claude-cli":
		return statusOK, "Runs through the local Claude CLI."
	case "anthropic":
		if info.KeyPresent {
			return statusOK, "Uses the Anthropic API."
		}
		return statusDown, "API key not set"
	}
	return statusDown, "Unknown provider"
}

// budgetState derives the budget card from a result.
func budgetState(info SystemInfo, res *systemResult) templates.SystemBudget {
	if res == nil || res.spendRes.status == statusUnknown {
		return templates.SystemBudget{Status: statusUnknown, Tone: "default", Text: "Not checked yet"}
	}
	if res.spendRes.status != statusOK {
		return templates.SystemBudget{Status: statusDown, Tone: "danger", Text: res.spendRes.text}
	}
	spent, _ := usd(res.spend.USD)
	b := templates.SystemBudget{Status: statusOK, Tone: "ok", Spent: "$" + spent}
	if info.BudgetUSD <= 0 || math.IsNaN(info.BudgetUSD) || math.IsInf(info.BudgetUSD, 0) {
		b.Text = "No budget set"
		return b
	}
	of, _ := usd(strconv.FormatFloat(info.BudgetUSD, 'f', -1, 64))
	b.Of = "$" + of
	f := res.spend.Fraction
	if math.IsNaN(f) || f < 0 {
		f = 0
	}
	b.Percent = strconv.Itoa(int(math.Min(f*100, 9999))) + "%"
	switch {
	case f >= 1:
		b.Status, b.Tone, b.Text = statusDown, "danger", "Budget reached: new model calls are refused"
	case f >= 0.8:
		b.Tone, b.Text = "warn", "Close to the daily budget"
	default:
		b.Text = "Within the daily budget"
	}
	return b
}

// usd formats a decimal string with two decimals, exactly (half away from
// zero). It reports false for text that is not a plain non-negative decimal.
func usd(s string) (string, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return "", false
	}
	for _, c := range s {
		if c != '.' && (c < '0' || c > '9') {
			return "", false
		}
	}
	return r.FloatString(2), true
}

// overallHealth is down when Postgres, ERPNext or the model row is down or
// the budget is reached; unknown before any check; else ok.
func overallHealth(info SystemInfo, res *systemResult) string {
	if res == nil {
		return statusUnknown
	}
	if m, _ := modelRow(info); m == statusDown || res.postgres.status == statusDown || res.erp.status == statusDown {
		return statusDown
	}
	if res.spendRes.status == statusOK && info.BudgetUSD > 0 && res.spend.Fraction >= 1 {
		return statusDown // the budget is reached
	}
	return statusOK
}

// systemHealth is the shell's dot: the last result's overall status. It
// never waits for a check and never starts one.
func (s *Server) systemHealth() string {
	if s.system == nil {
		return ""
	}
	s.sysCache.mu.Lock()
	res := s.sysCache.res
	s.sysCache.mu.Unlock()
	return overallHealth(s.system.Info, res)
}

// warmSystem keeps the cache warm until ctx ends, so the dot is not stuck on
// "unknown" and never needs a check of its own.
func (s *Server) warmSystem(ctx context.Context) {
	t := time.NewTicker(systemWarmEvery)
	defer t.Stop()
	for {
		s.systemStatus(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// handleSystemPage answers GET /system.
func (s *Server) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	res := s.systemStatus(r.Context(), r.URL.Query().Get("refresh") == "1")
	info := s.system.Info
	page := templates.SystemPage{User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf}

	pg := templates.SystemRow{ID: "system-postgres", Title: "Postgres", Status: res.postgres.status, Text: res.postgres.text}
	if pg.Status == statusOK {
		pg.Text = "Database is reachable."
	}
	erp := templates.SystemRow{ID: "system-erpnext", Title: "ERPNext", Status: res.erp.status, Text: res.erp.text}
	if erp.Status == statusOK {
		erp.Text = "ERPNext is reachable."
	}
	if info.ERPHost != "" {
		erp.Facts = []templates.Fact{{Label: "Host", Value: info.ERPHost}}
	}
	mStatus, mText := modelRow(info)
	key := "API key not set"
	if info.KeyPresent {
		key = "API key set"
	}
	model := templates.SystemRow{ID: "system-model", Title: "Model provider", Status: mStatus, Text: mText,
		Facts: []templates.Fact{{Label: "Provider", Value: info.Provider}}}
	if info.Provider == "anthropic" {
		model.Facts = append(model.Facts, templates.Fact{Label: "Key", Value: key})
	}
	page.Rows = []templates.SystemRow{pg, erp, model}
	for i := range page.Rows[:2] {
		if page.Rows[i].Text == "" {
			page.Rows[i].Text = "Not checked yet"
		}
	}
	page.Budget = budgetState(info, res)
	if !res.checked.IsZero() {
		page.Checked = fmt.Sprintf("%s UTC", res.checked.UTC().Format("15:04:05"))
	}
	s.render(w, r, http.StatusOK, templates.System(page))
}
