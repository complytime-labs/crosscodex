package attestation

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"
	"os"
	"path/filepath"
	"time"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// validateFIPSKey checks that the signing key uses a FIPS-approved algorithm.
func validateFIPSKey(signer crypto.Signer) error {
	pub := signer.Public()
	ecKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("key type %T is not FIPS-approved: %w", pub, ErrNonFIPSAlgorithm)
	}
	curve := ecKey.Curve
	switch curve {
	case elliptic.P256(), elliptic.P384(), elliptic.P521():
		return nil
	default:
		return fmt.Errorf("curve %v is not FIPS-approved: %w", curve.Params().Name, ErrNonFIPSAlgorithm)
	}
}

// startSpan starts a trace span if a tracer is configured. Returns a no-op span if not.
func (g *generator) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if g.tracer != nil {
		return g.tracer.Start(ctx, name, opts...)
	}
	return ctx, trace.SpanFromContext(ctx)
}

// recordMetrics records operation counter and latency. Nil-guarded.
func (g *generator) recordMetrics(ctx context.Context, operation, result string, start time.Time) {
	if g.opCounter != nil {
		g.opCounter.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("operation", operation),
				attribute.String("result", result),
			),
		)
	}
	if g.opLatency != nil {
		g.opLatency.Record(ctx, time.Since(start).Milliseconds(),
			metric.WithAttributes(
				attribute.String("operation", operation),
				attribute.String("result", result),
			),
		)
	}
}

// dumpEnvelopeToBytes serializes an Envelope to JSON bytes via temp file.
// This workaround exists because in_toto.Envelope.Dump only writes to file paths.
func dumpEnvelopeToBytes(env *in_toto.Envelope) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "attestation-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "envelope.json")
	if err := env.Dump(path); err != nil {
		return nil, fmt.Errorf("dump envelope: %w", err)
	}

	return os.ReadFile(path)
}

// loadEnvelopeFromBytes deserializes an Envelope from JSON bytes via temp file.
// This workaround exists because in_toto.LoadMetadata only reads from file paths.
// Note: LoadMetadata cannot parse layout envelopes with canonical JSON newlines;
// use parseDSSEEnvelope + parseLayoutFromDSSE for layout verification instead.
func loadEnvelopeFromBytes(data []byte) (*in_toto.Envelope, error) {
	tmpDir, err := os.MkdirTemp("", "attestation-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "envelope.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, fmt.Errorf("write temp envelope: %w", err)
	}

	metadata, err := in_toto.LoadMetadata(path)
	if err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}

	env, ok := metadata.(*in_toto.Envelope)
	if !ok {
		return nil, fmt.Errorf("metadata is %T, not *in_toto.Envelope", metadata)
	}

	return env, nil
}
