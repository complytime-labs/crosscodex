package attestation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
		// The signer writes its key ID into each DSSE signature; with no
		// signature naming the trusted key, the link was signed by another
		// key rather than tampered with (see ErrUntrustedKey for why an
		// unsigned keyid is safe to rely on here).
		sigs := env.Sigs()
		if !slices.ContainsFunc(sigs, func(s in_toto.Signature) bool { return s.KeyID == keyID }) {
			keyIDs := make([]string, len(sigs))
			for i, s := range sigs {
				keyIDs[i] = s.KeyID
			}
			return nil, newUntrustedKeyError(keyIDs, err)
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

	// Verify signature at the DSSE level. Only the signatures that verified
	// name a signer: an envelope may carry other, bogus entries.
	keyIDs, err := verifyDSSESignature(ctx, dsseEnv, verKey)
	if err != nil {
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

// VerifyChain verifies one run's whole attestation; see the Generator
// interface for the checks it makes.
func (g *generator) VerifyChain(ctx context.Context, layout *SignedLayout, links []*SignedLink, subject Artifact) (*VerifiedChain, error) {
	operation := "VerifyChain"
	start := time.Now()
	ctx, span := g.startSpan(ctx, "attestation.VerifyChain")
	defer span.End()

	fail := func(err error) (*VerifiedChain, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		g.recordMetrics(ctx, operation, "failure", start)
		return nil, err
	}

	if subject.URI == "" {
		return fail(fmt.Errorf("verify chain: no subject: name the run the attestation must be for: %w", ErrSubjectMismatch))
	}
	if layout == nil || len(layout.Raw) == 0 {
		return fail(fmt.Errorf("verify chain: no layout to verify the links against: %w", ErrInvalidLayout))
	}
	verifiedLayout, err := g.VerifyLayout(ctx, layout.Raw)
	if err != nil {
		return fail(fmt.Errorf("verify chain: layout: %w", err))
	}
	declared := make(map[string]bool, len(verifiedLayout.Steps))
	for _, step := range verifiedLayout.Steps {
		declared[step.Name] = true
	}

	// Steps, materials and products come from the verified envelopes only;
	// the SignedLink fields are the caller's and are not trusted.
	byStep := make(map[string]*VerifiedLink, len(links))
	for i, link := range links {
		label := fmt.Sprintf("link %d", i)
		if link != nil && link.Step != "" {
			label = fmt.Sprintf("link %d (step %q)", i, link.Step)
		} else if link != nil {
			if claimed := claimedStep(link.Raw); claimed != "" {
				label = fmt.Sprintf("link %d (unverified step %q)", i, claimed)
			}
		}
		if link == nil {
			return fail(fmt.Errorf("verify chain: %s is nil: %w", label, ErrVerificationFailed))
		}
		verified, err := g.Verify(ctx, link.Raw)
		if err != nil {
			return fail(fmt.Errorf("verify chain: %s: %w", label, err))
		}
		if !declared[verified.Step] {
			return fail(fmt.Errorf("verify chain: %s is signed for step %q, which the layout does not declare: %w",
				label, verified.Step, ErrUnexpectedLink))
		}
		if _, dup := byStep[verified.Step]; dup {
			return fail(fmt.Errorf("verify chain: more than one link for step %q: %w", verified.Step, ErrUnexpectedLink))
		}
		byStep[verified.Step] = verified
	}

	type producer struct{ step, digest string }
	producedBy := make(map[string]producer)
	for _, step := range verifiedLayout.Steps {
		verified, ok := byStep[step.Name]
		if !ok {
			return fail(fmt.Errorf("verify chain: layout step %q has no link: %w", step.Name, ErrMissingLink))
		}
		for _, p := range verified.Products {
			if _, seen := producedBy[p.URI]; !seen {
				producedBy[p.URI] = producer{step: step.Name, digest: p.Digest}
			}
		}
	}

	recorded := subject.Digest
	for _, step := range verifiedLayout.Steps {
		verified := byStep[step.Name]
		if !slices.Contains(step.ExpectedMaterials, subject.URI) {
			return fail(fmt.Errorf("verify chain: layout step %q does not name the run %q: the layout is for another run: %w",
				step.Name, subject.URI, ErrSubjectMismatch))
		}
		got, ok := artifactDigest(verified.Materials, subject.URI)
		if !ok {
			return fail(fmt.Errorf("verify chain: the link for step %q does not record the run %q: it is from another run: %w",
				step.Name, subject.URI, ErrSubjectMismatch))
		}
		if recorded == "" {
			recorded = got
		}
		if got != recorded {
			return fail(fmt.Errorf("verify chain: the link for step %q records the run %q with digest %q, not %q: %w",
				step.Name, subject.URI, got, recorded, ErrSubjectMismatch))
		}
		if uri, ok := firstMissing(step.ExpectedMaterials, verified.Materials); !ok {
			return fail(fmt.Errorf("verify chain: the link for step %q does not record its expected material %q: %w",
				step.Name, uri, ErrChainBroken))
		}
		if uri, ok := firstMissing(step.ExpectedProducts, verified.Products); !ok {
			return fail(fmt.Errorf("verify chain: the link for step %q does not record its expected product %q: %w",
				step.Name, uri, ErrChainBroken))
		}
		for _, m := range verified.Materials {
			p, shared := producedBy[m.URI]
			if shared && p.step != step.Name && p.digest != m.Digest {
				return fail(fmt.Errorf("verify chain: chain broken between step %q and step %q: artifact %q digest mismatch: %w",
					p.step, step.Name, m.URI, ErrChainBroken))
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	g.recordMetrics(ctx, operation, "success", start)
	return &VerifiedChain{Layout: verifiedLayout, Subject: Artifact{URI: subject.URI, Digest: recorded}}, nil
}

// artifactDigest returns the digest artifacts record for uri.
func artifactDigest(artifacts []Artifact, uri string) (string, bool) {
	for _, a := range artifacts {
		if a.URI == uri {
			return a.Digest, true
		}
	}
	return "", false
}

// claimedStep returns the step name a link envelope's payload states,
// without verifying it, or "" if it cannot be read. It only labels errors.
func claimedStep(raw []byte) string {
	env, err := parseDSSEEnvelope(raw)
	if err != nil {
		return ""
	}
	payload, err := env.DecodeB64Payload()
	if err != nil {
		return ""
	}
	var link struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(fixCanonicalJSONNewlines(payload), &link); err != nil {
		return ""
	}
	return link.Name
}

// firstMissing returns the first URI in want that no artifact in have
// records, and false; or "" and true when have records them all.
func firstMissing(want []string, have []Artifact) (string, bool) {
	recorded := make(map[string]bool, len(have))
	for _, a := range have {
		recorded[a.URI] = true
	}
	for _, uri := range want {
		if !recorded[uri] {
			return uri, false
		}
	}
	return "", true
}
