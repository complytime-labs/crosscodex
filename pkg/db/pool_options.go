package db

import (
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Option func(*poolOptions)

type poolOptions struct {
	maxIdleConns int
	connMaxLife  time.Duration
	tracer       trace.Tracer
	meter        metric.Meter
}

func defaultOptions() poolOptions {
	return poolOptions{
		maxIdleConns: 5,
		connMaxLife:  30 * time.Minute,
	}
}

func WithMaxIdleConns(n int) Option {
	return func(o *poolOptions) {
		o.maxIdleConns = n
	}
}

func WithConnMaxLifetime(d time.Duration) Option {
	return func(o *poolOptions) {
		o.connMaxLife = d
	}
}

// WithTelemetry configures OpenTelemetry tracing and metrics for the pool.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) Option {
	return func(o *poolOptions) {
		o.tracer = tracer
		o.meter = meter
	}
}
