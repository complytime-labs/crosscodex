package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// resolveEndpoint returns the per-signal endpoint if set, else the shared endpoint.
func resolveEndpoint(signalEndpoint, sharedEndpoint string) string {
	if signalEndpoint != "" {
		return signalEndpoint
	}
	return sharedEndpoint
}

// resolveProtocol returns the per-signal protocol if set, else the shared protocol.
// Falls back to "grpc" when both are empty.
func resolveProtocol(signalProtocol, sharedProtocol string) string {
	if signalProtocol != "" {
		return signalProtocol
	}
	if sharedProtocol != "" {
		return sharedProtocol
	}
	return "grpc"
}

// buildResource constructs the OTel Resource with service metadata.
func buildResource(ctx context.Context, opts *options) (*resource.Resource, error) {
	if opts.resource != nil {
		return opts.resource, nil
	}

	serviceName := opts.serviceName
	if serviceName == "" {
		serviceName = "crosscodex"
	}

	attrs := []resource.Option{
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	}

	if opts.serviceVersion != "" {
		attrs = append(attrs,
			resource.WithAttributes(semconv.ServiceVersion(opts.serviceVersion)),
		)
	}

	return resource.New(ctx, attrs...)
}

// createTracerProvider creates a real or no-op TracerProvider.
func createTracerProvider(ctx context.Context, endpoint, protocol string, sampleRate float64, res *resource.Resource) (trace.TracerProvider, func(context.Context) error, error) {
	if endpoint == "" {
		return nooptrace.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}

	var exporter sdktrace.SpanExporter
	var err error

	switch protocol {
	case "grpc":
		exporter, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		)
	case "http":
		exporter, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpoint(endpoint),
			otlptracehttp.WithInsecure(),
		)
	default:
		return nil, nil, fmt.Errorf("%w: unsupported tracing protocol %q (must be grpc or http)", ErrInvalidConfig, protocol)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRate))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	return tp, tp.Shutdown, nil
}

// createMeterProvider creates a real or no-op MeterProvider.
func createMeterProvider(ctx context.Context, endpoint, protocol, interval string, res *resource.Resource) (metric.MeterProvider, func(context.Context) error, error) {
	if endpoint == "" {
		return noopmetric.NewMeterProvider(), func(context.Context) error { return nil }, nil
	}

	var exporter sdkmetric.Exporter
	var err error

	switch protocol {
	case "grpc":
		exporter, err = otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(endpoint),
			otlpmetricgrpc.WithInsecure(),
		)
	case "http":
		exporter, err = otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpoint(endpoint),
			otlpmetrichttp.WithInsecure(),
		)
	default:
		return nil, nil, fmt.Errorf("%w: unsupported metrics protocol %q (must be grpc or http)", ErrInvalidConfig, protocol)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("creating metric exporter: %w", err)
	}

	readerOpts := []sdkmetric.PeriodicReaderOption{}
	if interval != "" {
		d, parseErr := time.ParseDuration(interval)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("%w: invalid metrics interval %q: %v", ErrInvalidConfig, interval, parseErr)
		}
		readerOpts = append(readerOpts, sdkmetric.WithInterval(d))
	}

	reader := sdkmetric.NewPeriodicReader(exporter, readerOpts...)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	return mp, mp.Shutdown, nil
}
