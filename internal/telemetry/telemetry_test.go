package telemetry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
	"github.com/abhishekjha/close-copilot/internal/telemetry/llmtrace"
	"github.com/abhishekjha/close-copilot/internal/telemetry/telemetrytest"
)

func TestSetupNoEndpointIsNoop(t *testing.T) {
	telemetrytest.Install(t) // restores the global provider afterwards
	shutdown, err := telemetry.Setup(t.Context(), "svc", config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	ctx, span := telemetry.Start(t.Context(), "x")
	defer span.End()
	if span.SpanContext().IsValid() || telemetry.TraceID(ctx) != "" {
		t.Error("no-op tracer produced a valid span")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("an SDK provider is installed without an endpoint")
	}
}

func TestSetupErrorsNeverQuoteHeaders(t *testing.T) {
	telemetrytest.Install(t)
	cfg := config.Config{OTLPEndpoint: "https://otel.example.test/api"}
	cfg.OTLPHeaders = config.NewSecret("s3cret-no-equals")
	_, err := telemetry.Setup(t.Context(), "svc", cfg)
	if err == nil {
		t.Fatal("want an error for a malformed header list")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error quotes the header secret: %v", err)
	}
	cfg.OTLPEndpoint = "not a url"
	cfg.OTLPHeaders = config.NewSecret("")
	if _, err := telemetry.Setup(t.Context(), "svc", cfg); err == nil {
		t.Error("want an error for a bad endpoint")
	}
}

func TestSetupWithEndpointShutsDown(t *testing.T) {
	telemetrytest.Install(t)
	cfg := config.Config{OTLPEndpoint: "http://127.0.0.1:1/api/public/otel"}
	cfg.OTLPHeaders = config.NewSecret("Authorization=Basic abc")
	shutdown, err := telemetry.Setup(t.Context(), "svc", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// No span was started, so nothing is exported and no collector is hit.
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestTraceCrossesHTTP(t *testing.T) {
	rec := telemetrytest.Install(t)
	var serverTrace string
	srv := httptest.NewServer(telemetry.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := telemetry.StartTool(r.Context(), "list_bank_lines")
		serverTrace = span.SpanContext().TraceID().String()
		span.End()
	})))
	defer srv.Close()

	ctx, root := telemetry.Start(t.Context(), telemetry.SpanCloseRun)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := (&http.Client{Transport: telemetry.WrapTransport(http.DefaultTransport)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	root.End()

	if want := root.SpanContext().TraceID().String(); serverTrace != want {
		t.Errorf("server trace %q, client trace %q", serverTrace, want)
	}
	var tool bool
	for _, s := range rec.Ended() {
		if s.Name() == "mcp.tools/call list_bank_lines" {
			tool = true
		}
	}
	if !tool {
		t.Error("tool span missing")
	}
}

type fakeProvider struct{ err error }

func (f fakeProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Model: "m-resolved", Text: "ANSWER-TEXT", Usage: llm.Usage{InputTokens: 12, OutputTokens: 3, CostUSD: 0.5}}, f.err
}

func TestProviderSpan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts string
		err     error
	}{
		{"default", "", nil},
		{"prompts", "true", nil},
		{"error", "", errors.New("boom 29AAAAA0000A1Z5")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := telemetrytest.Install(t)
			t.Setenv(telemetry.EnvTracePrompts, tc.prompts)
			p := llmtrace.Provider(fakeProvider{err: tc.err})
			_, err := p.Complete(t.Context(), llm.Request{
				Model:    "m-req",
				System:   []llm.Block{{Text: "SYSTEM-PROMPT"}},
				Messages: []llm.Message{llm.NewUserTextMessage("USER-PROMPT 27AAAPA1234A1Z5")},
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			ended := rec.Ended()
			if len(ended) != 1 || ended[0].Name() != telemetry.SpanChat {
				t.Fatalf("spans %v", ended)
			}
			all := strings.Join(telemetrytest.Strings(rec), "\n")
			events := strings.Join(telemetrytest.EventStrings(rec), "\n")
			for _, bad := range []string{"SYSTEM-PROMPT", "USER-PROMPT", "ANSWER-TEXT", "29AAAAA0000A1Z5", "27AAAPA1234A1Z5", "boom"} {
				if strings.Contains(all, bad) {
					t.Errorf("attribute holds %q", bad)
				}
			}
			if tc.err == nil {
				for _, want := range []string{
					"gen_ai.request.model=m-req", "gen_ai.usage.input_tokens=12",
					"gen_ai.usage.output_tokens=3", "gen_ai.usage.cost_usd=0.5",
				} {
					if !strings.Contains(all, want) {
						t.Errorf("missing %q in\n%s", want, all)
					}
				}
			}
			hasPrompt := strings.Contains(events, "USER-PROMPT")
			if hasPrompt != (tc.prompts == "true") {
				t.Errorf("prompt event present=%v with TRACE_PROMPTS=%q", hasPrompt, tc.prompts)
			}
		})
	}
}

func TestTraceID(t *testing.T) {
	telemetrytest.Install(t)
	if telemetry.TraceID(t.Context()) != "" {
		t.Error("trace ID without a span")
	}
	ctx, span := telemetry.Start(t.Context(), "x")
	defer span.End()
	if got := telemetry.TraceID(ctx); got != trace.SpanFromContext(ctx).SpanContext().TraceID().String() || len(got) != 32 {
		t.Errorf("trace ID %q", got)
	}
}
