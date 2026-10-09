package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

type mockRunner struct {
	mu        sync.Mutex
	calls     []mockCall
	responses []mockResponse
}

type mockCall struct {
	Path  string
	Args  []string
	Stdin []byte
}

type mockResponse struct {
	Stdout []byte
	Stderr []byte
	Err    error
}

func (m *mockRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, mockCall{
		Path:  path,
		Args:  append([]string(nil), args...),
		Stdin: append([]byte(nil), stdin...),
	})

	if len(m.responses) > 0 {
		resp := m.responses[0]
		m.responses = m.responses[1:]
		return resp.Stdout, resp.Stderr, resp.Err
	}

	return []byte(`{"type":"result","result":"ok"}`), nil, nil
}

func TestClaudeCLI_SimpleText(t *testing.T) {
	ctx := context.Background()
	runner := &mockRunner{
		responses: []mockResponse{{
			Stdout: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "Hello from Claude",
				"model": "haiku",
				"usage": {
					"input_tokens": 100,
					"output_tokens": 20,
					"cache_creation_input_tokens": 10,
					"cache_read_input_tokens": 5
				}
			}`),
		}},
	}

	pricing := &PricingTable{
		Models: map[string]ModelPricing{
			"haiku": {Input: 1.0, Output: 5.0, CacheWrite: 1.25, CacheRead: 0.1},
		},
	}

	cli := NewClaudeCLI("claude", runner, pricing)
	req := Request{
		Model:    "haiku",
		Messages: []Message{NewUserTextMessage("Hello Claude")},
	}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Complete error: %v", err)
	}

	if resp.Text != "Hello from Claude" {
		t.Errorf("expected text 'Hello from Claude', got %q", resp.Text)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("expected stop reason 'end_turn', got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
	if resp.Usage.CostUSD <= 0 {
		t.Errorf("expected non-zero CostUSD, got %f", resp.Usage.CostUSD)
	}

	// Verify runner arguments
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.calls))
	}
	args := runner.calls[0].Args
	if !slices.Contains(args, "-p") || !slices.Contains(args, "--output-format") {
		t.Errorf("missing standard flags in args: %v", args)
	}
	if string(runner.calls[0].Stdin) != "Hello Claude" {
		t.Errorf("expected stdin 'Hello Claude', got %q", string(runner.calls[0].Stdin))
	}
}

func TestClaudeCLI_ForcedTool(t *testing.T) {
	ctx := context.Background()
	runner := &mockRunner{
		responses: []mockResponse{{
			Stdout: []byte(`{
				"type": "result",
				"subtype": "success",
				"structured_output": {
					"explanation": "Bank charge verified",
					"needs_review": false
				},
				"total_cost_usd": 0.0042,
				"model": "sonnet",
				"usage": {
					"input_tokens": 200,
					"output_tokens": 50
				}
			}`),
		}},
	}

	cli := NewClaudeCLI("claude", runner, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"explanation":{"type":"string"}}}`)
	req := Request{
		Model: "sonnet",
		Tools: []ToolSpec{{
			Name:        "emit_explanation",
			InputSchema: schema,
		}},
		ForceTool: "emit_explanation",
		Messages:  []Message{NewUserTextMessage("explain finding")},
	}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Complete error: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "emit_explanation" {
		t.Errorf("expected tool name 'emit_explanation', got %q", resp.ToolCalls[0].Name)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("expected stop reason 'tool_use', got %q", resp.StopReason)
	}
	if resp.Usage.CostUSD != 0.0042 {
		t.Errorf("expected CostUSD 0.0042 from total_cost_usd, got %f", resp.Usage.CostUSD)
	}

	// Verify --json-schema flag
	args := runner.calls[0].Args
	idx := slices.Index(args, "--json-schema")
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("expected --json-schema flag in args: %v", args)
	}
	if args[idx+1] != string(schema) {
		t.Errorf("expected schema %s, got %s", string(schema), args[idx+1])
	}
}

func TestClaudeCLI_EmulatedMultiTool(t *testing.T) {
	ctx := context.Background()
	runner := &mockRunner{
		responses: []mockResponse{{
			Stdout: []byte(`{
				"type": "result",
				"subtype": "success",
				"structured_output": {
					"tool_name": "read_books",
					"tool_args": {"account": "Bank"}
				},
				"model": "haiku",
				"usage": {
					"input_tokens": 150,
					"output_tokens": 30
				}
			}`),
		}},
	}

	cli := NewClaudeCLI("claude", runner, nil)
	req := Request{
		Model: "haiku",
		Tools: []ToolSpec{
			{Name: "read_books", InputSchema: json.RawMessage(`{}`)},
			{Name: "emit_resolution", InputSchema: json.RawMessage(`{}`)},
		},
		Messages: []Message{NewUserTextMessage("investigate")},
	}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Complete error: %v", err)
	}

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_books" {
		t.Fatalf("expected read_books tool call, got %+v", resp.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &args); err != nil {
		t.Fatalf("failed to unmarshal tool args: %v", err)
	}
	if args["account"] != "Bank" {
		t.Errorf("expected account 'Bank', got %v", args["account"])
	}
}

func TestClaudeCLI_SystemPromptFileCleanup(t *testing.T) {
	ctx := context.Background()
	var sysPath string
	runner := &mockRunner{
		responses: []mockResponse{{
			Stdout: []byte(`{"type":"result","result":"done"}`),
		}},
	}

	cli := NewClaudeCLI("claude", runner, nil)
	req := Request{
		System: []Block{
			{Text: "System line 1"},
			{Text: "System line 2"},
		},
		Messages: []Message{NewUserTextMessage("hi")},
	}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Complete error: %v", err)
	}
	if resp.Text != "done" {
		t.Errorf("expected 'done', got %q", resp.Text)
	}

	// Inspect syspath passed
	args := runner.calls[0].Args
	idx := slices.Index(args, "--system-prompt-file")
	if idx == -1 || idx+1 >= len(args) {
		t.Fatalf("expected --system-prompt-file flag in args: %v", args)
	}
	sysPath = args[idx+1]

	// Verify temp file is removed after Complete returns
	if _, err := os.Stat(sysPath); !os.IsNotExist(err) {
		t.Errorf("expected temp system prompt file to be removed, but it exists: %s", sysPath)
	}
}

func TestClaudeCLI_RetryOnNonZeroExit(t *testing.T) {
	ctx := context.Background()
	runner := &mockRunner{
		responses: []mockResponse{
			{Stderr: []byte("transient error"), Err: errors.New("exit status 1")},
			{Stdout: []byte(`{"type":"result","result":"success after retry"}`)},
		},
	}

	cli := NewClaudeCLI("claude", runner, nil, WithCLIRetryBackoff(1*time.Millisecond))
	req := Request{Messages: []Message{NewUserTextMessage("test")}}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	if resp.Text != "success after retry" {
		t.Errorf("expected 'success after retry', got %q", resp.Text)
	}
	if len(runner.calls) != 2 {
		t.Errorf("expected 2 calls (initial + retry), got %d", len(runner.calls))
	}
}
