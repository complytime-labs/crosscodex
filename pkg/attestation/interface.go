package attestation

import (
	"context"
	"crypto"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Generator creates and verifies in-toto attestations for supply chain integrity.
type Generator interface {
	// CreateLayout creates a signed supply chain layout.
	CreateLayout(ctx context.Context, opts LayoutOptions) (*SignedLayout, error)

	// CreateLink creates a signed execution record for a pipeline step.
	// The link's ByProducts["trace_id"] is automatically populated from
	// telemetry.TraceIDFromContext(ctx) if a span is active.
	// Additional byproducts can be injected via WithByProducts option.
	CreateLink(ctx context.Context, step string, materials, products []Artifact, opts ...LinkOption) (*SignedLink, error)

	// Verify deserializes and verifies a signed link envelope.
	Verify(ctx context.Context, data []byte) (*VerifiedLink, error)

	// VerifyLayout deserializes and verifies a signed layout envelope.
	// Returns ErrExpired if the layout has expired.
	VerifyLayout(ctx context.Context, data []byte) (*VerifiedLayout, error)

	// VerifyChain verifies one run's whole attestation against its layout,
	// and that it is the attestation of subject, the run the caller expects:
	//   - subject.URI is set, every layout step expects it as a material,
	//     and every link records it as a material with subject.Digest, or,
	//     when subject.Digest is empty, all with the same digest
	//     (ErrSubjectMismatch), so a link or layout from another run is
	//     refused;
	//   - the layout is present (else ErrInvalidLayout), its signature is
	//     valid and it has not expired (VerifyLayout's errors);
	//   - every link's signature is valid (Verify's errors);
	//   - every layout step has exactly one link (ErrMissingLink), and no
	//     link is for an undeclared step or a step already linked
	//     (ErrUnexpectedLink);
	//   - each link records every material and product its layout step
	//     expects, and wherever one step's material is another step's
	//     product, in any order, their digests match (ErrChainBroken).
	// Steps, materials and products are read from the signed envelopes;
	// the other SignedLink fields are ignored and links is not modified.
	// Every error names the step and, for a chain error, the artifact URI.
	// It returns the verified layout and the subject as the links record it.
	VerifyChain(ctx context.Context, layout *SignedLayout, links []*SignedLink, subject Artifact) (*VerifiedChain, error)
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

// WithFIPSMode enables FIPS algorithm enforcement on signing operations.
// When enabled, CreateLayout and CreateLink verify that the signing key
// uses a FIPS-approved algorithm (ECDSA P-256, P-384, P-521).
// Rejects RSA, Ed25519, and non-approved curves with ErrNonFIPSAlgorithm.
//
// FIPS mode is NOT configured via AttestationConfig. It is a deployment-wide
// posture derived from tls.fips.enabled. The service layer must read
// TLSConfig.FIPS.Enabled and pass WithFIPSMode(cfg.TLS.FIPS.Enabled) when
// constructing the Generator. This avoids duplicating the FIPS toggle.
func WithFIPSMode(enabled bool) Option {
	return func(g *generator) error {
		g.fipsMode = enabled
		return nil
	}
}

// WithIncludeByProducts enables automatic byproduct enrichment.
// When true, CreateLink adds span_id, timestamp, and hostname to ByProducts
// in addition to the always-present trace_id.
func WithIncludeByProducts(enabled bool) Option {
	return func(g *generator) error {
		g.includeByProducts = enabled
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
