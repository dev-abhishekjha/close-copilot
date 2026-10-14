package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// TestAnthropicSecondCallReadsCachedPrefix (CC-1104): two requests shaped
// like the explainer's (one cacheable system block, one forced tool, a
// different user message each) send a byte-identical prefix (model, tools,
// tool_choice, system with its cache_control breakpoint), so the second
// call reads the cache the first wrote. The explainer side, that its
// requests share System and Tools across a run's findings, is
// internal/agent TestExplainCachedPrefixIdenticalAcrossFindings.
func TestAnthropicSecondCallReadsCachedPrefix(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("decode body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5",
			"content":[{"type":"tool_use","id":"toolu_1","name":"emit_explanation","input":{}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":10,"cache_read_input_tokens":900}}`))
	}))
	defer srv.Close()

	pricing := &PricingTable{Models: map[string]ModelPricing{"claude-haiku-5-5": {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1}}}
	p, err := NewAnthropicProvider(config.Config{AnthropicAPIKey: config.NewSecret("test-key")}, pricing,
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	base := Request{
		Model:     "claude-haiku-5-5",
		System:    []Block{{Text: "Static rules.\n\nACCOUNTS (the only accounts a proposal may use), as JSON:\n[\"a\",\"b\"]\n", Cacheable: true}},
		Tools:     []ToolSpec{{Name: "emit_explanation", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ForceTool: "emit_explanation",
		MaxTokens: 1024,
	}
	for _, user := range []string{"finding one", "finding two"} {
		req := base
		req.Messages = []Message{NewUserTextMessage(user)}
		resp, err := p.Complete(t.Context(), req)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Usage.CacheReadTokens != 900 {
			t.Errorf("cache read tokens %d, want 900 mapped from the response", resp.Usage.CacheReadTokens)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	for _, field := range []string{"model", "tools", "tool_choice", "system"} {
		if !bytes.Equal(bodies[0][field], bodies[1][field]) {
			t.Errorf("%s differs between the first and second call:\n%s\n%s", field, bodies[0][field], bodies[1][field])
		}
	}
	if bytes.Equal(bodies[0]["messages"], bodies[1]["messages"]) {
		t.Error("the user messages should differ")
	}
	var sys []map[string]json.RawMessage
	if err := json.Unmarshal(bodies[1]["system"], &sys); err != nil || len(sys) != 1 {
		t.Fatalf("system %s: %v", bodies[1]["system"], err)
	}
	if len(sys[0]["cache_control"]) == 0 {
		t.Error("the system block carries no cache_control breakpoint")
	}
}
