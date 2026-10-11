package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// Span names.
const (
	SpanCloseRun = "close_run"
	// SpanCheckPrefix is followed by the check name.
	SpanCheckPrefix = "check."
	SpanExplain     = "explain"
	SpanVerify      = "verify"
	// SpanInvestigate is reserved for the investigator (CC-706); nothing
	// emits it yet.
	SpanInvestigate = "investigate"
	SpanChat        = "gen_ai.chat"
	// SpanToolPrefix is followed by the tool name.
	SpanToolPrefix = "mcp.tools/call "
)

// Attribute keys.
const (
	AttrRequestModel  = "gen_ai.request.model"
	AttrResponseModel = "gen_ai.response.model"
	AttrUsageIn       = "gen_ai.usage.input_tokens"
	AttrUsageOut      = "gen_ai.usage.output_tokens"
	AttrCostUSD       = "gen_ai.usage.cost_usd"
	AttrToolName      = "gen_ai.tool.name"
	AttrMCPMethod     = "mcp.method.name"
)

// EnvTracePrompts turns on prompt span events when "true". Local use only.
const EnvTracePrompts = "TRACE_PROMPTS"

const tracerName = "github.com/abhishekjha/close-copilot"

// PromptsEnabled reports whether TRACE_PROMPTS=true.
func PromptsEnabled() bool { return os.Getenv(EnvTracePrompts) == "true" }

func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// Start starts a span named name.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartTool starts the server-side span of one MCP tool call.
func StartTool(ctx context.Context, tool string) (context.Context, trace.Span) {
	return Start(ctx, SpanToolPrefix+tool,
		attribute.String(AttrToolName, tool), attribute.String(AttrMCPMethod, "tools/call"))
}

// Fail marks span failed with a fixed description; err's text can carry
// identifiers or secrets, so it never reaches the backend.
func Fail(span trace.Span, description string) { span.SetStatus(codes.Error, description) }

// TraceID is the trace ID of the span in ctx, or "" when there is none.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.TraceID().IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// Setup installs the global tracer provider and W3C propagator. Without
// cfg.OTLPEndpoint it installs a no-op tracer and returns a no-op shutdown.
// shutdown flushes pending spans.
func Setup(ctx context.Context, serviceName string, cfg config.Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if cfg.OTLPEndpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	endpoint, err := tracesURL(cfg.OTLPEndpoint)
	if err != nil {
		return nil, err
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if h := cfg.OTLPHeaders.Reveal(); h != "" {
		headers, err := parseHeaders(h)
		if err != nil {
			return nil, err
		}
		opts = append(opts, otlptracehttp.WithHeaders(headers))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}
	return install(serviceName, sdktrace.NewBatchSpanProcessor(exp))
}

// install makes a tracer provider with processor global.
func install(serviceName string, sp sdktrace.SpanProcessor) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSpanProcessor(sp))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func tracesURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", errors.New("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT is not a valid URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/traces"
	return u.String(), nil
}

// parseHeaders parses "k=v,k2=v2". Errors never quote the input: it holds
// the exporter's credentials.
func parseHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, errors.New("telemetry: OTEL_EXPORTER_OTLP_HEADERS is not a list of key=value pairs")
		}
		if dec, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			v = dec
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// WrapTransport traces the requests rt sends and carries the trace on in
// traceparent. Put it inside the egress guard.
func WrapTransport(rt http.RoundTripper) http.RoundTripper { return otelhttp.NewTransport(rt) }

// WrapHandler continues the caller's trace for each request h serves.
func WrapHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "mcp.http", otelhttp.WithFilter(func(r *http.Request) bool {
		return r.URL.Path != "/healthz"
	}))
}

// Init is Setup for a command's main: it returns a stop func that flushes
// pending spans for up to five seconds, on a context that outlives ctx.
func Init(ctx context.Context, serviceName string, cfg config.Config, log *slog.Logger) (func(), error) {
	shutdown, err := Setup(ctx, serviceName, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.OTLPEndpoint != "" {
		log.Info("tracing on", "service", serviceName)
	}
	return func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdown(sctx); err != nil {
			log.Warn("trace flush failed", "err", err)
		}
	}, nil
}
