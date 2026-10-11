// Package telemetrytest records spans in memory for tests. It swaps the
// process-wide tracer provider, so tests that use it must not run in
// parallel with each other.
package telemetrytest

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// Off makes the no-op tracer global until the test ends.
func Off(t testing.TB) {
	t.Helper()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

// Install makes a recording tracer provider and the W3C propagator global
// until the test ends, and returns the recorder.
func Install(t testing.TB) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		// Never restore the earlier global provider: the default one
		// delegates to the first provider ever set.
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	return rec
}

// Strings is every attribute value of every ended span and span event in
// rec, formatted as text, with the attribute key in front.
func Strings(rec *tracetest.SpanRecorder) []string {
	var out []string
	add := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			out = append(out, string(kv.Key)+"="+kv.Value.String())
		}
	}
	for _, s := range rec.Ended() {
		out = append(out, "name="+s.Name(), "status="+s.Status().Description)
		add(s.Attributes())
		add(s.Resource().Attributes())
	}
	return out
}

// EventStrings is the text of every span event attribute in rec.
func EventStrings(rec *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range rec.Ended() {
		for _, e := range s.Events() {
			for _, kv := range e.Attributes {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}
