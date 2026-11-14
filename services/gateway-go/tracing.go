package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracing answers the question metrics cannot: which of the five things this request touched
// was slow. Metrics say "slow"; a span says "the OCR service, 4.1 s, and the gateway waited".
//
// Two rules, both learned from the metrics work and applied here unchanged:
//
//   - No user id in a span attribute. A trace is exported to a third party by definition; the
//     salaried answer already travels through this gateway, and an attribute is a place data
//     leaks and cardinality explodes. The trace id comes back in X-Trace-Id instead, so a user
//     who reports a problem carries a handle, not their identity.
//   - Off means off. With no OTLP endpoint there is no exporter and no span is recorded, rather
//     than pretending to trace into a void.
const tracerName = "agent-platform/gateway"

// SetupTracing installs a tracer provider and W3C propagation. An empty endpoint returns a
// shutdown function and no exporter: spans are collected by nothing, which is what "off" means.
func SetupTracing(ctx context.Context, endpoint, serviceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	opts := []sdktrace.TracerProviderOption{}
	if strings.TrimSpace(endpoint) != "" {
		httpOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(stripScheme(endpoint))}
		if !strings.HasPrefix(endpoint, "https://") {
			httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, httpOpts...)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	if serviceName == "" {
		serviceName = "agent-gateway"
	}
	opts = append(opts, sdktrace.WithResource(resourceWithService(serviceName)))
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func resourceWithService(name string) *resource.Resource {
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", name)))
	if err != nil {
		return resource.Default()
	}
	return res
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return strings.TrimPrefix(endpoint, "http://")
}

// Tracer is resolved per call so a test can swap the provider and still see the spans the
// production path creates.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// StartServerSpan continues an incoming trace when the caller sends traceparent, and starts one
// otherwise. The reference adapter and dsh both send it; a plain curl does not.
func StartServerSpan(ctx context.Context, r *http.Request, name string) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
	attrs := []attribute.KeyValue{
		attribute.String("http.method", r.Method),
		attribute.String("http.route", name),
	}
	return Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...))
}

// InjectContext puts the current trace into outgoing headers so the MCP service can join the
// same trace instead of starting a second, unrelated one.
func InjectContext(ctx context.Context, h http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

// TraceID returns the current trace id as hex, or "" when no trace is active. It is what the
// gateway puts in X-Trace-Id and in the audit line, and it carries no user data.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}
