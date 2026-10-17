package fixture

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// Account is a fixture sign-in. The passwords are synthetic, live only in this
// package and e2e/README.md, and only work against the fixture on localhost.
type Account struct {
	Name, Password string
	Role           web.Role
}

// Accounts are the fixture users, one per role.
var Accounts = []Account{
	{"viewer", "e2e-viewer-password", web.RoleViewer},
	{"asha", "e2e-maker-password", web.RoleMaker},
	{"ravi", "e2e-checker-password", web.RoleChecker},
}

// Clock is the fixture's data time: 2026-10-01 09:00 UTC. Every seeded row and
// every row the fixture writes is dated from it, so pages never show the wall
// clock; only the web server's own clock (sessions, deadlines) is real time.
var Clock = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// sessionKey is a synthetic 32-byte key.
const sessionKey = "e2e-fixture-session-key-0123456789"

// New builds the web server over fresh in-memory backends. quiet silences the
// request log.
func New(quiet bool) (*web.Server, error) {
	users := make([]web.User, 0, len(Accounts))
	for _, a := range Accounts {
		h, err := bcrypt.GenerateFromPassword([]byte(a.Password), web.MinBcryptCost)
		if err != nil {
			return nil, fmt.Errorf("fixture: hash password: %w", err)
		}
		users = append(users, web.User{Name: a.Name, Role: a.Role, PasswordHash: string(h)})
	}
	set, err := web.NewUsers(users...)
	if err != nil {
		return nil, fmt.Errorf("fixture: users: %w", err)
	}
	st := newStore(Clock)
	st.seed(Clock)
	const lots = 1_000_000
	opts := web.Options{
		SessionKey:      config.NewSecret(sessionKey),
		InsecureCookies: true, // plain http on localhost
		// Real time: the approve route sets a write deadline from this clock.
		Now:       time.Now,
		Runs:      &web.RunsBackend{Store: st, Execute: st.Execute, Findings: st},
		Ask:       Ask{},
		Approvals: Approvals{s: st},
		Audit:     st,
		Documents: Documents{},
		System: &web.SystemOptions{Backend: System{},
			Info: web.SystemInfo{Provider: "claude-cli", BudgetUSD: 5, ERPHost: "erp.example.test"}},
		// Raise the limits so repeated sign-ins and asks never see a 429.
		APIRate: lots, APIBurst: lots, AskRate: lots, AskBurst: lots,
		LoginIPFailures: lots, LoginUserFailures: lots,
	}
	var log *slog.Logger
	if quiet {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	} else {
		log = slog.Default()
	}
	return web.NewServer(opts, set, log)
}
