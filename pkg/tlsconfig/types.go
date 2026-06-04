package tlsconfig

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Resolver builds *tls.Config values from the application config.
// A zero-value Resolver is ready to use (telemetry is optional).
type Resolver struct {
	Tracer trace.Tracer
	Meter  metric.Meter
}

// FIPSStatus reports whether the binary was built with BoringCrypto.
type FIPSStatus struct {
	Enabled  bool   // Whether BoringCrypto is linked
	Provider string // "BoringCrypto" or ""
}
