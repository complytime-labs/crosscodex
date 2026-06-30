package telemetrytest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// buildTestResource constructs the OTel Resource for test providers,
// matching the production buildResource pattern: service.name, host,
// process, and telemetry SDK metadata.
func buildTestResource(serviceName string) (*resource.Resource, error) {
	return resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	)
}

// TestProvider is an in-memory OpenTelemetry provider for test assertions.
// When the TEST_TRACE_DIR environment variable is set, spans are also
// written as OTLP-style JSON to a file in that directory for visual
// validation. The file is named otlp-traces-<pid>-<timestamp>.jsonl to
// avoid collisions when running parallel test suites.
type TestProvider struct {
	tp           *sdktrace.TracerProvider
	mp           *sdkmetric.MeterProvider
	spanExporter *tracetest.InMemoryExporter
	metricReader *sdkmetric.ManualReader
	traceFile    io.Closer // non-nil when file export is active
	mu           sync.Mutex
}

// TestProviderOption configures NewTestProvider behavior.
type TestProviderOption func(*testProviderOptions)

type testProviderOptions struct {
	serviceName string
}

// WithServiceName sets the service.name resource attribute on the
// TracerProvider. This controls how spans appear in backends like
// Jaeger. Defaults to "crosscodex-test" if not specified.
func WithServiceName(name string) TestProviderOption {
	return func(o *testProviderOptions) { o.serviceName = name }
}

// NewTestProvider creates a TestProvider with in-memory exporters.
// If TEST_TRACE_DIR is set, spans are additionally written to a JSON
// file in that directory for offline inspection.
func NewTestProvider(opts ...TestProviderOption) (*TestProvider, error) {
	o := &testProviderOptions{serviceName: "crosscodex-test"}
	for _, opt := range opts {
		opt(o)
	}

	res, err := buildTestResource(o.serviceName)
	if err != nil {
		return nil, fmt.Errorf("build test resource: %w", err)
	}

	spanExporter := tracetest.NewInMemoryExporter()

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSyncer(spanExporter),
		sdktrace.WithResource(res),
	}

	var traceFile io.Closer
	if dir := os.Getenv("TEST_TRACE_DIR"); dir != "" {
		f, fileExp, err := newFileExporter(dir)
		if err != nil {
			return nil, fmt.Errorf("trace file export: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithSyncer(fileExp))
		traceFile = f
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)

	metricReader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricReader),
	)

	return &TestProvider{
		tp:           tp,
		mp:           mp,
		spanExporter: spanExporter,
		metricReader: metricReader,
		traceFile:    traceFile,
	}, nil
}

// newFileExporter creates a file-backed OTLP JSON exporter.
// Returns the exporter (implements io.Closer for Shutdown) and the
// SpanExporter interface.
func newFileExporter(dir string) (io.Closer, sdktrace.SpanExporter, error) {
	exp, err := NewOTLPFileExporter(dir)
	if err != nil {
		return nil, nil, err
	}
	return exp, exp, nil
}

// TracerProvider returns the test TracerProvider.
func (p *TestProvider) TracerProvider() trace.TracerProvider {
	return p.tp
}

// MeterProvider returns the test MeterProvider.
func (p *TestProvider) MeterProvider() metric.MeterProvider {
	return p.mp
}

// Shutdown flushes and shuts down both providers. If file-based trace
// export is active, the trace file is closed after flushing.
func (p *TestProvider) Shutdown(ctx context.Context) error {
	errs := errors.Join(p.tp.Shutdown(ctx), p.mp.Shutdown(ctx))
	if p.traceFile != nil {
		// Safe: OTLPFileExporter.Shutdown is idempotent — tp.Shutdown
		// already called it via the registered exporter, and Close
		// delegates to Shutdown with a done-flag guard.
		errs = errors.Join(errs, p.traceFile.Close())
	}
	return errs
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
