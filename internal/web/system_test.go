package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSystem is a SystemBackend that counts calls and can fail or block.
type fakeSystem struct {
	mu                   sync.Mutex
	pgCalls, erpCalls    int
	spendCalls           int
	pgErr, erpErr, spErr error
	spend                SpendToday
	pgBlock              chan struct{} // when set, Postgres waits for it (ignoring ctx)
	pgWaitCtx            bool          // Postgres waits for ctx.Done
}

func (f *fakeSystem) Postgres(ctx context.Context) error {
	f.mu.Lock()
	f.pgCalls++
	err, block, wait := f.pgErr, f.pgBlock, f.pgWaitCtx
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeSystem) ERPNext(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.erpCalls++
	return f.erpErr
}

func (f *fakeSystem) SpendToday(context.Context) (SpendToday, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spendCalls++
	return f.spend, f.spErr
}

func (f *fakeSystem) counts() (pg, erp, spend int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pgCalls, f.erpCalls, f.spendCalls
}

func newSystemServer(t testing.TB, be SystemBackend, info SystemInfo) *testServer {
	t.Helper()
	return newTestServer(t, func(o *Options) { o.System = &SystemOptions{Backend: be, Info: info} })
}

var okInfo = SystemInfo{Provider: "claude-cli", BudgetUSD: 5, ERPHost: "erp.example.test"}

func TestSystemPageAllOK(t *testing.T) {
	be := &fakeSystem{spend: SpendToday{USD: "1.250000", Fraction: 0.25}}
	ts := newSystemServer(t, be, okInfo)
	w := ts.get(t, RoleViewer, "/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := mainContent(t, w.Body.String())
	for _, want := range []string{"Postgres", "ERPNext", "Model provider", "Daily model budget", "$1.25", "of $5.00 (25%)", "Checked 09:00:00 UTC", "erp.example.test", "Check again", "/system?refresh=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if got := strings.Count(body, "ui-chip-ok"); got != 4 {
		t.Errorf("%d ok chips, want 4", got)
	}
	if strings.Contains(body, "ui-chip-danger") {
		t.Error("an all-ok page shows a down chip")
	}
}

func TestSystemPageDown(t *testing.T) {
	const pgText, erpText = "dial tcp 10.9.8.7:5432: password=hunter2", "Get http://erp.internal.test/boom: refused"
	be := &fakeSystem{pgErr: errors.New(pgText), erpErr: errors.New(erpText), spend: SpendToday{USD: "0"}}
	ts := newSystemServer(t, be, okInfo)
	w := ts.get(t, RoleChecker, "/system")
	body := w.Body.String()
	if got := strings.Count(body, "Not reachable"); got != 2 {
		t.Errorf("%d \"Not reachable\", want 2", got)
	}
	if got := strings.Count(body, "ui-chip-danger"); got != 2 {
		t.Errorf("%d down chips, want 2", got)
	}
	for _, bad := range []string{"hunter2", "10.9.8.7", "internal.test", "refused"} {
		if strings.Contains(body, bad) || strings.Contains(ts.logs.String(), bad) {
			t.Errorf("error text %q reached the page or the log", bad)
		}
	}
	if !strings.Contains(ts.logs.String(), "web: system check is down") {
		t.Error("a down check is not logged")
	}
}

func TestSystemPageModelRow(t *testing.T) {
	for _, tt := range []struct {
		name string
		info SystemInfo
		want []string
		down int
	}{
		{"cli", SystemInfo{Provider: "claude-cli"}, []string{"Local Claude CLI", "claude-cli"}, 0},
		{"anthropic key", SystemInfo{Provider: "anthropic", KeyPresent: true}, []string{"Uses the Anthropic API", "API key set"}, 0},
		{"anthropic no key", SystemInfo{Provider: "anthropic"}, []string{"API key not set"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			be := &fakeSystem{spend: SpendToday{USD: "0"}}
			ts := newSystemServer(t, be, tt.info)
			body := mainContent(t, ts.get(t, RoleViewer, "/system").Body.String())
			card := between(t, body, `id="system-model"`, "</section>")
			for _, want := range tt.want {
				if !strings.Contains(strings.ToLower(card), strings.ToLower(want)) {
					t.Errorf("model card lacks %q: %s", want, card)
				}
			}
			if got := strings.Count(card, "ui-chip-danger"); got != tt.down {
				t.Errorf("model card has %d down chips, want %d", got, tt.down)
			}
			// The three checks ran once each; the model row asked nothing.
			if pg, erp, sp := be.counts(); pg != 1 || erp != 1 || sp != 1 {
				t.Errorf("backend calls pg=%d erp=%d spend=%d, want 1 each", pg, erp, sp)
			}
		})
	}
}

func TestSystemPageBudgetTones(t *testing.T) {
	for _, tt := range []struct {
		name     string
		budget   float64
		fraction float64
		want     []string
		deny     []string
	}{
		{"below 80", 5, 0.79, []string{"Within the daily budget", "79%"}, []string{"ui-tone-warn", "ui-tone-danger", "Budget reached"}},
		{"80", 5, 0.8, []string{"Close to the daily budget", "ui-tone-warn"}, []string{"ui-tone-danger", "Budget reached"}},
		{"100", 5, 1, []string{"Budget reached: new model calls are refused", "ui-tone-danger"}, []string{"ui-tone-warn"}},
		{"over", 5, 1.5, []string{"Budget reached", "150%"}, nil},
		{"no budget", 0, 0, []string{"No budget set"}, []string{"ui-tone-warn", "ui-tone-danger", " of $"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			be := &fakeSystem{spend: SpendToday{USD: "3.5", Fraction: tt.fraction}}
			ts := newSystemServer(t, be, SystemInfo{Provider: "claude-cli", BudgetUSD: tt.budget})
			card := between(t, ts.get(t, RoleViewer, "/system").Body.String(), `id="system-budget"`, "</section>")
			for _, want := range tt.want {
				if !strings.Contains(card, want) {
					t.Errorf("budget card lacks %q", want)
				}
			}
			for _, bad := range tt.deny {
				if strings.Contains(card, bad) {
					t.Errorf("budget card has %q", bad)
				}
			}
		})
	}
}

func TestSystemNoSecrets(t *testing.T) {
	secrets := []string{"tok_SECRET123", "sk-ant-PLANTED", "postgres://copilot:pw0rd@db.internal:5432/x", "https://user:pass@erp.example.test/path?key=1"}
	err := errors.New(strings.Join(secrets, " "))
	be := &fakeSystem{pgErr: err, erpErr: err, spErr: err}
	info := SystemInfo{Provider: "anthropic", KeyPresent: true, BudgetUSD: 5, ERPHost: "erp.example.test"}
	ts := newSystemServer(t, be, info)
	for _, path := range []string{"/system", "/system?refresh=1", "/"} {
		body := ts.get(t, RoleChecker, path).Body.String()
		for _, s := range secrets {
			if strings.Contains(body, s) || strings.Contains(ts.logs.String(), s) {
				t.Errorf("%s: %q reached the page or the log", path, s)
			}
		}
	}
	body := ts.get(t, RoleViewer, "/system").Body.String()
	for _, bad := range []string{"pass@", "pw0rd", "https://erp.example.test", "?key="} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if !strings.Contains(body, "erp.example.test") {
		t.Error("the ERPNext host is not shown")
	}
}

func TestSystemChecksBounded(t *testing.T) {
	t.Run("honours the deadline", func(t *testing.T) {
		be := &fakeSystem{pgWaitCtx: true}
		ts := newSystemServer(t, be, okInfo)
		ts.sysCache.timeout = 50 * time.Millisecond
		start := time.Now()
		body := ts.get(t, RoleViewer, "/system").Body.String()
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("took %s", d)
		}
		if !strings.Contains(body, "Check failed") {
			t.Error("a timed-out check does not show \"Check failed\"")
		}
	})
	t.Run("ignores the deadline", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		be := &fakeSystem{pgBlock: release}
		ts := newSystemServer(t, be, okInfo)
		ts.sysCache.timeout = 50 * time.Millisecond
		done := make(chan string, 1)
		go func() { done <- ts.get(t, RoleViewer, "/system").Body.String() }()
		select {
		case body := <-done:
			if !strings.Contains(body, "Check failed") {
				t.Error("a stuck check does not show \"Check failed\"")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the page hung on a check that ignores its deadline")
		}
	})
}

func TestSystemCache(t *testing.T) {
	t.Run("ttl and forced refresh", func(t *testing.T) {
		be := &fakeSystem{spend: SpendToday{USD: "0"}}
		ts := newSystemServer(t, be, okInfo)
		pgCalls := func() int { n, _, _ := be.counts(); return n }
		ts.get(t, RoleViewer, "/system")
		ts.clock.Set(ts.clock.Now().Add(9 * time.Second))
		ts.get(t, RoleViewer, "/system")
		if n := pgCalls(); n != 1 {
			t.Fatalf("two requests within 10 s made %d checks, want 1", n)
		}
		ts.clock.Set(ts.clock.Now().Add(time.Second))
		ts.get(t, RoleViewer, "/system")
		if n := pgCalls(); n != 2 {
			t.Fatalf("after 10 s: %d checks, want 2", n)
		}
		ts.clock.Set(ts.clock.Now().Add(time.Second))
		ts.get(t, RoleViewer, "/system?refresh=1")
		if n := pgCalls(); n != 2 {
			t.Errorf("a forced refresh 1 s after a check ran it again (%d)", n)
		}
		ts.clock.Set(ts.clock.Now().Add(time.Second))
		ts.get(t, RoleViewer, "/system?refresh=1")
		if n := pgCalls(); n != 3 {
			t.Errorf("a forced refresh 2 s after a check made %d checks, want 3", n)
		}
		// A clock that goes backwards counts as stale.
		ts.clock.Set(ts.clock.Now().Add(-time.Hour))
		ts.get(t, RoleViewer, "/system")
		if n := pgCalls(); n != 4 {
			t.Errorf("after the clock went back: %d checks, want 4", n)
		}
	})
	t.Run("one refresh at a time", func(t *testing.T) {
		release := make(chan struct{})
		be := &fakeSystem{pgBlock: release, spend: SpendToday{USD: "0"}}
		ts := newSystemServer(t, be, okInfo)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if w := ts.get(t, RoleViewer, "/system"); w.Code != http.StatusOK {
					t.Errorf("status %d", w.Code)
				}
			}()
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if n, _, _ := be.counts(); n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		close(release)
		wg.Wait()
		if n, e, s := be.counts(); n != 1 || e != 1 || s != 1 {
			t.Errorf("8 concurrent requests made pg=%d erp=%d spend=%d checks, want 1 each", n, e, s)
		}
	})
}

func TestSystemRoles(t *testing.T) {
	be := &fakeSystem{spend: SpendToday{USD: "0"}}
	ts := newSystemServer(t, be, okInfo)
	for _, r := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		if w := ts.get(t, r, "/system"); w.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200", r, w.Code)
		}
	}
	w := ts.do(t, req{method: http.MethodGet, path: "/system"})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("anonymous: %d to %q, want a redirect to /login", w.Code, w.Header().Get("Location"))
	}
	if n, _, _ := be.counts(); n != 1 {
		t.Errorf("%d checks for the allowed requests and none for anonymous, want 1 (cached)", n)
	}
}

func TestSystemNotConfigured(t *testing.T) {
	ts := newTestServer(t)
	w := ts.get(t, RoleViewer, "/system")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not set up") {
		t.Error("501 page is not the friendly one")
	}
	if w := ts.do(t, req{method: http.MethodGet, path: "/system"}); w.Code != http.StatusSeeOther {
		t.Errorf("anonymous: %d, want the login redirect", w.Code)
	}
	if strings.Contains(ts.get(t, RoleViewer, "/").Body.String(), `class="health`) {
		t.Error("the dot shows with no status backend")
	}
}

func TestSystemHealthDot(t *testing.T) {
	be := &fakeSystem{spend: SpendToday{USD: "0"}}
	ts := newSystemServer(t, be, okInfo)
	dot := func(path string) string {
		t.Helper()
		body := ts.get(t, RoleViewer, path).Body.String()
		m := regexp.MustCompile(`<a class="health[^>]*>.*?</a>`).FindString(body)
		if m == "" {
			t.Fatalf("%s: no health dot", path)
		}
		return m
	}
	d := dot("/")
	for _, want := range []string{`data-health="unknown"`, `href="/system"`, "System: not checked yet", "visually-hidden"} {
		if !strings.Contains(d, want) {
			t.Errorf("unknown dot lacks %q: %s", want, d)
		}
	}
	// Rendering other pages made no backend call.
	for _, p := range []string{"/", "/ask", "/nope"} {
		ts.get(t, RoleViewer, p)
	}
	if pg, erp, sp := be.counts(); pg+erp+sp != 0 {
		t.Errorf("other pages made backend calls: %d %d %d", pg, erp, sp)
	}
	ts.get(t, RoleViewer, "/system")
	if d := dot("/"); !strings.Contains(d, `data-health="ok"`) || !strings.Contains(d, "all checks passing") {
		t.Errorf("after a good check: %s", d)
	}
	be.mu.Lock()
	be.erpErr = errors.New("down")
	be.mu.Unlock()
	ts.clock.Set(ts.clock.Now().Add(time.Minute))
	if d := dot("/"); !strings.Contains(d, `data-health="ok"`) {
		t.Errorf("the dot started a check itself: %s", d)
	}
	ts.get(t, RoleViewer, "/system")
	if d := dot("/"); !strings.Contains(d, `data-health="down"`) || !strings.Contains(d, "a check is failing") {
		t.Errorf("after a failing check: %s", d)
	}
}

func TestSystemHealthDotBudgetAndModel(t *testing.T) {
	for _, tt := range []struct {
		name string
		info SystemInfo
		sp   SpendToday
		want string
	}{
		{"budget reached", okInfo, SpendToday{USD: "5", Fraction: 1}, "down"},
		{"no key", SystemInfo{Provider: "anthropic", BudgetUSD: 5}, SpendToday{USD: "0"}, "down"},
		{"warn is still ok", okInfo, SpendToday{USD: "4", Fraction: 0.8}, "ok"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := newSystemServer(t, &fakeSystem{spend: tt.sp}, tt.info)
			ts.get(t, RoleViewer, "/system")
			if got := ts.systemHealth(); got != tt.want {
				t.Errorf("health %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemNav(t *testing.T) {
	ts := newSystemServer(t, &fakeSystem{}, okInfo)
	for _, r := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		nav := mainNav(t, ts.get(t, r, "/").Body.String())
		if !strings.Contains(nav, `href="/system"`) {
			t.Errorf("%s: System is not in the sidebar", r)
		}
	}
}

func TestSystemCSP(t *testing.T) {
	ts := newSystemServer(t, &fakeSystem{spend: SpendToday{USD: "1", Fraction: 0.9}}, okInfo)
	w := ts.get(t, RoleViewer, "/system")
	body := w.Body.String()
	for _, bad := range []string{"<style", "style=", "javascript:", "hx-on"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q (blocked by CSP)", bad)
		}
	}
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(m, "src=") {
			t.Errorf("inline script %s", m)
		}
	}
	if regexp.MustCompile(`\son[a-z]+=`).MatchString(body) {
		t.Error("inline event handler")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP %q", csp)
	}
}
