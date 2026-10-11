// Package llmtrace decorates an llm.Provider with a gen_ai.chat span. It is a
// separate package because llm imports httpx, which imports telemetry.
package llmtrace

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
)

type tracedProvider struct {
	next    llm.Provider
	prompts bool
}

// Provider decorates p so every call records one gen_ai.chat span with the
// model, token counts and cost. Message text is never an attribute; with
// TRACE_PROMPTS=true it is added as span events.
func Provider(p llm.Provider) llm.Provider {
	return &tracedProvider{next: p, prompts: telemetry.PromptsEnabled()}
}

func (t *tracedProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanChat, attribute.String(telemetry.AttrRequestModel, req.Model))
	defer span.End()
	if t.prompts {
		span.AddEvent("gen_ai.prompt", trace.WithAttributes(attribute.String("content", promptText(req))))
	}
	resp, err := t.next.Complete(ctx, req)
	if err != nil {
		telemetry.Fail(span, "llm call failed")
		return resp, err
	}
	if resp.Model != "" {
		span.SetAttributes(attribute.String(telemetry.AttrResponseModel, resp.Model))
	}
	span.SetAttributes(
		attribute.Int64(telemetry.AttrUsageIn, resp.Usage.InputTokens),
		attribute.Int64(telemetry.AttrUsageOut, resp.Usage.OutputTokens),
		attribute.Float64(telemetry.AttrCostUSD, resp.Usage.CostUSD),
	)
	if t.prompts && resp.Text != "" {
		span.AddEvent("gen_ai.completion", trace.WithAttributes(attribute.String("content", resp.Text)))
	}
	return resp, nil
}

func promptText(req llm.Request) string {
	var b strings.Builder
	for _, s := range req.System {
		b.WriteString(s.Text)
		b.WriteString("\n")
	}
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
