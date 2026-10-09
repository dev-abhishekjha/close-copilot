package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

	// maxRetriesCap bounds WithMaxRetries; values outside 0..maxRetriesCap are clamped.
	maxRetriesCap = 5
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
}

// WithBaseURL overrides the API base URL (useful in tests with httptest.Server).
func WithBaseURL(url string) AnthropicOption {
	return func(c *anthropicConfig) {
		c.baseURL = url
	}
}

// WithCallTimeout sets the per-call timeout (default 60s).
func WithCallTimeout(d time.Duration) AnthropicOption {
	return func(c *anthropicConfig) {
		c.callTimeout = d
	}
}

// WithMaxRetries sets the number of retries after the first attempt for
// 429/500/529. It is clamped to 0..5; a negative value means no retries.
func WithMaxRetries(retries int) AnthropicOption {
	return func(c *anthropicConfig) {
		c.maxRetries = retries
	}
}

// WithInitialBackoff sets the starting backoff duration before retrying. A
// non-positive value keeps the default.
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
	c.maxRetries = min(max(c.maxRetries, 0), maxRetriesCap)
	if c.initialBackoff <= 0 {
		c.initialBackoff = defaultInitialBackoff
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = defaultMaxBackoff
	}
	c.initialBackoff = min(c.initialBackoff, c.maxBackoff)

	httpClient := c.httpClient
	if httpClient == nil {
		var err error
		httpClient, err = httpx.New(cfg, httpx.WithTimeout(c.callTimeout))
		if err != nil {
			return nil, fmt.Errorf("llm: build httpx client: %w", err)
		}
	}

	apiKey := cfg.AnthropicAPIKey.Reveal()

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
//
// It retries 429, 500 and 529 up to the configured count with exponential
// backoff capped at maxBackoff. A Retry-After header is honoured when it is
// finite and non-negative; one longer than maxBackoff ends the retries and the
// error is returned rather than sleeping past the cap.
func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if err := validateTools(req); err != nil {
		return Response{}, err
	}
	params, err := p.buildParams(req)
	if err != nil {
		return Response{}, err
	}

	attempts := 0
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		attempts++
		callCtx, cancel := context.WithTimeout(ctx, p.callTimeout)
		msg, err := p.client.Messages.New(callCtx, params)
		cancel()

		if err == nil {
			return p.buildResponse(msg, req)
		}

		lastErr = err
		if ctx.Err() != nil || !p.isRetryable(err) || attempt == p.maxRetries {
			break
		}

		backoff, ok := p.backoffFor(err, attempt)
		if !ok {
			break
		}
		select {
		case <-ctx.Done():
			return Response{}, fmt.Errorf("llm anthropic completion: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}

	return Response{}, fmt.Errorf("llm anthropic completion failed after %d attempt(s): %w", attempts, lastErr)
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

func (p *AnthropicProvider) buildResponse(msg *anthropic.Message, req Request) (Response, error) {
	var resp Response
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Text += b.Text
		case anthropic.ToolUseBlock:
			if err := checkToolName(req, b.Name); err != nil {
				return Response{}, err
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:   b.ID,
				Name: b.Name,
				Args: b.Input,
			})
		}
	}

	resp.Model = string(msg.Model)
	if resp.Model == "" {
		resp.Model = req.Model
	}
	resp.StopReason = string(msg.StopReason)
	resp.Usage = Usage{
		InputTokens:      msg.Usage.InputTokens,
		OutputTokens:     msg.Usage.OutputTokens,
		CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadTokens:  msg.Usage.CacheReadInputTokens,
	}

	cost, err := p.pricing.Cost(resp.Model, resp.Usage)
	if err != nil {
		return resp, err
	}
	resp.Usage.CostUSD = cost
	return resp, nil
}

func (p *AnthropicProvider) isRetryable(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode == 500 || apiErr.StatusCode == 529
	}
	return false
}

// backoffFor returns how long to wait before retry number attempt+1. ok is
// false when the server asked for a wait longer than maxBackoff, in which case
// the caller stops retrying.
func (p *AnthropicProvider) backoffFor(err error, attempt int) (time.Duration, bool) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		if d, ok := parseRetryAfter(apiErr.Response.Header.Get("Retry-After"), time.Now()); ok {
			if d > p.maxBackoff {
				return 0, false
			}
			return d, true
		}
	}
	return expBackoff(p.initialBackoff, p.maxBackoff, attempt), true
}

// parseRetryAfter reads delay-seconds or an HTTP date. NaN, infinities,
// negative values and dates in the past are ignored (ok false). Very large
// values are returned saturated at math.MaxInt64 so the caller's cap applies
// without overflow.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
			return 0, false
		}
		if secs >= float64(math.MaxInt64)/float64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d >= 0 {
			return d, true
		}
	}
	return 0, false
}

// expBackoff doubles initial once per attempt, saturating at maxBackoff so the
// shift can never overflow.
func expBackoff(initial, maxBackoff time.Duration, attempt int) time.Duration {
	b := initial
	for i := 0; i < attempt; i++ {
		if b >= maxBackoff/2 {
			return maxBackoff
		}
		b *= 2
	}
	return min(b, maxBackoff)
}
