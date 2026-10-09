// Package llm provides model-agnostic LLM provider interfaces and
// implementations with tool calling, prompt caching, retries, and per-call
// cost accounting (CC-701).
package llm

import (
	"context"
	"encoding/json"
)

// Provider executes structured LLM requests.
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
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	Usage      Usage      `json:"usage"`
}
