package telemetrytest

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestProvider is an in-memory OpenTelemetry provider for test assertions.
type TestProvider struct {
	tp           *sdktrace.TracerProvider
	mp           *sdkmetric.MeterProvider
	spanExporter *tracetest.InMemoryExporter
	metricReader *sdkmetric.ManualReader
	mu           sync.Mutex
}

// NewTestProvider creates a TestProvider with in-memory exporters.
func NewTestProvider() (*TestProvider, error) {
	spanExporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(spanExporter),
	)

	metricReader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricReader),
	)

	return &TestProvider{
		tp:           tp,
		mp:           mp,
		spanExporter: spanExporter,
		metricReader: metricReader,
	}, nil
}

// TracerProvider returns the test TracerProvider.
func (p *TestProvider) TracerProvider() trace.TracerProvider {
	return p.tp
}

// MeterProvider returns the test MeterProvider.
func (p *TestProvider) MeterProvider() metric.MeterProvider {
	return p.mp
}

// Shutdown flushes and shuts down both providers.
func (p *TestProvider) Shutdown(ctx context.Context) error {
	return errors.Join(p.tp.Shutdown(ctx), p.mp.Shutdown(ctx))
}

// GetSpans returns all captured spans.
func (p *TestProvider) GetSpans() []sdktrace.ReadOnlySpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	stubs := p.spanExporter.GetSpans()
	spans := make([]sdktrace.ReadOnlySpan, len(stubs))
	for i := range stubs {
		spans[i] = stubs[i].Snapshot()
	}
	return spans
}

// GetMetrics collects and returns the current metric data.
func (p *TestProvider) GetMetrics() metricdata.ResourceMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	var rm metricdata.ResourceMetrics
	_ = p.metricReader.Collect(context.Background(), &rm)
	return rm
}

// Reset clears all captured spans and metrics.
func (p *TestProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spanExporter.Reset()
}
