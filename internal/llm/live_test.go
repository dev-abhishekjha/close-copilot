//go:build live

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
		model = "claude-haiku-4-5-20251001"
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

// TestLiveClaudeCLIIsolation runs the provider's exact command with
// stream-json output and checks the CLI's init event: no tools, no MCP
// servers, no slash commands or skills, and the empty temp working directory.
func TestLiveClaudeCLIIsolation(t *testing.T) {
	cliPath := os.Getenv("CLAUDE_CLI_PATH")
	if cliPath == "" {
		cliPath = "claude"
	}
	cli := NewClaudeCLI(cliPath, &OSProcessRunner{}, nil)
	cmd, cleanup, err := cli.prepare(Request{
		Model:    "haiku",
		System:   []Block{{Text: "Reply with the single word ok."}},
		Messages: []Message{NewUserTextMessage("ping")},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer cleanup()

	i := slices.Index(cmd.Args, "--output-format")
	if i < 0 {
		t.Fatalf("no --output-format in %q", cmd.Args)
	}
	cmd.Args[i+1] = "stream-json"
	cmd.Args = append(cmd.Args, "--verbose") // stream-json with -p requires --verbose

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stdout, stderr, err := (&OSProcessRunner{}).Run(ctx, cmd)
	if err != nil && bytes.Contains(stderr, []byte("unknown option")) {
		t.Fatalf("the CLI rejected an isolation flag: %s", snippet(stderr))
	}
	if err != nil {
		t.Skipf("live claude CLI call failed (probably not authenticated or installed): %v", err)
	}

	var init struct {
		Type          string            `json:"type"`
		Subtype       string            `json:"subtype"`
		Cwd           string            `json:"cwd"`
		Tools         []string          `json:"tools"`
		MCPServers    []json.RawMessage `json:"mcp_servers"`
		SlashCommands []string          `json:"slash_commands"`
		Skills        []string          `json:"skills"`
		APIKeySource  string            `json:"apiKeySource"`
	}
	found := false
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), maxCLIStdout)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &init); err == nil && init.Type == "system" && init.Subtype == "init" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no init event in stream-json output")
	}
	if init.Tools == nil || len(init.Tools) != 0 {
		t.Errorf("expected zero tools, got %v", init.Tools)
	}
	if init.MCPServers == nil || len(init.MCPServers) != 0 {
		t.Errorf("expected zero MCP servers, got %d", len(init.MCPServers))
	}
	if len(init.SlashCommands) != 0 || len(init.Skills) != 0 {
		t.Errorf("expected no slash commands or skills, got %v / %v", init.SlashCommands, init.Skills)
	}
	if init.APIKeySource != "" && init.APIKeySource != "none" {
		t.Errorf("CLI picked up an API key from %q; the subprocess env must not carry one", init.APIKeySource)
	}
	wantDir, _ := filepath.EvalSymlinks(cmd.Dir)
	gotDir, _ := filepath.EvalSymlinks(init.Cwd)
	if gotDir != wantDir {
		t.Errorf("cwd = %q, want the empty temp dir %q", init.Cwd, cmd.Dir)
	}
}
