package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans installs an in-memory provider so a test can read what production would export.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	// Production sets this in SetupTracing; a test that skipped it would prove only that a span
	// exists, not that it leaves the process.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

func spanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func attrOf(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// A trace is exported to a third party by definition, so the rule that keeps user ids out of
// metric labels applies here too — but a trace without an identity is still useful, because the
// id comes back in a header instead.
func TestTraceToolCallCarriesNoUserID(t *testing.T) {
	rec := recordSpans(t)
	base, _, _ := testGateway(t)

	req, _ := http.NewRequest(http.MethodPost, base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"hi"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-owner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	traceIDHeader := resp.Header.Get("X-Trace-Id")
	if len(traceIDHeader) != 32 {
		t.Fatalf("X-Trace-Id: want a 32-hex trace id, got %q", traceIDHeader)
	}

	spans := rec.Ended()
	tool := spanByName(spans, "mcp.tool")
	if tool == nil {
		t.Fatalf("no mcp.tool span; got %d spans", len(spans))
	}
	if got := attrOf(tool, "mcp.tool"); got != "echo" {
		t.Fatalf("mcp.tool attribute: want echo, got %q", got)
	}
	if got := attrOf(tool, "gateway.cache"); got != "miss" {
		t.Fatalf("cache attribute: want miss, got %q", got)
	}
	if tool.SpanContext().TraceID().String() != traceIDHeader {
		t.Fatalf("header %s is not the span's trace %s", traceIDHeader, tool.SpanContext().TraceID())
	}

	// The user is 1001, the role is owner: the role may be on a span, the id may not.
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.Emit(), "1001") {
				t.Fatalf("span %q carries the user id in %q", s.Name(), a.Key)
			}
		}
		if attrOf(s, "gateway.role") != "owner" {
			continue
		}
	}
	if got := attrOf(tool, "gateway.role"); got != "owner" {
		t.Fatalf("role attribute: want owner, got %q", got)
	}
}

// A span that stops at the gateway boundary cannot answer "which service was slow", which is the
// only question worth tracing.
func TestTraceReachesUpstreamService(t *testing.T) {
	recordSpans(t)
	var gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}}}})
	}))
	defer upstream.Close()

	ctx, span := Tracer().Start(context.Background(), "test")
	defer span.End()
	client := &Client{URL: upstream.URL}
	if _, err := client.CallTimeoutCtx(ctx, "anything", map[string]any{}, "1001", 0); err != nil {
		t.Fatalf("call: %v", err)
	}

	parts := strings.Split(gotHeader, "-")
	if len(parts) != 4 || parts[0] != "00" {
		t.Fatalf("traceparent: want 4 dash-separated fields, got %q", gotHeader)
	}
	if parts[1] != span.SpanContext().TraceID().String() {
		t.Fatalf("upstream trace id %q is not the caller's %s", parts[1], span.SpanContext().TraceID())
	}
}

// Route templates keep a dashboard from growing one row per user id.
func TestRouteTemplate(t *testing.T) {
	cases := map[string]string{
		"/health":                "/health",
		"/admin/users":           "/admin/users",
		"/admin/users/1001/role": "/admin/users/{id}/role",
		"/v1/respond":            "/v1/respond",
		"/":                      "/",
	}
	for in, want := range cases {
		if got := routeTemplate(in); got != want {
			t.Fatalf("routeTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoutesNameSpansByRoute(t *testing.T) {
	rec := recordSpans(t)
	base, _, _ := testGateway(t)
	resp, err := http.Get(base + "/admin/users/1001/role")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if s := spanByName(rec.Ended(), "GET /admin/users/{id}/role"); s == nil {
		names := []string{}
		for _, x := range rec.Ended() {
			names = append(names, x.Name())
		}
		t.Fatalf("want a span named by route, got %v", names)
	}
}
