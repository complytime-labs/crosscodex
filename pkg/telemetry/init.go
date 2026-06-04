package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Option configures Init behavior.
type Option func(*options)

type options struct {
	serviceName    string
	serviceVersion string
	resource       *resource.Resource
}

// WithServiceName sets the service name resource attribute.
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// WithServiceVersion sets the service version resource attribute.
func WithServiceVersion(version string) Option {
	return func(o *options) { o.serviceVersion = version }
}

// WithResource overrides automatic resource detection with a custom Resource.
func WithResource(res *resource.Resource) Option {
	return func(o *options) { o.resource = res }
}

// Init creates TracerProvider and MeterProvider with OTLP exporters, registers
// them globally, wraps the default slog handler with trace ID injection, and
// returns a shutdown function.
//
// An empty resolved endpoint disables the signal (no-op provider, no error).
// The returned shutdown function is always non-nil and safe to call.
func Init(ctx context.Context, cfg config.ObservabilityConfig, opts ...Option) (func(context.Context) error, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	traceEndpoint := resolveEndpoint(cfg.Tracing.Endpoint, cfg.Endpoint)
	traceProtocol := resolveProtocol(cfg.Tracing.Protocol, cfg.Protocol)
	metricsEndpoint := resolveEndpoint(cfg.Metrics.Endpoint, cfg.Endpoint)
	metricsProtocol := resolveProtocol(cfg.Metrics.Protocol, cfg.Protocol)

	res, err := buildResource(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("building OTel resource: %w", err)
	}

	sampleRate := cfg.Tracing.SampleRate
	if sampleRate == 0 && traceEndpoint != "" {
		sampleRate = 1.0
	}

	tp, tpShutdown, err := createTracerProvider(ctx, traceEndpoint, traceProtocol, sampleRate, res)
	if err != nil {
		return nil, err
	}

	mp, mpShutdown, err := createMeterProvider(ctx, metricsEndpoint, metricsProtocol, cfg.Metrics.Interval, res)
	if err != nil {
		_ = tpShutdown(ctx)
		return nil, err
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	current := slog.Default().Handler()
	slog.SetDefault(slog.New(newTraceHandler(current)))

	shutdown := func(ctx context.Context) error {
		return errors.Join(tpShutdown(ctx), mpShutdown(ctx))
	}

	return shutdown, nil
}

// validateConfig checks that config values are within acceptable bounds.
func validateConfig(cfg config.ObservabilityConfig) error {
	traceEndpoint := resolveEndpoint(cfg.Tracing.Endpoint, cfg.Endpoint)
	metricsEndpoint := resolveEndpoint(cfg.Metrics.Endpoint, cfg.Endpoint)

	if traceEndpoint != "" {
		if cfg.Tracing.SampleRate < 0 || cfg.Tracing.SampleRate > 1.0 {
			return fmt.Errorf("%w: sample_rate must be between 0.0 and 1.0, got %f", ErrInvalidConfig, cfg.Tracing.SampleRate)
		}

		protocol := resolveProtocol(cfg.Tracing.Protocol, cfg.Protocol)
		if protocol != "grpc" && protocol != "http" {
			return fmt.Errorf("%w: unsupported tracing protocol %q (must be grpc or http)", ErrInvalidConfig, protocol)
		}
	}

	if metricsEndpoint != "" {
		protocol := resolveProtocol(cfg.Metrics.Protocol, cfg.Protocol)
		if protocol != "grpc" && protocol != "http" {
			return fmt.Errorf("%w: unsupported metrics protocol %q (must be grpc or http)", ErrInvalidConfig, protocol)
		}

		if cfg.Metrics.Interval != "" {
			if _, err := time.ParseDuration(cfg.Metrics.Interval); err != nil {
				return fmt.Errorf("%w: invalid metrics interval %q: %v", ErrInvalidConfig, cfg.Metrics.Interval, err)
			}
		}
	}

	return nil
}
