package attestation

import (
	"context"
	"crypto"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Generator creates and verifies in-toto attestations.
type Generator interface {
	// CreateLayout creates a signed supply chain layout.
	CreateLayout(ctx context.Context, opts LayoutOptions) (*SignedLayout, error)

	// CreateLink creates a signed execution record for a pipeline step.
	// The link's ByProducts["trace_id"] is automatically populated from
	// telemetry.TraceIDFromContext(ctx) if a span is active.
	CreateLink(ctx context.Context, step string, materials, products []Artifact) (*SignedLink, error)

	// Verify deserializes and verifies a signed envelope.
	// Returns the verified link payload on success.
	Verify(ctx context.Context, data []byte) (*VerifiedLink, error)
}

// KeyProvider abstracts signing/verification key retrieval.
type KeyProvider interface {
	// SigningKey returns the private key for signing attestations.
	SigningKey(ctx context.Context) (crypto.Signer, error)

	// VerificationKey returns the public key for signature verification.
	VerificationKey(ctx context.Context) (crypto.PublicKey, error)

	// KeyID returns the key identifier used in attestation metadata.
	KeyID(ctx context.Context) (string, error)
}

// Option configures a Generator.
type Option func(*generator) error

// WithTelemetry enables OpenTelemetry tracing and metrics on the Generator.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) Option {
	return func(g *generator) error {
		g.tracer = tracer
		g.meter = meter

		var err error
		g.opCounter, err = meter.Int64Counter("attestation.operations.total")
		if err != nil {
			slog.Warn("attestation: failed to create operations counter", "error", err)
		}
		g.opLatency, err = meter.Int64Histogram("attestation.operation.duration_ms")
		if err != nil {
			slog.Warn("attestation: failed to create operation latency histogram", "error", err)
		}
		return nil
	}
}

// NewGenerator creates a Generator backed by in-toto-golang.
// Returns an error if kp is nil.
func NewGenerator(kp KeyProvider, opts ...Option) (Generator, error) {
	if kp == nil {
		return nil, fmt.Errorf("key provider is required")
	}

	g := &generator{
		keyProvider: kp,
	}

	for _, opt := range opts {
		if err := opt(g); err != nil {
			return nil, fmt.Errorf("apply attestation option: %w", err)
		}
	}

	return g, nil
}
