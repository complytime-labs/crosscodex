package telemetrytest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

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

// NewTestProvider creates a TestProvider with in-memory exporters.
// If TEST_TRACE_DIR is set, spans are additionally written to a JSON
// file in that directory for offline inspection.
func NewTestProvider() (*TestProvider, error) {
	spanExporter := tracetest.NewInMemoryExporter()

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSyncer(spanExporter),
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

// newFileExporter creates a file-backed stdouttrace exporter.
// Returns the file (for closing) and the exporter.
func newFileExporter(dir string) (*os.File, sdktrace.SpanExporter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, fmt.Errorf("create trace dir %q: %w", dir, err)
	}
	path := fmt.Sprintf("%s/otlp-traces-%d-%d.jsonl", dir, os.Getpid(), time.Now().UnixMilli())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("create trace file %q: %w", path, err)
	}
	exp, err := stdouttrace.New(
		stdouttrace.WithWriter(f),
		stdouttrace.WithPrettyPrint(),
	)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("create stdout trace exporter: %w", err)
	}
	return f, exp, nil
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
