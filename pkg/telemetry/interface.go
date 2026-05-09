package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Provider sets up OpenTelemetry exporters and providers.
//
// Implementations must configure OTLP exporters for traces and metrics.
type Provider interface {
	// TracerProvider returns the trace.TracerProvider for creating tracers.
	TracerProvider() trace.TracerProvider

	// MeterProvider returns the metric.MeterProvider for creating meters.
	MeterProvider() metric.MeterProvider

	// Shutdown flushes pending telemetry and releases resources.
	Shutdown(ctx context.Context) error
}
