// Package llm provides model-agnostic LLM provider interfaces and
// implementations with tool calling, prompt caching, retries, and per-call
// cost accounting (CC-701).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnknownTool reports a tool name that the request did not offer, either as
// ForceTool or in a model's tool call. Providers reject such calls rather than
// pass them on to a dispatcher.
var ErrUnknownTool = errors.New("llm: tool not offered in request")

// Provider executes structured LLM requests.
//
// A completed call whose model has no pricing row returns the Response (with
// token usage and Model set, CostUSD zero) together with an error wrapping
// ErrUnpricedModel. Callers must treat that as a failure; the usage is there
// only so the spend can still be recorded.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Block is a text segment in a prompt, optionally marked for caching.
type Block struct {
	Text      string
	Cacheable bool
}

// ToolResultBlock represents the result of executing a tool call, sent back to
// the model in a subsequent turn.
type ToolResultBlock struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
}

// ToolSpec defines a tool available to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall represents an invocation of a tool requested by the model.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Message is a conversational turn between the user and assistant.
type Message struct {
	Role        string            `json:"role"` // "user" or "assistant"
	Content     string            `json:"content,omitempty"`
	ToolCalls   []ToolCall        `json:"tool_calls,omitempty"`
	ToolResults []ToolResultBlock `json:"tool_results,omitempty"`
}

// NewUserTextMessage creates a simple user text message.
func NewUserTextMessage(text string) Message {
	return Message{
		Role:    "user",
		Content: text,
	}
}

// NewAssistantTextMessage creates an assistant text message.
func NewAssistantTextMessage(text string) Message {
	return Message{
		Role:    "assistant",
		Content: text,
	}
}

// NewAssistantToolCallMessage creates an assistant message requesting tool calls.
func NewAssistantToolCallMessage(calls ...ToolCall) Message {
	return Message{
		Role:      "assistant",
		ToolCalls: calls,
	}
}

// NewUserToolResultMessage creates a user message returning tool execution results.
func NewUserToolResultMessage(results ...ToolResultBlock) Message {
	return Message{
		Role:        "user",
		ToolResults: results,
	}
}

// Request is the model-agnostic completion request.
type Request struct {
	Model     string
	System    []Block
	Messages  []Message
	Tools     []ToolSpec
	ForceTool string // when set, the model must call this tool
	MaxTokens int
}

// Usage holds token consumption and computed cost for an LLM call.
type Usage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Response is the model completion output.
type Response struct {
	Model      string     `json:"model,omitempty"` // resolved model that served the call
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	Usage      Usage      `json:"usage"`
}

// validateTools checks that tool names are present and unique, and that
// ForceTool names an offered tool.
func validateTools(req Request) error {
	seen := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		if t.Name == "" {
			return errors.New("llm: tool with empty name")
		}
		if seen[t.Name] {
			return fmt.Errorf("llm: duplicate tool %q", t.Name)
		}
		seen[t.Name] = true
	}
	if req.ForceTool != "" && !seen[req.ForceTool] {
		return fmt.Errorf("%w: ForceTool %q", ErrUnknownTool, req.ForceTool)
	}
	return nil
}

// checkToolName reports whether name was offered in req.Tools.
func checkToolName(req Request, name string) error {
	for _, t := range req.Tools {
		if t.Name == name {
			return nil
		}
	}
	return fmt.Errorf("%w: model called %s", ErrUnknownTool, clip([]byte(name), 64))
}

// maxErrorSnippet bounds how much subprocess or model output an error carries.
const maxErrorSnippet = 512

// snippet renders untrusted output for an error message: control characters are
// replaced and the text is cut to maxErrorSnippet bytes, with the total length
// noted when cut. Model output can be derived from ledger, bank or document
// text, so errors never carry it whole.
func snippet(b []byte) string {
	return clip(b, maxErrorSnippet)
}

func clip(b []byte, limit int) string {
	var sb strings.Builder
	truncated := false
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			r = '?'
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			r = ' '
		}
		if sb.Len()+utf8.RuneLen(r) > limit {
			truncated = true
			break
		}
		sb.WriteRune(r)
		i += size
	}
	if truncated {
		return fmt.Sprintf("%q...(%d bytes total)", sb.String(), len(b))
	}
	return fmt.Sprintf("%q", sb.String())
}
