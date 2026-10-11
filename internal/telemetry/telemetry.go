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
	"go.opentelemetry.io/otel/trace/embedded"
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
	// The exporter uses its own HTTP client, not httpx: the egress guard does
	// not cover it, but the endpoint is operator-set (OTEL_EXPORTER_OTLP_ENDPOINT,
	// already on the allowlist) and the client is not traced, so exporting
	// spans never creates spans of its own.
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
// traceparent. Put it inside the egress guard. Client spans keep only the
// method, host, scheme and status: the URL path and query can hold party and
// company names or GSTINs (ERPNext filters), so they never reach a span.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(rt, otelhttp.WithTracerProvider(scrubProvider{}))
}

// WrapHandler continues the caller's trace for each request h serves. Server
// spans are scrubbed like client spans.
func WrapHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "mcp.http", otelhttp.WithTracerProvider(scrubProvider{}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz"
		}))
}

// safeHTTPAttrs are the only attributes an HTTP span may carry.
var safeHTTPAttrs = map[attribute.Key]bool{
	"http.request.method": true, "http.method": true, "http.response.status_code": true,
	"http.status_code": true, "server.address": true, "server.port": true,
	"url.scheme": true, "http.scheme": true, "net.peer.name": true, "net.peer.port": true,
	"network.protocol.name": true, "network.protocol.version": true, "error.type": true,
}

func scrub(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if safeHTTPAttrs[kv.Key] {
			out = append(out, kv)
		}
	}
	return out
}

// scrubProvider hands otelhttp a tracer that drops every attribute outside
// safeHTTPAttrs, exception events and status text (error text embeds the
// URL). It looks up the global provider on each span, so it works whether
// Setup ran before or after the wrapper was built.
type scrubProvider struct{ embedded.TracerProvider }

func (scrubProvider) Tracer(string, ...trace.TracerOption) trace.Tracer { return scrubTracer{} }

type scrubTracer struct{ embedded.Tracer }

func (scrubTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	clean := []trace.SpanStartOption{trace.WithSpanKind(cfg.SpanKind()), trace.WithAttributes(scrub(cfg.Attributes())...)}
	if cfg.NewRoot() {
		clean = append(clean, trace.WithNewRoot())
	}
	if !cfg.Timestamp().IsZero() {
		clean = append(clean, trace.WithTimestamp(cfg.Timestamp()))
	}
	ctx, span := tracer().Start(ctx, name, clean...)
	return ctx, scrubSpan{span}
}

type scrubSpan struct{ trace.Span }

func (s scrubSpan) SetAttributes(kv ...attribute.KeyValue)  { s.Span.SetAttributes(scrub(kv)...) }
func (s scrubSpan) RecordError(error, ...trace.EventOption) {}
func (s scrubSpan) SetStatus(c codes.Code, _ string)        { s.Span.SetStatus(c, "") }
func (s scrubSpan) AddEvent(string, ...trace.EventOption)   {}

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
