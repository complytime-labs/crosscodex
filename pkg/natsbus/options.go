package natsbus

import (
	"crypto/tls"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Client.
type Option func(*clientOptions)

type clientOptions struct {
	logger         *slog.Logger
	connectTimeout time.Duration
	reconnectWait  time.Duration
	maxReconnects  int
	tlsConfig      *tls.Config
	tracer         trace.Tracer
	meter          metric.Meter
}

func defaultClientOptions() clientOptions {
	return clientOptions{
		logger:         slog.Default(),
		connectTimeout: 5 * time.Second,
		reconnectWait:  2 * time.Second,
		maxReconnects:  60,
	}
}

// WithLogger sets the structured logger for the client.
func WithLogger(logger *slog.Logger) Option {
	return func(o *clientOptions) {
		o.logger = logger
	}
}

// WithConnectTimeout sets the initial connection timeout.
func WithConnectTimeout(d time.Duration) Option {
	return func(o *clientOptions) {
		o.connectTimeout = d
	}
}

// WithReconnectWait sets the wait time between reconnection attempts.
func WithReconnectWait(d time.Duration) Option {
	return func(o *clientOptions) {
		o.reconnectWait = d
	}
}

// WithMaxReconnects sets the maximum number of reconnection attempts.
// Use -1 for unlimited.
func WithMaxReconnects(n int) Option {
	return func(o *clientOptions) {
		o.maxReconnects = n
	}
}

// WithTLSConfig sets the TLS configuration for the NATS connection.
// This is the integration point for pkg/tlsconfig.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *clientOptions) {
		o.tlsConfig = cfg
	}
}

// WithTelemetry configures OpenTelemetry tracing and metrics for the client.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) Option {
	return func(o *clientOptions) {
		o.tracer = tracer
		o.meter = meter
	}
}
