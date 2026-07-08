// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"testing"

	"github.com/loafoe/mt-mcp-proxy/internal/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestBackendCallPropagatesTraceparent verifies that when a span is active, the
// backend client injects a W3C traceparent header into the downstream request,
// so an OTel-enabled mcp-grafana joins the proxy's trace.
func TestBackendCallPropagatesTraceparent(t *testing.T) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	fb, _, cfgs := twoBackends(t)
	h := harness(t, cfgs)
	// Force the handler to build clients with a recording tracer.
	h.clientFor = func(b *registry.Backend) *backendClient {
		c := newBackendClient(b)
		c.tracer = tp.Tracer("test")
		return c
	}

	sid := initSession(h)
	tok := token(t, "team-a")
	w := rpc(h, "tools/call", map[string]any{
		"name":      "query_prometheus",
		"arguments": map[string]any{"query": "up"},
	}, tok, sid)
	if w.Code != 200 {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}

	if tp := fb.header("Traceparent"); tp == "" {
		t.Fatalf("backend did not receive a traceparent header; headers=%v", fb.lastHeaders)
	}
}
