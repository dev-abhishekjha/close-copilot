package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

const (
	defaultCallTimeout    = 60 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 500 * time.Millisecond
	defaultMaxBackoff     = 30 * time.Second
	defaultMaxTokens      = 4096
)

// AnthropicOption customizes the Anthropic provider.
type AnthropicOption func(*anthropicConfig)

type anthropicConfig struct {
	baseURL        string
	httpClient     *http.Client
	callTimeout    time.Duration
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	apiKeyOverride string
}

// WithBaseURL overrides the API base URL (useful in tests with httptest.Server).
func WithBaseURL(url string) AnthropicOption {
	return func(c *anthropicConfig) {
		c.baseURL = url
	}
}

// WithHTTPClient overrides the outbound HTTP client (useful in tests).
func WithHTTPClient(client *http.Client) AnthropicOption {
	return func(c *anthropicConfig) {
		c.httpClient = client
	}
}

// WithCallTimeout sets the per-call timeout (default 60s).
func WithCallTimeout(d time.Duration) AnthropicOption {
	return func(c *anthropicConfig) {
		c.callTimeout = d
	}
}

// WithMaxRetries sets the maximum number of retry attempts for 429/500/529.
func WithMaxRetries(retries int) AnthropicOption {
	return func(c *anthropicConfig) {
		c.maxRetries = retries
	}
}

// WithInitialBackoff sets the starting backoff duration before retrying.
func WithInitialBackoff(d time.Duration) AnthropicOption {
	return func(c *anthropicConfig) {
		c.initialBackoff = d
	}
}

// AnthropicProvider executes LLM completions via Anthropic's Messages API.
type AnthropicProvider struct {
	client         *anthropic.Client
	pricing        *PricingTable
	callTimeout    time.Duration
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// NewAnthropicProvider initializes an AnthropicProvider routed through internal/httpx.
func NewAnthropicProvider(cfg config.Config, pricing *PricingTable, opts ...AnthropicOption) (*AnthropicProvider, error) {
	c := anthropicConfig{
		callTimeout:    defaultCallTimeout,
		maxRetries:     defaultMaxRetries,
		initialBackoff: defaultInitialBackoff,
		maxBackoff:     defaultMaxBackoff,
	}
	for _, opt := range opts {
		opt(&c)
	}

	httpClient := c.httpClient
	if httpClient == nil {
		var err error
		httpClient, err = httpx.New(cfg, httpx.WithTimeout(c.callTimeout))
		if err != nil {
			return nil, fmt.Errorf("llm: build httpx client: %w", err)
		}
	}

	apiKey := cfg.AnthropicAPIKey.Reveal()
	if c.apiKeyOverride != "" {
		apiKey = c.apiKeyOverride
	}

	var sdkOpts []option.RequestOption
	sdkOpts = append(sdkOpts,
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0), // retries handled explicitly by AnthropicProvider
	)
	if apiKey != "" {
		sdkOpts = append(sdkOpts, option.WithAPIKey(apiKey))
	}
	if c.baseURL != "" {
		sdkOpts = append(sdkOpts, option.WithBaseURL(c.baseURL))
	}

	client := anthropic.NewClient(sdkOpts...)
	return &AnthropicProvider{
		client:         &client,
		pricing:        pricing,
		callTimeout:    c.callTimeout,
		maxRetries:     c.maxRetries,
		initialBackoff: c.initialBackoff,
		maxBackoff:     c.maxBackoff,
	}, nil
}

// Complete executes an LLM request against the Anthropic Messages API with retries,
// tool mapping, and prompt caching.
func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return Response{}, err
	}

	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, p.callTimeout)
		msg, err := p.client.Messages.New(callCtx, params)
		cancel()

		if err == nil {
			return p.buildResponse(msg, req.Model), nil
		}

		lastErr = err
		if !p.isRetryable(err) || attempt == p.maxRetries {
			break
		}

		backoff := p.backoffFor(err, attempt)
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(backoff):
		}
	}

	return Response{}, fmt.Errorf("llm anthropic completion failed: %w", lastErr)
}

func (p *AnthropicProvider) buildParams(req Request) (anthropic.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
	}

	// System blocks with prompt caching
	if len(req.System) > 0 {
		hasExplicitCacheable := false
		for _, b := range req.System {
			if b.Cacheable {
				hasExplicitCacheable = true
				break
			}
		}

		for i, b := range req.System {
			tb := anthropic.TextBlockParam{
				Text: b.Text,
			}
			// Mark cacheable if explicitly set, or on the last system block if none set
			if b.Cacheable || (!hasExplicitCacheable && i == len(req.System)-1) {
				tb.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
			params.System = append(params.System, tb)
		}
	}

	// Messages conversion
	for _, m := range req.Messages {
		var blocks []anthropic.ContentBlockParamUnion
		if m.Content != "" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Content))
		}
		for _, call := range m.ToolCalls {
			blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, call.Args, call.Name))
		}
		for _, res := range m.ToolResults {
			blocks = append(blocks, anthropic.NewToolResultBlock(res.ToolCallID, res.Content, res.IsError))
		}
		if len(blocks) == 0 {
			blocks = append(blocks, anthropic.NewTextBlock(""))
		}

		if m.Role == "assistant" {
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))
		} else {
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
		}
	}

	// Tools conversion
	for _, t := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return anthropic.MessageNewParams{}, fmt.Errorf("llm: unmarshal tool %q schema: %w", t.Name, err)
			}
		}
		tp := anthropic.ToolParam{
			Name:        t.Name,
			InputSchema: schema,
		}
		if t.Description != "" {
			tp.Description = param.NewOpt(t.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	// Force tool
	if req.ForceTool != "" {
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(req.ForceTool)
	}

	return params, nil
}

func (p *AnthropicProvider) buildResponse(msg *anthropic.Message, model string) Response {
	var resp Response
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Text += b.Text
		case anthropic.ToolUseBlock:
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:   b.ID,
				Name: b.Name,
				Args: b.Input,
			})
		}
	}

	resp.StopReason = string(msg.StopReason)
	resp.Usage = Usage{
		InputTokens:      msg.Usage.InputTokens,
		OutputTokens:     msg.Usage.OutputTokens,
		CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadTokens:  msg.Usage.CacheReadInputTokens,
	}

	if p.pricing != nil {
		effectiveModel := string(msg.Model)
		if effectiveModel == "" {
			effectiveModel = model
		}
		resp.Usage.CostUSD = p.pricing.Cost(effectiveModel, resp.Usage)
	}

	return resp
}

func (p *AnthropicProvider) isRetryable(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode == 500 || apiErr.StatusCode == 529
	}
	return false
}

func (p *AnthropicProvider) backoffFor(err error, attempt int) time.Duration {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		if ra := apiErr.Response.Header.Get("Retry-After"); ra != "" {
			if secs, parseErr := strconv.ParseFloat(ra, 64); parseErr == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
			if t, parseErr := http.ParseTime(ra); parseErr == nil {
				d := time.Until(t)
				if d > 0 {
					return d
				}
			}
		}
	}

	backoff := p.initialBackoff * time.Duration(1<<attempt)
	if backoff > p.maxBackoff {
		backoff = p.maxBackoff
	}
	return backoff
}
