package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultCLITimeout      = 120 * time.Second
	defaultCLIRetryBackoff = 500 * time.Millisecond
)

// ProcessRunner runs a process with arguments and stdin, returning its outputs.
type ProcessRunner interface {
	Run(ctx context.Context, path string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// OSProcessRunner runs processes using the operating system's exec package.
type OSProcessRunner struct{}

// Run executes the command using exec.CommandContext.
func (r *OSProcessRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	//nolint:gosec // G204: path and arguments are configured for the local Claude CLI provider.
	cmd := exec.CommandContext(ctx, path, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// ClaudeCLIOption configures ClaudeCLI instances.
type ClaudeCLIOption func(*ClaudeCLI)

// WithCLITimeout configures the per-call CLI timeout.
func WithCLITimeout(d time.Duration) ClaudeCLIOption {
	return func(c *ClaudeCLI) {
		c.CallTimeout = d
	}
}

// WithCLIRetryBackoff configures the backoff before retrying on non-zero exit.
func WithCLIRetryBackoff(d time.Duration) ClaudeCLIOption {
	return func(c *ClaudeCLI) {
		c.RetryBackoff = d
	}
}

// ClaudeCLI implements Provider by executing the local `claude` CLI binary.
type ClaudeCLI struct {
	Path         string
	Runner       ProcessRunner
	Pricing      *PricingTable
	CallTimeout  time.Duration
	RetryBackoff time.Duration
}

// NewClaudeCLI creates a new ClaudeCLI provider.
func NewClaudeCLI(path string, runner ProcessRunner, pricing *PricingTable, opts ...ClaudeCLIOption) *ClaudeCLI {
	if path == "" {
		path = "claude"
	}
	if runner == nil {
		runner = &OSProcessRunner{}
	}
	cli := &ClaudeCLI{
		Path:         path,
		Runner:       runner,
		Pricing:      pricing,
		CallTimeout:  defaultCLITimeout,
		RetryBackoff: defaultCLIRetryBackoff,
	}
	for _, opt := range opts {
		opt(cli)
	}
	return cli
}

// Complete executes a request using `claude -p` with retry on failure.
func (c *ClaudeCLI) Complete(ctx context.Context, req Request) (Response, error) {
	args, cleanup, err := c.buildArgs(req)
	if err != nil {
		return Response{}, err
	}
	defer cleanup()

	stdin := c.buildStdin(req)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, c.CallTimeout)
		stdout, stderr, runErr := c.Runner.Run(callCtx, c.Path, args, stdin)
		cancel()

		if runErr == nil {
			return c.parseOutput(stdout, req)
		}

		lastErr = fmt.Errorf("claude cli exit error: %w (stderr: %s)", runErr, string(stderr))
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(c.RetryBackoff):
			}
		}
	}

	return Response{}, fmt.Errorf("llm claude cli failed after retry: %w", lastErr)
}

func (c *ClaudeCLI) buildArgs(req Request) ([]string, func(), error) {
	cleanup := func() {}

	args := []string{
		"-p",
		"--output-format", "json",
		"--no-session-persistence",
		"--tools", "",
	}

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	// System prompt file
	if len(req.System) > 0 {
		var sb strings.Builder
		for i, block := range req.System {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(block.Text)
		}

		tmpFile, err := os.CreateTemp("", "claude-sysprompt-*.txt")
		if err != nil {
			return nil, cleanup, fmt.Errorf("llm: create temp system prompt file: %w", err)
		}
		tmpPath := tmpFile.Name()
		cleanup = func() {
			_ = os.Remove(tmpPath)
		}

		if _, err := tmpFile.WriteString(sb.String()); err != nil {
			_ = tmpFile.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("llm: write temp system prompt file: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("llm: close temp system prompt file: %w", err)
		}

		args = append(args, "--system-prompt-file", tmpPath)
	}

	// Schema for structured output / forced tool / emulated tool calls
	if req.ForceTool != "" {
		var schema string
		for _, t := range req.Tools {
			if t.Name == req.ForceTool {
				schema = string(t.InputSchema)
				break
			}
		}
		if schema != "" {
			args = append(args, "--json-schema", schema)
		}
	} else if len(req.Tools) == 1 {
		args = append(args, "--json-schema", string(req.Tools[0].InputSchema))
	} else if len(req.Tools) > 1 {
		combinedSchema, err := buildMultiToolSchema(req.Tools)
		if err == nil {
			args = append(args, "--json-schema", combinedSchema)
		}
	}

	return args, cleanup, nil
}

func buildMultiToolSchema(tools []ToolSpec) (string, error) {
	toolNames := make([]string, len(tools))
	for i, t := range tools {
		toolNames[i] = t.Name
	}

	schemaObj := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tool_name": map[string]interface{}{
				"type": "string",
				"enum": toolNames,
			},
			"tool_args": map[string]interface{}{
				"type": "object",
			},
		},
		"required": []string{"tool_name", "tool_args"},
	}

	data, err := json.Marshal(schemaObj)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *ClaudeCLI) buildStdin(req Request) []byte {
	if len(req.Messages) == 1 &&
		req.Messages[0].Role == "user" &&
		len(req.Messages[0].ToolCalls) == 0 &&
		len(req.Messages[0].ToolResults) == 0 {
		return []byte(req.Messages[0].Content)
	}

	var sb strings.Builder
	for i, m := range req.Messages {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		sb.WriteString(strings.ToUpper(role))
		sb.WriteString(":\n")
		if m.Content != "" {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		for _, call := range m.ToolCalls {
			fmt.Fprintf(&sb, "Tool Call [%s (id: %s)]: %s\n", call.Name, call.ID, string(call.Args))
		}
		for _, res := range m.ToolResults {
			status := "success"
			if res.IsError {
				status = "error"
			}
			fmt.Fprintf(&sb, "Tool Result [%s, %s]: %s\n", res.ToolCallID, status, res.Content)
		}
	}

	return []byte(sb.String())
}

type cliOutput struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     *float64        `json:"total_cost_usd"`
	Model            string          `json:"model"`
	Usage            struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (c *ClaudeCLI) parseOutput(raw []byte, req Request) (Response, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Response{}, errors.New("llm: empty output from claude cli")
	}

	var out cliOutput
	if err := json.Unmarshal(trimmed, &out); err != nil {
		// Attempt fallback if there was leading/trailing text around JSON
		start := bytes.IndexByte(trimmed, '{')
		end := bytes.LastIndexByte(trimmed, '}')
		if start >= 0 && end > start {
			if retryErr := json.Unmarshal(trimmed[start:end+1], &out); retryErr != nil {
				return Response{}, fmt.Errorf("llm: unmarshal claude cli json: %w (raw: %s)", err, string(trimmed))
			}
		} else {
			return Response{}, fmt.Errorf("llm: unmarshal claude cli json: %w (raw: %s)", err, string(trimmed))
		}
	}

	resp := Response{
		Text: out.Result,
		Usage: Usage{
			InputTokens:      out.Usage.InputTokens,
			OutputTokens:     out.Usage.OutputTokens,
			CacheWriteTokens: out.Usage.CacheCreationInputTokens,
			CacheReadTokens:  out.Usage.CacheReadInputTokens,
		},
	}

	if out.TotalCostUSD != nil {
		resp.Usage.CostUSD = *out.TotalCostUSD
	} else if c.Pricing != nil {
		model := out.Model
		if model == "" {
			model = req.Model
		}
		resp.Usage.CostUSD = c.Pricing.Cost(model, resp.Usage)
	}

	// Resolve tool calls
	if req.ForceTool != "" && len(out.StructuredOutput) > 0 {
		resp.ToolCalls = []ToolCall{{
			ID:   "call_0",
			Name: req.ForceTool,
			Args: out.StructuredOutput,
		}}
		resp.StopReason = "tool_use"
	} else if len(req.Tools) > 0 && len(out.StructuredOutput) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out.StructuredOutput, &obj); err == nil {
			var tName string
			for _, k := range []string{"tool_name", "tool", "name"} {
				if rawVal, ok := obj[k]; ok {
					_ = json.Unmarshal(rawVal, &tName)
					if tName != "" {
						break
					}
				}
			}

			var tArgs json.RawMessage
			for _, k := range []string{"tool_args", "input", "args"} {
				if rawVal, ok := obj[k]; ok {
					tArgs = rawVal
					break
				}
			}

			if tName != "" {
				if len(tArgs) == 0 {
					tArgs = json.RawMessage("{}")
				}
				resp.ToolCalls = []ToolCall{{
					ID:   "call_0",
					Name: tName,
					Args: tArgs,
				}}
				resp.StopReason = "tool_use"
			} else if len(req.Tools) == 1 {
				resp.ToolCalls = []ToolCall{{
					ID:   "call_0",
					Name: req.Tools[0].Name,
					Args: out.StructuredOutput,
				}}
				resp.StopReason = "tool_use"
			}
		}
	}

	if resp.StopReason == "" {
		resp.StopReason = "end_turn"
	}

	return resp, nil
}
