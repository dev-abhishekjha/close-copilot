//go:build live

package llm

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func TestLiveAnthropicToolCall(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}

	cfg := config.Config{
		AnthropicAPIKey: config.NewSecret(apiKey),
	}
	pricing, err := LoadPricing("")
	if err != nil {
		t.Logf("load pricing: %v; continuing without pricing table", err)
	}

	provider, err := NewAnthropicProvider(cfg, pricing)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}

	model := os.Getenv("LLM_MODEL_FAST")
	if model == "" {
		model = "claude-3-5-haiku-20241022"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"result": {"type": "string"},
			"confidence": {"type": "number"}
		},
		"required": ["result", "confidence"]
	}`)

	req := Request{
		Model: model,
		System: []Block{
			{Text: "You are a test evaluator. Call evaluate_test to respond.", Cacheable: true},
		},
		Messages: []Message{
			NewUserTextMessage("Please test tool execution with result 'pass' and confidence 1.0"),
		},
		Tools: []ToolSpec{{
			Name:        "evaluate_test",
			Description: "Evaluates the live test run",
			InputSchema: schema,
		}},
		ForceTool: "evaluate_test",
		MaxTokens: 1024,
	}

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		t.Fatalf("live Complete failed: %v", err)
	}

	if len(resp.ToolCalls) == 0 {
		t.Fatalf("expected at least 1 tool call, got none; text=%s", resp.Text)
	}
	if resp.ToolCalls[0].Name != "evaluate_test" {
		t.Errorf("expected tool call evaluate_test, got %s", resp.ToolCalls[0].Name)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("expected non-zero usage, got %+v", resp.Usage)
	}
	t.Logf("Live Anthropic call succeeded. Tool call: %s, Args: %s, Usage: %+v",
		resp.ToolCalls[0].Name, string(resp.ToolCalls[0].Args), resp.Usage)
}

func TestLiveClaudeCLIStructuredCall(t *testing.T) {
	cliPath := os.Getenv("CLAUDE_CLI_PATH")
	if cliPath == "" {
		cliPath = "claude"
	}

	pricing, err := LoadPricing("")
	if err != nil {
		t.Logf("load pricing: %v; continuing without pricing table", err)
	}

	cli := NewClaudeCLI(cliPath, &OSProcessRunner{}, pricing)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"status": {"type": "string", "enum": ["ok", "fail"]},
			"message": {"type": "string"}
		},
		"required": ["status", "message"]
	}`)

	req := Request{
		Model: "haiku",
		System: []Block{
			{Text: "Respond via emit_status with status 'ok' and message 'live cli pass'."},
		},
		Messages: []Message{
			NewUserTextMessage("Return status"),
		},
		Tools: []ToolSpec{{
			Name:        "emit_status",
			Description: "Emits verification status",
			InputSchema: schema,
		}},
		ForceTool: "emit_status",
	}

	resp, err := cli.Complete(ctx, req)
	if err != nil {
		t.Skipf("live claude CLI call failed (probably not authenticated or installed): %v", err)
	}

	if len(resp.ToolCalls) == 0 {
		t.Fatalf("expected tool call, got none; text=%s", resp.Text)
	}
	t.Logf("Live Claude CLI call succeeded. Tool: %s, Args: %s, Usage: %+v",
		resp.ToolCalls[0].Name, string(resp.ToolCalls[0].Args), resp.Usage)
}
