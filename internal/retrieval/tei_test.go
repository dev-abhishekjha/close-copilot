package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

func vec(seed float32) []float32 {
	v := make([]float32, EmbedDim)
	v[0] = seed
	return v
}

func newTEI(t *testing.T, h http.Handler) (*TEI, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tei, err := NewTEI(config.Config{TEIEmbedURL: srv.URL, TEIRerankURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	fake := func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	tei.embed.sleep, tei.rerank.sleep = fake, fake
	return tei, &waits
}

func TestEmbedBatching(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Inputs []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		sizes = append(sizes, len(in.Inputs))
		mu.Unlock()
		out := make([][]float32, len(in.Inputs))
		for i, s := range in.Inputs {
			var n int
			_, _ = fmt.Sscanf(s, "t%d", &n)
			out[i] = vec(float32(n))
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	texts := make([]string, 70)
	for i := range texts {
		texts[i] = fmt.Sprintf("t%d", i)
	}
	got, err := tei.Embed(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "[32 32 6]" {
		t.Fatalf("batch sizes %v", sizes)
	}
	if len(got) != 70 {
		t.Fatalf("got %d vectors", len(got))
	}
	for i, v := range got {
		if v[0] != float32(i) {
			t.Fatalf("vector %d out of order: %v", i, v[0])
		}
	}
}

func TestEmbedEmptyNoCall(t *testing.T) {
	var calls atomic.Int32
	tei, _ := newTEI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	if got, err := tei.Embed(t.Context(), nil); got != nil || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := tei.Rerank(t.Context(), "q", nil); got != nil || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
	if calls.Load() != 0 {
		t.Fatal("made a call for no texts")
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float32{vec(1)})
	}))
	_, err := tei.Embed(t.Context(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "1 vectors for 2 texts") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmbedWrongDimension(t *testing.T) {
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float32{{1, 2, 3}})
	}))
	if _, err := tei.Embed(t.Context(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmbedRetry(t *testing.T) {
	var calls atomic.Int32
	tei, waits := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([][]float32{vec(1)})
	}))
	if _, err := tei.Embed(t.Context(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(*waits) != 2 || (*waits)[0] != time.Second {
		t.Fatalf("calls=%d waits=%v", calls.Load(), *waits)
	}
}

func TestEmbedRetryExhausted(t *testing.T) {
	var calls atomic.Int32
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := tei.Embed(t.Context(), []string{"a"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || calls.Load() != maxAttempts {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestEmbedNoRetryOn4xx(t *testing.T) {
	for _, status := range []int{400, 413} {
		var calls atomic.Int32
		tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		}))
		_, err := tei.Embed(t.Context(), []string{"secret text"})
		if err == nil || calls.Load() != 1 {
			t.Fatalf("status %d: err=%v calls=%d", status, err, calls.Load())
		}
		if !strings.Contains(err.Error(), "batch 0") || strings.Contains(err.Error(), "secret text") {
			t.Fatalf("status %d: err = %v", status, err)
		}
	}
}

func TestEmbedCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	tei.embed.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := tei.Embed(ctx, []string{"a"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRerankDecode(t *testing.T) {
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query string
			Texts []string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/rerank" || in.Query != "bank charge" || len(in.Texts) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"index":2,"score":0.98},{"index":0,"score":0.1},{"index":1,"score":0.01}]`))
	}))
	got, err := tei.Rerank(t.Context(), "bank charge", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Index != 2 || got[0].Score != 0.98 {
		t.Fatalf("got %+v", got)
	}
}

func TestRerankIndexOutOfRange(t *testing.T) {
	tei, _ := newTEI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"index":5,"score":1}]`))
	}))
	if _, err := tei.Rerank(t.Context(), "q", []string{"a"}); err == nil {
		t.Fatal("want error")
	}
}

func TestNewTEIEmptyURL(t *testing.T) {
	if _, err := NewTEI(config.Config{TEIRerankURL: "http://localhost:1"}); err == nil {
		t.Fatal("want error for empty embed URL")
	}
	if _, err := NewTEI(config.Config{TEIEmbedURL: "http://localhost:1"}); err == nil {
		t.Fatal("want error for empty rerank URL")
	}
}

func TestHostOutsideAllowlistRefused(t *testing.T) {
	tei, err := NewTEI(config.Config{TEIEmbedURL: "http://localhost:1", TEIRerankURL: "http://localhost:1"})
	if err != nil {
		t.Fatal(err)
	}
	tei.embed.base = "http://evil.example.invalid:9"
	var calls int
	tei.embed.sleep = func(context.Context, time.Duration) error { calls++; return nil }
	_, err = tei.Embed(t.Context(), []string{"a"})
	if !errors.Is(err, httpx.ErrHostRefused) || calls != 0 {
		t.Fatalf("err=%v sleeps=%d", err, calls)
	}
}
