package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// probeTimeout bounds the ERPNext reachability probe.
const probeTimeout = 2 * time.Second

// systemStore is what the status page needs of the store; *store.Store
// satisfies it.
type systemStore interface {
	Ping(ctx context.Context) error
	LLMSpendToday(ctx context.Context, now time.Time) (string, error)
}

// systemAdapter implements web.SystemBackend over the store and an ERPNext
// probe. It reads only: it never calls the model provider.
type systemAdapter struct {
	st     systemStore
	probe  func(ctx context.Context) error
	budget float64
	now    func() time.Time
}

func (a systemAdapter) Postgres(ctx context.Context) error { return a.st.Ping(ctx) }

func (a systemAdapter) ERPNext(ctx context.Context) error { return a.probe(ctx) }

func (a systemAdapter) SpendToday(ctx context.Context) (web.SpendToday, error) {
	usd, err := a.st.LLMSpendToday(ctx, a.now())
	if err != nil {
		return web.SpendToday{}, err
	}
	out := web.SpendToday{USD: usd}
	// The fraction only picks the tone of the budget card.
	if spent, err := strconv.ParseFloat(usd, 64); err == nil && a.budget > 0 {
		out.Fraction = spent / a.budget
	}
	return out, nil
}

// newSystemOptions builds the status page's backend and static facts from
// cfg. The probe's client comes from httpx.New, so it keeps to the egress
// allowlist; the ping needs no key, and none is sent.
func newSystemOptions(cfg config.Config, st systemStore) (*web.SystemOptions, error) {
	client, err := httpx.New(cfg, httpx.WithTimeout(probeTimeout))
	if err != nil {
		return nil, fmt.Errorf("agent: system status client: %w", err)
	}
	pingURL, host := erpPingURL(cfg.ERPBaseURL)
	return &web.SystemOptions{
		Backend: systemAdapter{st: st, probe: erpProbe(client, pingURL), budget: cfg.LLMDailyBudgetUSD, now: time.Now},
		Info: web.SystemInfo{
			Provider:   cfg.LLMProvider,
			KeyPresent: !cfg.AnthropicAPIKey.IsZero(),
			BudgetUSD:  cfg.LLMDailyBudgetUSD,
			ERPHost:    host,
		},
	}, nil
}

// erpPingURL returns ERPNext's unauthenticated ping URL and the host to
// show, both without userinfo, query or fragment. Both are empty when base
// is not an http(s) URL with a host.
func erpPingURL(base string) (pingURL, host string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.JoinPath("api", "method", "ping").String(), u.Host
}

var errERPNotUp = errors.New("agent: ERPNext ping did not answer 2xx")

// erpProbe returns a check that GETs pingURL: any 2xx is up, anything else
// is an error. An empty pingURL means ERPNext is not configured.
func erpProbe(client *http.Client, pingURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		if pingURL == "" {
			return web.ErrSystemNotConfigured
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingURL, nil)
		if err != nil {
			return errERPNotUp
		}
		resp, err := client.Do(req)
		if err != nil {
			return errERPNotUp
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return errERPNotUp
		}
		return nil
	}
}
