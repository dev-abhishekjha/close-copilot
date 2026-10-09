package llm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestFakeProviderScriptAndCalls(t *testing.T) {
	ctx := context.Background()

	resp1 := Response{
		Text:       "hello world",
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 5, OutputTokens: 2},
	}
	resp2 := Response{
		ToolCalls: []ToolCall{{
			ID:   "call_123",
			Name: "calc",
			Args: json.RawMessage(`{"a": 1}`),
		}},
		StopReason: "tool_use",
	}

	fake := NewFakeProvider(
		ScriptedCall{Response: resp1},
		ScriptedCall{Err: errors.New("simulated error")},
		ScriptedCall{Response: resp2},
	)

	// Call 1
	r1, err := fake.Complete(ctx, Request{Model: "fast", Messages: []Message{NewUserTextMessage("hi")}})
	if err != nil {
		t.Fatalf("unexpected call 1 error: %v", err)
	}
	if r1.Text != "hello world" {
		t.Errorf("expected 'hello world', got %q", r1.Text)
	}

	// Call 2
	_, err = fake.Complete(ctx, Request{Model: "strong"})
	if err == nil || err.Error() != "simulated error" {
		t.Fatalf("expected simulated error, got %v", err)
	}

	// Call 3
	r3, err := fake.Complete(ctx, Request{Model: "fast"})
	if err != nil {
		t.Fatalf("unexpected call 3 error: %v", err)
	}
	if len(r3.ToolCalls) != 1 || r3.ToolCalls[0].Name != "calc" {
		t.Errorf("expected calc tool call, got %+v", r3.ToolCalls)
	}

	// Call 4 (exhausted -> default fake response)
	r4, err := fake.Complete(ctx, Request{Model: "fast"})
	if err != nil {
		t.Fatalf("unexpected call 4 error: %v", err)
	}
	if r4.Text != "fake response" {
		t.Errorf("expected default response, got %q", r4.Text)
	}

	// Verify calls record
	calls := fake.Calls()
	if len(calls) != 4 {
		t.Fatalf("expected 4 recorded calls, got %d", len(calls))
	}
	if calls[0].Model != "fast" || calls[1].Model != "strong" {
		t.Errorf("recorded call models mismatch: %+v", calls)
	}

	last, ok := fake.LastCall()
	if !ok || last.Model != "fast" {
		t.Errorf("expected last call model 'fast', got ok=%v, model=%s", ok, last.Model)
	}

	// Test Reset
	fake.Reset()
	if fake.CallCount() != 0 {
		t.Errorf("expected 0 calls after reset, got %d", fake.CallCount())
	}
}

func TestFakeProviderConcurrency(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeProvider().WithFallback(Response{Text: "concurrent"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := fake.Complete(ctx, Request{Model: "test"})
			if err != nil {
				t.Errorf("concurrent call failed: %v", err)
			}
			if resp.Text != "concurrent" {
				t.Errorf("expected 'concurrent', got %q", resp.Text)
			}
		}(i)
	}
	wg.Wait()

	if fake.CallCount() != 50 {
		t.Errorf("expected 50 recorded calls, got %d", fake.CallCount())
	}
}
