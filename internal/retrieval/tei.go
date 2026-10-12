package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

const (
	// EmbedBatchSize is how many texts one /embed request carries.
	EmbedBatchSize = 32
	// EmbedDim is the vector length of BAAI/bge-small-en-v1.5, which is also
	// doc_chunks.embedding vector(384).
	EmbedDim = 384

	maxAttempts   = 4
	backoffBase   = 250 * time.Millisecond
	backoffMax    = 4 * time.Second
	retryAfterCap = 30 * time.Second
	maxBody       = 64 << 20 // 64 MiB
)

// Embedder turns texts into 384-dimension vectors, in input order.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Reranker scores texts against a query. Index refers to the input slice.
type Reranker interface {
	Rerank(ctx context.Context, query string, texts []string) ([]Scored, error)
}

// Scored is one reranked text: its index in the input and TEI's score.
type Scored struct {
	Index int     `json:"index"`
	Score float32 `json:"score"`
}

// caller sends POSTs with retries. Every call here is free of side effects,
// so POSTs are retried too.
type caller struct {
	hc    *http.Client
	base  string
	sleep func(context.Context, time.Duration) error
}

func newCaller(cfg config.Config, baseURL, envName string, opts ...httpx.Option) (*caller, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("retrieval: %s is not set", envName)
	}
	hc, err := httpx.New(cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("retrieval: build http client: %w", err)
	}
	return &caller{hc: hc, base: strings.TrimRight(baseURL, "/"), sleep: sleepCtx}, nil
}

// post sends payload to path and returns the 2xx body. op names the
// operation in errors; request text is never included.
func (c *caller) post(ctx context.Context, op, path, contentType string, payload []byte) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		data, status, header, err := c.once(ctx, path, contentType, payload)
		retry := attempt < maxAttempts
		if err != nil {
			retry = retry && ctx.Err() == nil && !errors.Is(err, httpx.ErrHostRefused) && !errors.Is(err, errBody)
		} else {
			retry = retry && retryableStatus(status)
		}
		if err == nil && status >= 200 && status <= 299 {
			return data, nil
		}
		var wait time.Duration
		if retry {
			wait = backoff(attempt, header)
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= wait {
				retry = false
			}
		}
		if !retry {
			if err != nil {
				return nil, fmt.Errorf("retrieval: %s: %w", op, err)
			}
			return nil, &StatusError{Op: op, Status: status}
		}
		slog.DebugContext(ctx, "retrieval: retrying", "op", op, "attempt", attempt, "status", status, "wait", wait)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("retrieval: %s: gave up after attempt %d: %w", op, attempt, err)
		}
	}
}

var errBody = errors.New("bad response body")

// StatusError is a non-2xx answer from TEI or docling.
type StatusError struct {
	Op     string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("retrieval: %s: server answered status %d", e.Op, e.Status)
}

func (c *caller) once(ctx context.Context, path, contentType string, payload []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("%w: read: %w", errBody, err)
	}
	if len(data) > maxBody {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("%w: over %d bytes", errBody, maxBody)
	}
	return data, resp.StatusCode, resp.Header, nil
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff honours Retry-After (capped), else exponential with jitter.
func backoff(attempt int, header http.Header) time.Duration {
	if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, retryAfterCap)
		}
		if t, err := http.ParseTime(v); err == nil {
			return min(max(time.Until(t), 0), retryAfterCap)
		}
	}
	d := backoffBase
	for i := 1; i < attempt && d < backoffMax; i++ {
		d *= 2
	}
	d = min(d, backoffMax)
	half := d / 2
	return half + rand.N(d-half+1) //nolint:gosec // retry jitter, not a security decision
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// TEI is a client for text-embeddings-inference: /embed and /rerank.
type TEI struct {
	embed  *caller
	rerank *caller
}

// NewTEI builds a client for cfg.TEIEmbedURL and cfg.TEIRerankURL. Either
// URL being empty is an error.
func NewTEI(cfg config.Config) (*TEI, error) {
	e, err := newCaller(cfg, cfg.TEIEmbedURL, config.EnvTEIEmbedURL)
	if err != nil {
		return nil, err
	}
	r, err := newCaller(cfg, cfg.TEIRerankURL, config.EnvTEIRerankURL)
	if err != nil {
		return nil, err
	}
	return &TEI{embed: e, rerank: r}, nil
}

// Embed returns one vector per text, in input order, sending batches of 32.
// It fails when a response has a different count than its batch. No texts
// means no call. A vector that is not EmbedDim long is an error.
func (t *TEI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start, batch := 0, 0; start < len(texts); start, batch = start+EmbedBatchSize, batch+1 {
		end := min(start+EmbedBatchSize, len(texts))
		op := fmt.Sprintf("embed batch %d", batch)
		payload, err := json.Marshal(struct {
			Inputs []string `json:"inputs"`
		}{texts[start:end]})
		if err != nil {
			return nil, fmt.Errorf("retrieval: %s: encode: %w", op, err)
		}
		data, err := t.embed.post(ctx, op, "/embed", "application/json", payload)
		if err != nil {
			return nil, err
		}
		var vecs [][]float32
		if err := json.Unmarshal(data, &vecs); err != nil {
			return nil, fmt.Errorf("retrieval: %s: decode response: %w", op, err)
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("retrieval: %s: got %d vectors for %d texts", op, len(vecs), end-start)
		}
		for i, v := range vecs {
			if len(v) != EmbedDim {
				return nil, fmt.Errorf("retrieval: %s: vector %d has %d dimensions, want %d", op, i, len(v), EmbedDim)
			}
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Rerank scores texts against query and returns TEI's list as given (not
// re-sorted). No texts means no call.
func (t *TEI) Rerank(ctx context.Context, query string, texts []string) ([]Scored, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(struct {
		Query string   `json:"query"`
		Texts []string `json:"texts"`
	}{query, texts})
	if err != nil {
		return nil, fmt.Errorf("retrieval: rerank: encode: %w", err)
	}
	data, err := t.rerank.post(ctx, "rerank", "/rerank", "application/json", payload)
	if err != nil {
		return nil, err
	}
	var out []Scored
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("retrieval: rerank: decode response: %w", err)
	}
	for _, s := range out {
		if s.Index < 0 || s.Index >= len(texts) {
			return nil, fmt.Errorf("retrieval: rerank: index %d out of range for %d texts", s.Index, len(texts))
		}
	}
	return out, nil
}
