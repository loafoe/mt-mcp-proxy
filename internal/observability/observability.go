// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Package observability provides OpenTelemetry-based metrics and tracing for the
// proxy, modeled after mcp-grafana's observability package.
//
// Metrics use the OTel SDK with a Prometheus exporter, exposed on a separate
// scrape listener. Tracing uses an OTLP gRPC exporter and is enabled only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise the global tracer is a no-op.
// The W3C trace-context propagator is always installed so the proxy can inject
// traceparent headers into downstream mcp-grafana calls, letting those
// (OTel-enabled) instances join the same trace.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config controls metrics exposure and resource identity. Tracing is driven by
// OTEL_* environment variables, not these fields.
type Config struct {
	MetricsListen  string
	MetricsPath    string
	ServiceName    string
	ServiceVersion string
}

// Observability holds the providers and instruments. It is safe to use a nil
// *Observability: every method becomes a no-op, so callers need not branch.
type Observability struct {
	registry *prometheus.Registry
	tracer   trace.Tracer

	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider

	opDuration   metric.Float64Histogram
	opCount      metric.Int64Counter
	sessionCount metric.Int64Counter

	metricsListen string
	metricsPath   string
}

// Setup builds the metrics provider (always) and the tracer provider (only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set), installs them as globals along with the
// W3C trace-context + baggage propagator, and returns an *Observability.
func Setup(ctx context.Context, cfg Config) (*Observability, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "mt-mcp-proxy"
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	// Always install the propagator so downstream calls carry traceparent even
	// when this proxy exports nothing (a collector downstream may still record).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	o := &Observability{
		registry:      prometheus.NewRegistry(),
		metricsListen: cfg.MetricsListen,
		metricsPath:   cfg.MetricsPath,
	}

	// Metrics: OTel SDK MeterProvider exporting to a dedicated Prometheus
	// registry.
	promExp, err := promexporter.New(promexporter.WithRegisterer(o.registry))
	if err != nil {
		return nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	o.meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
		// Native-histogram-style explicit buckets for the operation duration.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "mcp.server.operation.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: mcpHistogramBuckets,
			}},
		)),
	)
	otel.SetMeterProvider(o.meterProvider)
	if err := o.initInstruments(); err != nil {
		return nil, err
	}

	// Tracing: only when an OTLP endpoint is configured.
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		exp, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otlp trace exporter: %w", err)
		}
		o.tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(exp),
		)
		otel.SetTracerProvider(o.tracerProvider)
	}
	// Tracer() resolves from the global provider, which is a no-op when no OTLP
	// provider was installed above.
	if o.tracerProvider != nil {
		o.tracer = o.tracerProvider.Tracer("github.com/loafoe/mt-mcp-proxy")
	} else {
		o.tracer = noop.NewTracerProvider().Tracer("github.com/loafoe/mt-mcp-proxy")
	}

	return o, nil
}

func (o *Observability) initInstruments() error {
	meter := o.meterProvider.Meter("github.com/loafoe/mt-mcp-proxy")
	var err error
	o.opDuration, err = meter.Float64Histogram(
		"mcp.server.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP operations handled by the proxy."),
	)
	if err != nil {
		return fmt.Errorf("operation duration histogram: %w", err)
	}
	o.opCount, err = meter.Int64Counter(
		"mcp.server.operation.count",
		metric.WithDescription("Count of MCP operations handled by the proxy."),
	)
	if err != nil {
		return fmt.Errorf("operation count counter: %w", err)
	}
	o.sessionCount, err = meter.Int64Counter(
		"mcp.server.session.count",
		metric.WithDescription("Count of MCP sessions minted by the proxy."),
	)
	if err != nil {
		return fmt.Errorf("session count counter: %w", err)
	}
	return nil
}

// ToolCallAttrs describes a recorded MCP operation.
type ToolCallAttrs struct {
	Method    string // mcp.method.name, e.g. "tools/call"
	Tool      string // mcp.tool.name
	TenantID  string // resolved tenant, if any
	Backend   string // resolved backend, if any
	ErrorType string // error.type; empty means success
}

// RecordToolCall observes the operation-duration histogram and increments the
// operation counter with the given attributes. durSeconds is the wall time.
func (o *Observability) RecordToolCall(ctx context.Context, a ToolCallAttrs, durSeconds float64) {
	if o == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String(attrMethodName, a.Method)}
	if a.Tool != "" {
		attrs = append(attrs, attribute.String(attrToolName, a.Tool))
	}
	if a.TenantID != "" {
		attrs = append(attrs, attribute.String(attrTenantID, a.TenantID))
	}
	if a.Backend != "" {
		attrs = append(attrs, attribute.String(attrBackend, a.Backend))
	}
	if a.ErrorType != "" {
		attrs = append(attrs, attribute.String(attrErrorType, a.ErrorType))
	}
	set := metric.WithAttributes(attrs...)
	o.opDuration.Record(ctx, durSeconds, set)
	o.opCount.Add(ctx, 1, set)
}

// RecordSession counts a newly minted MCP session.
func (o *Observability) RecordSession(ctx context.Context) {
	if o == nil {
		return
	}
	o.sessionCount.Add(ctx, 1)
}

// Tracer returns the proxy tracer (a no-op tracer when tracing is disabled).
func (o *Observability) Tracer() trace.Tracer {
	if o == nil {
		return noop.NewTracerProvider().Tracer("github.com/loafoe/mt-mcp-proxy")
	}
	return o.tracer
}

// MetricsHandler returns the Prometheus scrape handler.
func (o *Observability) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{})
}

// MetricsListen and MetricsPath expose the configured scrape address/path.
func (o *Observability) MetricsListen() string {
	if o == nil {
		return ""
	}
	return o.metricsListen
}

func (o *Observability) MetricsPath() string {
	if o == nil || o.metricsPath == "" {
		return "/metrics"
	}
	return o.metricsPath
}

// Shutdown flushes and stops the providers. Best-effort; the first error is
// returned.
func (o *Observability) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	var firstErr error
	if o.tracerProvider != nil {
		if err := o.tracerProvider.Shutdown(ctx); err != nil {
			firstErr = err
		}
	}
	if o.meterProvider != nil {
		if err := o.meterProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
