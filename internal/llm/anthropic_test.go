package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	provider, err := NewAnthropicProvider(cfg, nil,
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
