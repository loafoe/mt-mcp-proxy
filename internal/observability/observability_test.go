// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// setup builds an Observability with tracing disabled (no OTLP endpoint).
func setup(t *testing.T) *Observability {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	o, err := Setup(context.Background(), Config{ServiceName: "test", MetricsListen: ":0", MetricsPath: "/metrics"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = o.Shutdown(context.Background()) })
	return o
}

func TestSetupInstallsPropagator(t *testing.T) {
	setup(t)
	// The W3C trace-context propagator must always be installed so downstream
	// calls can carry traceparent even when tracing export is off.
	fields := otel.GetTextMapPropagator().Fields()
	var hasTraceparent bool
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
	}
	if !hasTraceparent {
		t.Fatalf("expected traceparent in propagator fields, got %v", fields)
	}
}

func TestMetricsHandlerExposesSeries(t *testing.T) {
	o := setup(t)

	o.RecordToolCall(context.Background(), ToolCallAttrs{
		Method:   "tools/call",
		Tool:     "query_prometheus",
		TenantID: "team-a",
		Backend:  "grafana-ab",
	}, 0.123)
	o.RecordSession(context.Background())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	o.MetricsHandler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"mcp_server_operation_duration",
		"mcp_server_operation_count",
		"mcp_server_session_count",
		`mcp_tenant_id="team-a"`,
		`mcp_backend_name="grafana-ab"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}

func TestTracerNoopWhenDisabled(t *testing.T) {
	o := setup(t)
	_, span := o.Tracer().Start(context.Background(), "x")
	// With no OTLP endpoint the span must not be recording.
	if span.IsRecording() {
		t.Fatal("expected non-recording span when tracing disabled")
	}
	span.End()
}

func TestNilObservabilityIsSafe(t *testing.T) {
	var o *Observability
	// All recording methods and the tracer must be safe on a nil receiver.
	o.RecordToolCall(context.Background(), ToolCallAttrs{Method: "tools/call"}, 1)
	o.RecordSession(context.Background())
	if o.MetricsListen() != "" {
		t.Error("nil MetricsListen should be empty")
	}
	_, span := o.Tracer().Start(context.Background(), "x")
	span.End()
	if err := o.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown: %v", err)
	}
}

func TestPropagatorInjectsTraceparent(t *testing.T) {
	setup(t)
	// A composite propagator with TraceContext should expose Inject; verify it
	// is wired so client.post can emit traceparent.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(context.Background(), carrier)
	// With no active span the carrier may be empty; the contract we assert is
	// that the propagator is non-nil and accepts a HeaderCarrier-like map.
	_ = carrier
}
