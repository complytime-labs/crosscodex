package attestation

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

type generator struct {
	keyProvider       KeyProvider
	tracer            trace.Tracer
	meter             metric.Meter
	opCounter         metric.Int64Counter
	opLatency         metric.Int64Histogram
	fipsMode          bool
	includeByProducts bool
}

// CreateLayout creates a signed in-toto layout envelope.
func (g *generator) CreateLayout(ctx context.Context, opts LayoutOptions) (*SignedLayout, error) {
	start := time.Now()
	operation := "CreateLayout"
	ctx, span := g.startSpan(ctx, "attestation.CreateLayout",
		trace.WithAttributes(
			attribute.Int("attestation.step_count", len(opts.Steps)),
			attribute.String("attestation.expires_in", opts.ExpiresIn.String()),
		),
	)
	defer span.End()

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

	if g.fipsMode {
		if fipsErr := validateFIPSKey(signer); fipsErr != nil {
			span.RecordError(fipsErr)
			span.SetStatus(codes.Error, fipsErr.Error())
			g.recordMetrics(ctx, operation, "error", start)
			return nil, fipsErr
		}
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

	expiresTime := time.Now().UTC().Add(opts.ExpiresIn)

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
func (g *generator) CreateLink(ctx context.Context, step string, materials, products []Artifact, opts ...LinkOption) (*SignedLink, error) {
	start := time.Now()
	operation := "CreateLink"
	ctx, span := g.startSpan(ctx, "attestation.CreateLink",
		trace.WithAttributes(attribute.String("attestation.step", step)),
	)
	defer span.End()

	var lo linkOptions
	for _, opt := range opts {
		opt(&lo)
	}

	signer, err := g.keyProvider.SigningKey(ctx)
	if err != nil {
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("load signing key: %w: %w", ErrKeyLoadFailed, err)
	}

	if g.fipsMode {
		if fipsErr := validateFIPSKey(signer); fipsErr != nil {
			span.RecordError(fipsErr)
			span.SetStatus(codes.Error, fipsErr.Error())
			g.recordMetrics(ctx, operation, "error", start)
			return nil, fipsErr
		}
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

	// Build byproducts map — trace_id is always present.
	byProducts := map[string]any{
		"trace_id": traceID,
	}

	if g.includeByProducts {
		byProducts["span_id"] = telemetry.SpanIDFromContext(ctx)
		byProducts["timestamp"] = time.Now().UTC().Format(time.RFC3339)
		hostname, _ := os.Hostname()
		byProducts["hostname"] = hostname
	}

	// Merge caller-supplied byproducts, but protect reserved keys.
	if lo.extraByProducts != nil {
		for k, v := range lo.extraByProducts {
			if k == "trace_id" || k == "span_id" {
				continue // reserved
			}
			byProducts[k] = v
		}
	}

	link := in_toto.Link{
		Type:       "link",
		Name:       step,
		Materials:  artifactsToHashObj(materials),
		Products:   artifactsToHashObj(products),
		ByProducts: byProducts,
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
	ctx, span := g.startSpan(ctx, "attestation.Verify")
	defer span.End()

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
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
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

// VerifyLayout deserializes and verifies a signed layout envelope.
// Returns ErrExpired if the layout has expired.
//
// Layout verification operates at the DSSE level rather than using
// in_toto.LoadMetadata, because canonical JSON (cjson.EncodeCanonical)
// may produce raw newlines in PEM-encoded key values, which the in-toto
// library's LoadMetadata cannot parse back.
func (g *generator) VerifyLayout(ctx context.Context, data []byte) (*VerifiedLayout, error) {
	operation := "VerifyLayout"
	start := time.Now()
	ctx, span := g.startSpan(ctx, "attestation.VerifyLayout")
	defer span.End()

	// Parse as DSSE envelope (does not decode the payload).
	dsseEnv, err := parseDSSEEnvelope(data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("unmarshal layout envelope: %w: %w", ErrVerificationFailed, err)
	}

	// Get verification key.
	pubKey, err := g.keyProvider.VerificationKey(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("get verification key: %w", err)
	}

	keyID, err := g.keyProvider.KeyID(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("get key ID: %w", err)
	}

	verKey, err := pubKeyToInTotoKey(pubKey, keyID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("convert verification key: %w: %w", ErrVerificationFailed, err)
	}

	// Verify signature at the DSSE level.
	if err := verifyDSSESignature(ctx, dsseEnv, verKey); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("verify layout signature: %w: %w", ErrVerificationFailed, err)
	}

	// Parse the layout from the DSSE payload (handles canonical JSON newlines).
	layout, err := parseLayoutFromDSSE(dsseEnv)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("parse layout payload: %w: %w", ErrVerificationFailed, err)
	}

	// Check expiry.
	expires, err := time.Parse(in_toto.ISO8601DateSchema, layout.Expires)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, fmt.Errorf("parse layout expiry: %w: %w", ErrVerificationFailed, err)
	}
	if time.Now().After(expires) {
		err := fmt.Errorf("layout expired at %s: %w", expires.Format(time.RFC3339), ErrExpired)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, err
	}

	// Extract key IDs from signatures.
	var keyIDs []string
	for _, sig := range dsseEnv.Signatures {
		keyIDs = append(keyIDs, sig.KeyID)
	}

	result := &VerifiedLayout{
		Steps:       inTotoStepsToSteps(layout.Steps),
		Inspections: inTotoInspectionsToInspections(layout.Inspect),
		Expires:     expires,
		KeyIDs:      keyIDs,
	}

	span.SetStatus(codes.Ok, "")
	g.recordMetrics(ctx, operation, "success", start)
	return result, nil
}

// VerifyChain validates artifact integrity across consecutive pipeline steps.
func (g *generator) VerifyChain(ctx context.Context, layout *SignedLayout, links []*SignedLink) error {
	operation := "VerifyChain"
	start := time.Now()
	ctx, span := g.startSpan(ctx, "attestation.VerifyChain")
	defer span.End()

	if len(links) <= 1 {
		span.SetStatus(codes.Ok, "")
		g.recordMetrics(ctx, operation, "success", start)
		return nil
	}

	// If layout is provided, sort links by layout step order.
	if layout != nil {
		stepOrder, err := g.extractStepOrder(layout)
		if err == nil && len(stepOrder) > 0 {
			orderMap := make(map[string]int, len(stepOrder))
			for i, name := range stepOrder {
				orderMap[name] = i
			}
			sort.Slice(links, func(i, j int) bool {
				oi, oki := orderMap[links[i].Step]
				oj, okj := orderMap[links[j].Step]
				if !oki {
					oi = len(stepOrder)
				}
				if !okj {
					oj = len(stepOrder)
				}
				return oi < oj
			})
		}
	}

	// Verify consecutive pairs.
	for i := 0; i < len(links)-1; i++ {
		stepN := links[i]
		stepN1 := links[i+1]

		// Build product map for step N.
		productDigests := make(map[string]string, len(stepN.Products))
		for _, p := range stepN.Products {
			productDigests[p.URI] = p.Digest
		}

		// Check shared URIs.
		for _, m := range stepN1.Materials {
			if productDigest, shared := productDigests[m.URI]; shared {
				if productDigest != m.Digest {
					err := fmt.Errorf("chain broken between step %q and step %q: artifact %q digest mismatch: %w",
						stepN.Step, stepN1.Step, m.URI, ErrChainBroken)
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
					g.recordMetrics(ctx, operation, "failure", start)
					return err
				}
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	g.recordMetrics(ctx, operation, "success", start)
	return nil
}

// extractStepOrder extracts step names from a signed layout in order.
func (g *generator) extractStepOrder(layout *SignedLayout) ([]string, error) {
	dsseEnv, err := parseDSSEEnvelope(layout.Raw)
	if err != nil {
		return nil, err
	}

	l, err := parseLayoutFromDSSE(dsseEnv)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(l.Steps))
	for i, s := range l.Steps {
		names[i] = s.Name
	}
	return names, nil
}
