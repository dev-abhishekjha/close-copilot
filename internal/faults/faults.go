// Package faults injects dependency failures for tests and eval runs
// (CC-1101). Faults come only from the FAULTS environment variable or the
// eval --faults flag, never from a config file, and only cmd/eval and tests
// read them: the agent and the MCP servers do not.
//
// Syntax: comma-separated specs, each target:kind=value:rate=r, for example
//
//	erp:latency=2s:rate=0.2,erp:error=503:rate=0.1
//
// The only target is erp; kinds are latency=<duration> and error=<HTTP
// status, 400 to 599>; rate is in (0, 1].
package faults

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnvFaults is the environment variable FromEnv reads.
const EnvFaults = "FAULTS"

// TargetERP is the one fault target: the ERPNext dependency.
const TargetERP = "erp"

// Kind is what a Spec injects.
type Kind string

// Fault kinds.
const (
	KindLatency Kind = "latency"
	KindError   Kind = "error"
)

// maxLatency bounds an injected delay so a typo can't hang a run.
const maxLatency = time.Minute

// Spec is one fault rule: with probability Rate, a call to Target is
// delayed by Latency (KindLatency) or fails with Status (KindError).
type Spec struct {
	Target  string
	Kind    Kind
	Latency time.Duration
	Status  int
	Rate    float64
}

// String renders the spec in the syntax Parse reads.
func (s Spec) String() string {
	v := ""
	if s.Kind == KindLatency {
		v = s.Latency.String()
	} else {
		v = strconv.Itoa(s.Status)
	}
	return fmt.Sprintf("%s:%s=%s:rate=%s", s.Target, s.Kind, v, strconv.FormatFloat(s.Rate, 'g', -1, 64))
}

// Parse reads a FAULTS value. An empty or blank string is no faults. It
// rejects an unknown target or kind, a bad rate, a bad duration or status,
// and any malformed part.
func Parse(s string) ([]Spec, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []Spec
	for _, item := range strings.Split(s, ",") {
		sp, err := parseSpec(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, nil
}

func parseSpec(item string) (Spec, error) {
	bad := func(format string, a ...any) (Spec, error) {
		return Spec{}, fmt.Errorf("faults: %.60q: %s", item, fmt.Sprintf(format, a...))
	}
	parts := strings.Split(item, ":")
	if len(parts) != 3 {
		return bad("want target:kind=value:rate=r")
	}
	sp := Spec{Target: parts[0]}
	if sp.Target != TargetERP {
		return bad("unknown target %.20q (only %s)", sp.Target, TargetERP)
	}
	kind, val, ok := strings.Cut(parts[1], "=")
	if !ok {
		return bad("want kind=value, got %.20q", parts[1])
	}
	switch Kind(kind) {
	case KindLatency:
		d, err := time.ParseDuration(val)
		if err != nil || d <= 0 || d > maxLatency {
			return bad("latency %.20q is not a duration above 0 and up to %s", val, maxLatency)
		}
		sp.Kind, sp.Latency = KindLatency, d
	case KindError:
		n, err := strconv.Atoi(val)
		if err != nil || n < 400 || n > 599 {
			return bad("error %.20q is not an HTTP status from 400 to 599", val)
		}
		sp.Kind, sp.Status = KindError, n
	default:
		return bad("unknown kind %.20q (latency or error)", kind)
	}
	rkey, rval, ok := strings.Cut(parts[2], "=")
	if !ok || rkey != "rate" {
		return bad("want rate=r, got %.20q", parts[2])
	}
	r, err := strconv.ParseFloat(rval, 64)
	if err != nil || !(r > 0 && r <= 1) {
		return bad("rate %.20q is not a number in (0, 1]", rval)
	}
	sp.Rate = r
	return sp, nil
}

// FromEnv parses the FAULTS environment variable (empty means off).
func FromEnv() ([]Spec, error) {
	return Parse(os.Getenv(EnvFaults))
}

// Decision is the outcome of one call: wait Delay, then fail with Status
// (when non-zero) or go on.
type Decision struct {
	Delay  time.Duration
	Status int
}

// Injector draws decisions for calls to the erp target from a seeded
// random source, so a run repeats. It is safe for concurrent use.
type Injector struct {
	specs []Spec

	mu  sync.Mutex
	rnd *rand.Rand
}

// NewInjector returns an injector for specs (only the erp target counts)
// seeded with seed.
func NewInjector(specs []Spec, seed int64) *Injector {
	var erp []Spec
	for _, s := range specs {
		if s.Target == TargetERP {
			erp = append(erp, s)
		}
	}
	//nolint:gosec // a seeded, repeatable source for test faults, not a security decision
	return &Injector{specs: erp, rnd: rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))}
}

// Next draws the decision for one call. Every spec is drawn, in order, so
// the sequence depends only on the seed and the number of calls: latencies
// add up and the first error that fires wins.
func (i *Injector) Next() Decision {
	if i == nil {
		return Decision{}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	var d Decision
	for _, s := range i.specs {
		if i.rnd.Float64() >= s.Rate {
			continue
		}
		switch s.Kind {
		case KindLatency:
			d.Delay += s.Latency
		case KindError:
			if d.Status == 0 {
				d.Status = s.Status
			}
		}
	}
	return d
}

// SleepFunc waits d or until ctx ends.
type SleepFunc func(ctx context.Context, d time.Duration) error

// RealSleep is a SleepFunc on the wall clock.
func RealSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrInjected is wrapped by every error the injector manufactures.
var ErrInjected = errors.New("injected fault")

// Transport is an http.RoundTripper that injects the erp faults in front of
// Base. A latency fault sleeps (honouring the request context); an error
// fault answers with the status and a Frappe-style body without calling
// Base. It is for tests and eval runs only.
type Transport struct {
	Base  http.RoundTripper // required
	Inj   *Injector
	Sleep SleepFunc // nil means RealSleep
}

// NewTransport wraps base with the faults in specs, seeded with seed.
func NewTransport(base http.RoundTripper, specs []Spec, seed int64) *Transport {
	return &Transport{Base: base, Inj: NewInjector(specs, seed)}
}

// Wrap returns a function that wraps a transport with t's faults, the
// shape of frappe.Options.Transport.
func (t *Transport) Wrap() func(http.RoundTripper) http.RoundTripper {
	return func(base http.RoundTripper) http.RoundTripper {
		return &Transport{Base: base, Inj: t.Inj, Sleep: t.Sleep}
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	d := t.Inj.Next()
	if d.Delay > 0 {
		sleep := t.Sleep
		if sleep == nil {
			sleep = RealSleep
		}
		if err := sleep(req.Context(), d.Delay); err != nil {
			return nil, err
		}
	}
	if d.Status != 0 {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		body := `{"exc_type":"InjectedFault","message":"injected fault"}`
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", d.Status, http.StatusText(d.Status)),
			StatusCode:    d.Status,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}, nil
	}
	if t.Base == nil {
		return nil, errors.New("faults: transport has no base RoundTripper")
	}
	return t.Base.RoundTrip(req)
}
