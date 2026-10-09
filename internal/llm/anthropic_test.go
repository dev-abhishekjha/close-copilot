package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestAnthropicProvider_SuccessAndMapping(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		_ = json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-5-sonnet-20241022",
			"content": [
				{
					"type": "text",
					"text": "Explanation note"
				},
				{
					"type": "tool_use",
					"id": "toolu_abc",
					"name": "emit_explanation",
					"input": {
						"explanation": "Bank fee identified",
						"needs_review": false
					}
				}
			],
			"stop_reason": "tool_use",
			"usage": {
				"input_tokens": 1000,
				"output_tokens": 200,
				"cache_creation_input_tokens": 500,
				"cache_read_input_tokens": 100
			}
		}`))
	}))
	defer srv.Close()

	cfg := config.Config{
		AnthropicAPIKey: config.NewSecret("test-api-key"),
	}
	pricing := &PricingTable{
		Models: map[string]ModelPricing{
			"claude-3-5-sonnet-20241022": {
				Input:      3.0,
				Output:     15.0,
				CacheWrite: 3.75,
				CacheRead:  0.30,
			},
		},
	}

	provider, err := NewAnthropicProvider(cfg, pricing,
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewAnthropicProvider failed: %v", err)
	}

	req := Request{
		Model: "claude-3-5-sonnet-20241022",
		System: []Block{
			{Text: "System prompt preamble", Cacheable: false},
			{Text: "Cached instructions", Cacheable: true},
		},
		Messages: []Message{
			NewUserTextMessage("Explain this bank line"),
		},
		Tools: []ToolSpec{{
			Name:        "emit_explanation",
			Description: "Emits close explanation",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		ForceTool: "emit_explanation",
	}

	resp, err := provider.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	// Verify response mapping
	if resp.Text != "Explanation note" {
		t.Errorf("expected text 'Explanation note', got %q", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "toolu_abc" || resp.ToolCalls[0].Name != "emit_explanation" {
		t.Errorf("unexpected tool call: %+v", resp.ToolCalls[0])
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("expected stop reason 'tool_use', got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 1000 || resp.Usage.OutputTokens != 200 {
		t.Errorf("unexpected usage tokens: %+v", resp.Usage)
	}
	if resp.Usage.CostUSD <= 0 {
		t.Errorf("expected positive CostUSD, got %f", resp.Usage.CostUSD)
	}

	// Verify system prompt cache control was formatted in payload
	sysBlocks, ok := receivedBody["system"].([]interface{})
	if !ok || len(sysBlocks) != 2 {
		t.Fatalf("expected 2 system blocks in request payload, got %v", receivedBody["system"])
	}
	b2 := sysBlocks[1].(map[string]interface{})
	if b2["cache_control"] == nil {
		t.Errorf("expected cache_control on block 2, got nil")
	}

	// Verify forced tool choice
	toolChoice, ok := receivedBody["tool_choice"].(map[string]interface{})
	if !ok || toolChoice["name"] != "emit_explanation" {
		t.Errorf("expected tool_choice for emit_explanation, got %v", receivedBody["tool_choice"])
	}
}

func TestAnthropicProvider_RetriesOnRateLimitAndErrors(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		if att == 1 {
			// First call: 429 Too Many Requests with Retry-After header
			w.Header().Set("Retry-After", "0.01")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		if att == 2 {
			// Second call: 529 Overloaded
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
			return
		}

		// Third call: success
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_success",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-5-haiku-20241022",
			"content": [{"type":"text","text":"ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	cfg := config.Config{
		AnthropicAPIKey: config.NewSecret("dummy-key"),
	}

	provider, err := NewAnthropicProvider(cfg, anthropicTestPricing(),
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithInitialBackoff(2*time.Millisecond),
		WithMaxRetries(3),
	)
	if err != nil {
		t.Fatalf("NewAnthropicProvider failed: %v", err)
	}

	resp, err := provider.Complete(context.Background(), Request{
		Model:    "claude-3-5-haiku-20241022",
		Messages: []Message{NewUserTextMessage("ping")},
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got error: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("expected 'ok', got %q", resp.Text)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", atomic.LoadInt32(&attempts))
	}
}

func TestAnthropicProvider_NonRetryableError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`))
	}))
	defer srv.Close()

	cfg := config.Config{AnthropicAPIKey: config.NewSecret("key")}
	provider, err := NewAnthropicProvider(cfg, nil,
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithInitialBackoff(1*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = provider.Complete(context.Background(), Request{
		Model:    "claude-3-5-haiku-20241022",
		Messages: []Message{NewUserTextMessage("bad")},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("expected exactly 1 attempt on 400 error, got %d", atomic.LoadInt32(&attempts))
	}
}

func anthropicTestPricing() *PricingTable {
	return &PricingTable{Models: map[string]ModelPricing{
		"claude-3-5-haiku-20241022": {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1},
		"claude-haiku-4-5-20251001": {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1},
	}}
}

const okMessage = `{
	"id": "msg_ok", "type": "message", "role": "assistant",
	"model": "claude-haiku-4-5-20251001",
	"content": [{"type":"text","text":"ok"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 10, "output_tokens": 5}
}`

// scriptedServer answers each request with the next status/header pair and
// then okBody; it counts requests.
func scriptedServer(t *testing.T, statuses []int, retryAfter []string, okBody string) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		w.Header().Set("Content-Type", "application/json")
		if i < len(statuses) {
			if i < len(retryAfter) && retryAfter[i] != "" {
				w.Header().Set("Retry-After", retryAfter[i])
			}
			w.WriteHeader(statuses[i])
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
			return
		}
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func newTestProvider(t *testing.T, srv *httptest.Server, opts ...AnthropicOption) *AnthropicProvider {
	t.Helper()
	all := append([]AnthropicOption{WithBaseURL(srv.URL), WithHTTPClient(srv.Client())}, opts...)
	p, err := NewAnthropicProvider(config.Config{AnthropicAPIKey: config.NewSecret("k")}, anthropicTestPricing(), all...)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	return p
}

func pingRequest() Request {
	return Request{Model: "claude-haiku-4-5-20251001", Messages: []Message{NewUserTextMessage("ping")}}
}

func TestAnthropicProvider_RetryAfterBeyondCapStops(t *testing.T) {
	for _, ra := range []string{"86400", "1e300", "31"} {
		t.Run(ra, func(t *testing.T) {
			srv, n := scriptedServer(t, []int{429}, []string{ra}, okMessage)
			p := newTestProvider(t, srv, WithInitialBackoff(time.Millisecond), WithMaxRetries(3))

			start := time.Now()
			_, err := p.Complete(context.Background(), pingRequest())
			if err == nil {
				t.Fatal("expected the 429 to be returned")
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("slept %s on Retry-After %s", time.Since(start), ra)
			}
			if got := atomic.LoadInt32(n); got != 1 {
				t.Errorf("expected 1 attempt, got %d", got)
			}
		})
	}
}

func TestAnthropicProvider_InvalidRetryAfterFallsBack(t *testing.T) {
	for _, ra := range []string{"NaN", "+Inf", "-Inf", "-5", "soon"} {
		t.Run(ra, func(t *testing.T) {
			srv, n := scriptedServer(t, []int{529}, []string{ra}, okMessage)
			p := newTestProvider(t, srv, WithInitialBackoff(time.Millisecond), WithMaxRetries(2))

			start := time.Now()
			resp, err := p.Complete(context.Background(), pingRequest())
			if err != nil {
				t.Fatalf("expected success after retry, got %v", err)
			}
			if resp.Text != "ok" || atomic.LoadInt32(n) != 2 {
				t.Errorf("text %q after %d attempts", resp.Text, atomic.LoadInt32(n))
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("invalid Retry-After %s should use the short exponential backoff, took %s", ra, time.Since(start))
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"0", 0, true},
		{"1.5", 1500 * time.Millisecond, true},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"-1", 0, false},
		{"1e300", time.Duration(math.MaxInt64), true},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), 0, false},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestExpBackoffNeverOverflows(t *testing.T) {
	maxB := 30 * time.Second
	for _, initial := range []time.Duration{time.Millisecond, 20 * time.Second, time.Duration(math.MaxInt64 / 3)} {
		for attempt := 0; attempt <= 70; attempt++ {
			got := expBackoff(min(initial, maxB), maxB, attempt)
			if got <= 0 || got > maxB {
				t.Fatalf("expBackoff(%v, %d) = %v", initial, attempt, got)
			}
		}
	}
	if got := expBackoff(time.Millisecond, maxB, 3); got != 8*time.Millisecond {
		t.Errorf("expBackoff(1ms, 3) = %v, want 8ms", got)
	}
}

func TestAnthropicProvider_MaxRetriesClamped(t *testing.T) {
	t.Run("negative means one attempt", func(t *testing.T) {
		srv, n := scriptedServer(t, []int{500, 500}, nil, okMessage)
		p := newTestProvider(t, srv, WithInitialBackoff(time.Millisecond), WithMaxRetries(-1))
		_, err := p.Complete(context.Background(), pingRequest())
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "%!") {
			t.Errorf("malformed error: %v", err)
		}
		if got := atomic.LoadInt32(n); got != 1 {
			t.Errorf("expected 1 attempt, got %d", got)
		}
	})

	t.Run("large is capped at five retries", func(t *testing.T) {
		srv, n := scriptedServer(t, []int{500, 500, 500, 500, 500, 500, 500, 500, 500, 500}, nil, okMessage)
		p := newTestProvider(t, srv, WithInitialBackoff(time.Millisecond), WithMaxRetries(100))
		if _, err := p.Complete(context.Background(), pingRequest()); err == nil {
			t.Fatal("expected an error")
		}
		if got := atomic.LoadInt32(n); got != 1+maxRetriesCap {
			t.Errorf("expected %d attempts, got %d", 1+maxRetriesCap, got)
		}
	})
}

func toolUseMessage(name string) string {
	return fmt.Sprintf(`{
		"id": "msg_t", "type": "message", "role": "assistant",
		"model": "claude-haiku-4-5-20251001",
		"content": [{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`, name)
}

func TestAnthropicProvider_RejectsToolNotOffered(t *testing.T) {
	srv, _ := scriptedServer(t, nil, nil, toolUseMessage("post_journal_entry"))
	p := newTestProvider(t, srv)
	req := pingRequest()
	req.Tools = []ToolSpec{{Name: "emit_explanation", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	resp, err := p.Complete(context.Background(), req)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("no tool call may be returned, got %+v", resp.ToolCalls)
	}
}

func TestAnthropicProvider_ForceToolNotOffered(t *testing.T) {
	srv, n := scriptedServer(t, nil, nil, okMessage)
	p := newTestProvider(t, srv)
	req := pingRequest()
	req.Tools = []ToolSpec{{Name: "emit_explanation", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req.ForceTool = "post_journal_entry"
	if _, err := p.Complete(context.Background(), req); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
	if got := atomic.LoadInt32(n); got != 0 {
		t.Errorf("no request may be sent, got %d", got)
	}
}

func TestAnthropicProvider_UnpricedModel(t *testing.T) {
	body := strings.Replace(okMessage, "claude-haiku-4-5-20251001", "claude-unlisted-9", 1)
	srv, _ := scriptedServer(t, nil, nil, body)
	p := newTestProvider(t, srv)
	resp, err := p.Complete(context.Background(), pingRequest())
	if !errors.Is(err, ErrUnpricedModel) {
		t.Fatalf("expected ErrUnpricedModel, got %v", err)
	}
	if resp.Model != "claude-unlisted-9" || resp.Usage.InputTokens != 10 || resp.Usage.CostUSD != 0 {
		t.Errorf("usage should be reported unpriced: %+v", resp)
	}
}
