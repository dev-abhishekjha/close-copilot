package faults

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/ledger"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []Spec
		wantErr string
	}{
		{"empty", "", nil, ""},
		{"blank", "  ", nil, ""},
		{"both", "erp:latency=2s:rate=0.2, erp:error=503:rate=0.1", []Spec{
			{Target: "erp", Kind: KindLatency, Latency: 2 * time.Second, Rate: 0.2},
			{Target: "erp", Kind: KindError, Status: 503, Rate: 0.1},
		}, ""},
		{"rate one", "erp:error=500:rate=1", []Spec{{Target: "erp", Kind: KindError, Status: 500, Rate: 1}}, ""},
		{"unknown target", "llm:error=503:rate=0.1", nil, "unknown target"},
		{"unknown kind", "erp:drop=1:rate=0.1", nil, "unknown kind"},
		{"rate zero", "erp:error=503:rate=0", nil, "rate"},
		{"rate over one", "erp:error=503:rate=1.5", nil, "rate"},
		{"rate nan", "erp:error=503:rate=NaN", nil, "rate"},
		{"rate text", "erp:error=503:rate=often", nil, "rate"},
		{"bad duration", "erp:latency=fast:rate=0.1", nil, "duration"},
		{"negative duration", "erp:latency=-1s:rate=0.1", nil, "duration"},
		{"huge duration", "erp:latency=2h:rate=0.1", nil, "duration"},
		{"bad status", "erp:error=200:rate=0.1", nil, "status"},
		{"missing rate", "erp:error=503", nil, "want target"},
		{"wrong third part", "erp:error=503:p=0.1", nil, "rate"},
		{"no value", "erp:error:rate=0.1", nil, "kind=value"},
		{"trailing comma", "erp:error=503:rate=0.1,", nil, "want target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("spec %d: got %+v, want %+v", i, got[i], tt.want[i])
				}
				if rt, err := Parse(got[i].String()); err != nil || len(rt) != 1 || rt[0] != got[i] {
					t.Errorf("spec %d does not round-trip through %q: %v", i, got[i].String(), err)
				}
			}
		})
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvFaults, "")
	if got, err := FromEnv(); err != nil || got != nil {
		t.Fatalf("empty: %v, %v", got, err)
	}
	t.Setenv(EnvFaults, "erp:error=503:rate=0.5")
	if got, err := FromEnv(); err != nil || len(got) != 1 {
		t.Fatalf("set: %v, %v", got, err)
	}
	t.Setenv(EnvFaults, "nope")
	if _, err := FromEnv(); err == nil {
		t.Fatal("bad value: want an error")
	}
}

func TestInjectorIsSeededAndRated(t *testing.T) {
	specs, _ := Parse("erp:error=503:rate=0.1")
	run := func(seed int64) (out []int) {
		inj := NewInjector(specs, seed)
		for range 2000 {
			out = append(out, inj.Next().Status)
		}
		return out
	}
	a, b, c := run(1), run(1), run(2)
	hits, same := 0, true
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("the same seed gave different decisions")
		}
		if a[i] != c[i] {
			same = false
		}
		if a[i] == 503 {
			hits++
		}
	}
	if same {
		t.Error("different seeds gave the same decisions")
	}
	if hits < 140 || hits > 260 {
		t.Errorf("%d of 2000 calls faulted at rate 0.1", hits)
	}
	if d := (*Injector)(nil).Next(); d != (Decision{}) {
		t.Errorf("nil injector decided %+v", d)
	}
}

func TestTransport(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	get := func(rt http.RoundTripper, ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		return rt.RoundTrip(req)
	}

	t.Run("error fault answers without calling base", func(t *testing.T) {
		specs, _ := Parse("erp:error=503:rate=1")
		tr := NewTransport(http.DefaultTransport, specs, 1)
		before := reached.Load()
		resp, err := get(tr, t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 503 || !strings.Contains(string(body), "InjectedFault") || reached.Load() != before {
			t.Errorf("status %d body %s reached %d", resp.StatusCode, body, reached.Load()-before)
		}
	})

	t.Run("latency fault sleeps then passes through", func(t *testing.T) {
		specs, _ := Parse("erp:latency=2s:rate=1")
		var slept time.Duration
		tr := NewTransport(http.DefaultTransport, specs, 1)
		tr.Sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }
		resp, err := get(tr, t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if slept != 2*time.Second || resp.StatusCode != 200 {
			t.Errorf("slept %s, status %d", slept, resp.StatusCode)
		}
	})

	t.Run("latency fault honours cancellation", func(t *testing.T) {
		specs, _ := Parse("erp:latency=30s:rate=1")
		tr := NewTransport(http.DefaultTransport, specs, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		resp, err := get(tr, ctx)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err %v, want a deadline error", err)
		}
	})

	t.Run("no faults is a pass-through and Wrap shares the injector", func(t *testing.T) {
		tr := NewTransport(http.DefaultTransport, nil, 1)
		resp, err := get(tr.Wrap()(http.DefaultTransport), t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
}

type stubBooks struct {
	checks.BooksReader
	calls int
}

func (s *stubBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	s.calls++
	return ledger.TB{}, nil
}

func TestBooksReaderFaults(t *testing.T) {
	specs, _ := Parse("erp:error=503:rate=1")
	stub := &stubBooks{}
	b := &Books{Reader: stub, Inj: NewInjector(specs, 1)}
	_, err := b.TrialBalance(t.Context(), "c", time.Time{}, time.Time{})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrInjected) || !strings.Contains(err.Error(), "ERPNext unavailable") {
		t.Fatalf("err %v, want ERPNext unavailable", err)
	}
	if stub.calls != 0 {
		t.Error("a faulted call reached the reader")
	}
}
