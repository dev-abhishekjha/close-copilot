package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func explainRequest() llm.Request {
	return llm.Request{
		Model:     "claude-haiku-5-5",
		System:    []llm.Block{{Text: "You explain close findings.", Cacheable: true}},
		Messages:  []llm.Message{llm.NewUserTextMessage("Explain bank charge TXN-0002 of Rs 295.00 for testco.")},
		Tools:     []llm.ToolSpec{{Name: "emit_explanation", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ForceTool: "emit_explanation",
		MaxTokens: 512,
	}
}

// steppingClock advances by step on every call.
func steppingClock(step time.Duration) func() time.Time {
	t := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(step)
		return t
	}
}

func TestRecordingProvider(t *testing.T) {
	okResp := llm.Response{
		Model: "claude-haiku-5-5", StopReason: "tool_use",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "emit_explanation", Args: json.RawMessage(`{"explanation":"SMS charges"}`)}},
		Usage:     llm.Usage{InputTokens: 300, OutputTokens: 40, CacheReadTokens: 200, CostUSD: 0.000123},
	}
	unpriced := llm.Response{Model: "claude-unlisted-9", Text: "x", Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}}
	hardErr := errors.New("transport closed")
	dbErr := errors.New("db down")

	tests := []struct {
		name       string
		script     llm.ScriptedCall
		putErr     error
		failKind   string
		callErr    error
		wantErrIs  []error
		wantCalls  int // llm_calls rows
		wantInner  int // model calls
		wantModel  string
		wantTokens int64
		wantCost   string
	}{
		{name: "success", script: llm.ScriptedCall{Response: okResp}, wantCalls: 1, wantInner: 1, wantModel: "claude-haiku-5-5", wantTokens: 300, wantCost: "0.000123"},
		{
			name:      "unpriced model is recorded, then the error returned",
			script:    llm.ScriptedCall{Response: unpriced, Err: fmt.Errorf("claude-cli: %w", llm.ErrUnpricedModel)},
			wantErrIs: []error{llm.ErrUnpricedModel}, wantCalls: 1, wantInner: 1, wantModel: "claude-unlisted-9", wantTokens: 10, wantCost: "0",
		},
		{
			name:   "error without a response is not recorded",
			script: llm.ScriptedCall{Err: hardErr}, wantErrIs: []error{hardErr}, wantCalls: 0, wantInner: 1,
		},
		{
			name:   "prompt that can't be stored never reaches the model",
			script: llm.ScriptedCall{Response: okResp}, putErr: dbErr, failKind: store.ArtifactPrompt,
			wantErrIs: []error{dbErr}, wantCalls: 0, wantInner: 0,
		},
		{
			name:   "response that can't be stored fails, call not repeated",
			script: llm.ScriptedCall{Response: okResp}, putErr: dbErr, failKind: store.ArtifactResponse,
			wantErrIs: []error{dbErr}, wantCalls: 0, wantInner: 1,
		},
		{
			name:   "llm_calls insert failure fails, call not repeated",
			script: llm.ScriptedCall{Response: okResp}, callErr: dbErr,
			wantErrIs: []error{dbErr}, wantCalls: 0, wantInner: 1,
		},
		{
			name:   "unpriced plus a recording failure reports both",
			script: llm.ScriptedCall{Response: unpriced, Err: llm.ErrUnpricedModel}, callErr: dbErr,
			wantErrIs: []error{llm.ErrUnpricedModel, dbErr}, wantCalls: 0, wantInner: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeArtifactStore()
			fs.putErr, fs.failKind, fs.callErr = tt.putErr, tt.failKind, tt.callErr
			inner := llm.NewFakeProvider(tt.script)
			p := NewRecordingProvider(inner, fs, uuid.New(), uuid.New())
			p.now = steppingClock(250 * time.Millisecond)

			req := explainRequest()
			resp, err := p.Complete(context.Background(), req)
			if len(tt.wantErrIs) == 0 && err != nil {
				t.Fatalf("Complete: %v", err)
			}
			for _, want := range tt.wantErrIs {
				if !errors.Is(err, want) {
					t.Errorf("Complete error %v, want it to wrap %v", err, want)
				}
			}
			if got := inner.CallCount(); got != tt.wantInner {
				t.Errorf("model calls %d, want %d", got, tt.wantInner)
			}
			if len(fs.calls) != tt.wantCalls {
				t.Fatalf("llm_calls rows %d, want %d", len(fs.calls), tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			c := fs.calls[0]
			if c.Model != tt.wantModel || c.InputTokens != tt.wantTokens || c.CostUSD != tt.wantCost || c.LatencyMS != 250 || c.RunID == uuid.Nil {
				t.Errorf("llm call %+v", c)
			}
			wantPrompt, _ := store.Canonical(req)
			if got := fs.content[c.PromptSHA256]; string(got) != string(wantPrompt) || fs.kinds[c.PromptSHA256] != store.ArtifactPrompt {
				t.Errorf("prompt artifact %s (%s), want %s", got, fs.kinds[c.PromptSHA256], wantPrompt)
			}
			wantResp, _ := store.Canonical(resp)
			if got := fs.content[c.ResponseSHA256]; string(got) != string(wantResp) || fs.kinds[c.ResponseSHA256] != store.ArtifactResponse {
				t.Errorf("response artifact %s (%s), want %s", got, fs.kinds[c.ResponseSHA256], wantResp)
			}
		})
	}
}

func TestRecordingProviderRecordsAfterCancel(t *testing.T) {
	fs := newFakeArtifactStore()
	ctx, cancel := context.WithCancel(context.Background())
	inner := cancellingProvider{cancel: cancel}
	p := NewRecordingProvider(inner, fs, uuid.New(), uuid.New())
	if _, err := p.Complete(ctx, explainRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("llm_calls rows %d, want 1 even though the context was cancelled after the call", len(fs.calls))
	}
}

// cancellingProvider cancels the caller's context as the call returns.
type cancellingProvider struct{ cancel context.CancelFunc }

func (c cancellingProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	c.cancel()
	return llm.Response{Model: "claude-haiku-5-5", Text: "ok", Usage: llm.Usage{InputTokens: 1}}, nil
}

func TestRecordingProviderNeedsStep(t *testing.T) {
	inner := llm.NewFakeProvider()
	p := NewRecordingProvider(inner, newFakeArtifactStore(), uuid.New(), uuid.Nil)
	if _, err := p.Complete(context.Background(), explainRequest()); err == nil {
		t.Fatal("Complete without a step should fail")
	}
	if inner.CallCount() != 0 {
		t.Error("the model was called without a step to record against")
	}
}

// Recording after the call is bounded even though it ignores the caller's
// cancellation.
func TestRecordingProviderRecordTimeout(t *testing.T) {
	fs := newFakeArtifactStore()
	fs.block = true
	inner := llm.NewFakeProvider(llm.ScriptedCall{Response: llm.Response{Model: "claude-haiku-5-5", Text: "ok", Usage: llm.Usage{InputTokens: 1}}})
	p := NewRecordingProvider(inner, fs, uuid.New(), uuid.New())
	p.recordTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := p.Complete(context.Background(), explainRequest())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Complete: %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recording blocked past its timeout")
	}
	if inner.CallCount() != 1 {
		t.Errorf("model calls %d, want 1", inner.CallCount())
	}
	if DefaultRecordTimeout != 10*time.Second {
		t.Errorf("DefaultRecordTimeout %v, want 10s", DefaultRecordTimeout)
	}
}

// Usage reaches the store as exact decimal text, never through a float
// selected in this package.
func TestUsageOf(t *testing.T) {
	tests := []struct {
		in   llm.Usage
		want usageWire
	}{
		{llm.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CostUSD: 0.000123}, usageWire{1, 2, 3, "0.000123"}},
		{llm.Usage{InputTokens: 9007199254740993}, usageWire{9007199254740993, 0, 0, "0"}},
		{llm.Usage{CostUSD: 12.5}, usageWire{0, 0, 0, "12.5"}},
	}
	for _, tt := range tests {
		got, err := usageOf(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("usageOf(%+v) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}
