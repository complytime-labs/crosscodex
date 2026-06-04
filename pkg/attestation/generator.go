package attestation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

type generator struct {
	keyProvider KeyProvider
	tracer      trace.Tracer
	meter       metric.Meter
	opCounter   metric.Int64Counter
	opLatency   metric.Int64Histogram
}

// CreateLayout creates a signed in-toto layout envelope.
func (g *generator) CreateLayout(ctx context.Context, opts LayoutOptions) (*SignedLayout, error) {
	start := time.Now()
	operation := "CreateLayout"

	if g.tracer != nil {
		var span trace.Span
		ctx, span = g.tracer.Start(ctx, "attestation.CreateLayout",
			trace.WithAttributes(
				attribute.Int("attestation.step_count", len(opts.Steps)),
				attribute.String("attestation.expires_in", opts.ExpiresIn.String()),
			),
		)
		defer span.End()
	}

	if len(opts.Steps) == 0 {
		err := fmt.Errorf("layout requires at least one step: %w", ErrInvalidLayout)
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, err
	}

	signer, err := g.keyProvider.SigningKey(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("load signing key: %w", err)
	}

	keyID, err := g.keyProvider.KeyID(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("get key ID: %w", err)
	}

	// Public key for the layout's Keys map
	itoKey, err := signerToInTotoKey(signer, keyID)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("convert signing key: %w", err)
	}

	// Signing key (includes private material) for envelope signing
	signingKey, err := signerToInTotoSigningKey(signer, keyID)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("convert signing key for envelope: %w", err)
	}

	expiresTime := time.Now().Add(opts.ExpiresIn)

	layout := in_toto.Layout{
		Type:    "layout",
		Steps:   stepsToInToto(opts.Steps),
		Inspect: inspectionsToInToto(opts.Inspections),
		Keys:    map[string]in_toto.Key{keyID: itoKey},
		Expires: expiresTime.Format(in_toto.ISO8601DateSchema),
	}

	var env in_toto.Envelope
	if err := env.SetPayload(layout); err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("set layout payload: %w: %w", ErrSignatureFailed, err)
	}
	if err := env.Sign(signingKey); err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("sign layout envelope: %w: %w", ErrSignatureFailed, err)
	}

	raw, err := dumpEnvelopeToBytes(&env)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("serialize layout envelope: %w: %w", ErrSignatureFailed, err)
	}

	g.recordMetrics(ctx, operation, "success", start)
	return &SignedLayout{Raw: raw, Expires: expiresTime}, nil
}

// CreateLink creates a signed in-toto link envelope with trace correlation.
func (g *generator) CreateLink(ctx context.Context, step string, materials, products []Artifact) (*SignedLink, error) {
	start := time.Now()
	operation := "CreateLink"

	if g.tracer != nil {
		var span trace.Span
		ctx, span = g.tracer.Start(ctx, "attestation.CreateLink",
			trace.WithAttributes(attribute.String("attestation.step", step)),
		)
		defer span.End()
	}

	signer, err := g.keyProvider.SigningKey(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("load signing key: %w: %w", ErrKeyLoadFailed, err)
	}

	keyID, err := g.keyProvider.KeyID(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("get key ID: %w", err)
	}

	signingKey, err := signerToInTotoSigningKey(signer, keyID)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("convert signing key: %w", err)
	}

	traceID := telemetry.TraceIDFromContext(ctx)

	link := in_toto.Link{
		Type:       "link",
		Name:       step,
		Materials:  artifactsToHashObj(materials),
		Products:   artifactsToHashObj(products),
		ByProducts: map[string]any{"trace_id": traceID},
		Command:    []string{},
	}

	var env in_toto.Envelope
	if err := env.SetPayload(link); err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("set link payload: %w: %w", ErrSignatureFailed, err)
	}
	if err := env.Sign(signingKey); err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("sign link envelope: %w: %w", ErrSignatureFailed, err)
	}

	raw, err := dumpEnvelopeToBytes(&env)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("serialize link envelope: %w: %w", ErrSignatureFailed, err)
	}

	g.recordMetrics(ctx, operation, "success", start)
	return &SignedLink{
		Raw:       raw,
		Step:      step,
		TraceID:   traceID,
		Materials: materials,
		Products:  products,
	}, nil
}

// Verify deserializes and verifies a signed link envelope.
func (g *generator) Verify(ctx context.Context, data []byte) (*VerifiedLink, error) {
	start := time.Now()
	operation := "Verify"

	var span trace.Span
	if g.tracer != nil {
		ctx, span = g.tracer.Start(ctx, "attestation.Verify")
		defer span.End()
	}

	pubKey, err := g.keyProvider.VerificationKey(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("load verification key: %w", err)
	}

	keyID, err := g.keyProvider.KeyID(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("get key ID: %w", err)
	}

	env, err := loadEnvelopeFromBytes(data)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("unmarshal envelope: %w: %w", ErrVerificationFailed, err)
	}

	verKey, err := pubKeyToInTotoKey(pubKey, keyID)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("convert verification key: %w: %w", ErrVerificationFailed, err)
	}

	if err := env.VerifySignature(verKey); err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		if span != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return nil, fmt.Errorf("verify signature: %w: %w", ErrVerificationFailed, err)
	}

	payload := env.GetPayload()

	var link in_toto.Link
	switch v := payload.(type) {
	case *in_toto.Link:
		link = *v
	case in_toto.Link:
		link = v
	default:
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("parse link payload: %w: expected in_toto.Link, got %T", ErrVerificationFailed, payload)
	}

	g.recordMetrics(ctx, operation, "success", start)
	return &VerifiedLink{
		Step:       link.Name,
		Materials:  hashObjToArtifacts(link.Materials),
		Products:   hashObjToArtifacts(link.Products),
		ByProducts: link.ByProducts,
	}, nil
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
